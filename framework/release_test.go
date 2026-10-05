package framework

import (
	"context"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/cache"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/jobs"
)

// janitorIDs returns the IDs of the in-memory cache janitor goroutines alive
// now. A goroutine that has been created but not yet scheduled shows only its
// WithJanitor wrapper frame, so both frames count. Tracking IDs, not a count,
// keeps the tests below immune to unrelated janitors exiting meanwhile.
func janitorIDs() map[string]bool {
	buf := make([]byte, 1<<22)
	ids := map[string]bool{}
	for _, block := range strings.Split(string(buf[:runtime.Stack(buf, true)]), "\n\n") {
		if !strings.Contains(block, "cache.(*Memory).runJanitor") && !strings.Contains(block, "WithJanitor.func1") {
			continue
		}
		if header, _, ok := strings.Cut(block, " ["); ok {
			ids[strings.TrimPrefix(header, "goroutine ")] = true
		}
	}
	return ids
}

// newJanitors waits up to two seconds for every janitor not in before to exit,
// and returns those still running.
func newJanitors(before map[string]bool) []string {
	var leaked []string
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		leaked = leaked[:0]
		for id := range janitorIDs() {
			if !before[id] {
				leaked = append(leaked, id)
			}
		}
		if len(leaked) == 0 || time.Now().After(deadline) {
			return leaked
		}
	}
}

// failingConfig passes Validate but fails late in New, after the cache and the
// job dispatcher are open: auth is on and no database is attached.
func failingConfig() config.Config {
	cfg := config.Default()
	cfg.Environment = config.EnvironmentTest
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.Auth.JWTSecret = strings.Repeat("s", 64)
	return cfg
}

// A failed New never returns the *App, so it must release what it opened
// itself: 20 failures used to leave 20 cache janitors running for the life of
// the process (issue #435).
func TestFailedNewReleasesWhatItOpened(t *testing.T) {
	before := janitorIDs()
	for range 20 {
		if _, err := New(WithConfig(failingConfig())); err == nil || !strings.Contains(err.Error(), "no database is attached") {
			t.Fatalf("New() = %v, want the late auth failure", err)
		}
	}
	if leaked := newJanitors(before); len(leaked) > 0 {
		t.Fatalf("%d cache janitors leaked by 20 failed New calls", len(leaked))
	}
}

// A cache the caller passed in is the caller's: a failed New leaves it open.
func TestFailedNewLeavesTheCallersCacheOpen(t *testing.T) {
	before := janitorIDs()
	callerCfg := config.Default().Cache
	callerCfg.Driver = config.CacheDriverMemory
	store, err := cache.Open(callerCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var mine string
	for id := range janitorIDs() {
		if !before[id] {
			mine = id
		}
	}
	if mine == "" {
		t.Fatal("cache.Open started no janitor to track")
	}

	if _, err := New(WithConfig(failingConfig()), WithCache(store)); err == nil {
		t.Fatal("New() = nil, want the late auth failure")
	}
	// Close waits for the janitor to finish, so a wrongly closed cache's
	// janitor is gone, or about to be, by now.
	time.Sleep(50 * time.Millisecond)
	if !janitorIDs()[mine] {
		t.Fatal("a failed New closed the cache the caller passed in")
	}
}

// RunContext releases the app on every return, including a listen failure and
// a nil context: the stop hooks run once and the app-owned cache and
// dispatcher are closed.
func TestRunContextFailuresReleaseTheApp(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()

	for name, run := range map[string]func(*App) error{
		"listen fails": func(app *App) error { return RunContext(context.Background(), app) },
		"nil context":  func(app *App) error { return RunContext(nil, app) }, //nolint:staticcheck // the nil context is the case under test
	} {
		t.Run(name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Environment = config.EnvironmentTest
			cfg.HTTP.Addr = busy.Addr().String()
			assertReleasedOnce(t, cfg, nil, run, true)
		})
	}
}

// RunWorker and worker mode release the app exactly once on every return:
// before the worker starts (refused options, flags, driver, -h, a metrics
// listener that cannot bind) and after RunWorker ran, where worker mode must
// not release a second time.
func TestWorkerReturnsReleaseTheAppExactlyOnce(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()

	memoryDriver := func() config.Config {
		cfg := config.Default()
		cfg.Environment = config.EnvironmentTest
		cfg.Jobs.Driver = config.JobsDriverMemory
		return cfg
	}
	// A redis-driver config with a memory dispatcher attached passes worker
	// mode's driver check without dialing Redis; a failing start hook then
	// ends RunWorker, which releases the app itself.
	failingStart := func() config.Config {
		cfg := memoryDriver()
		cfg.Jobs.Driver = config.JobsDriverRedis
		return cfg
	}
	cases := map[string]struct {
		cfg         config.Config
		failStart   bool
		run         func(*App) error
		wantSuccess bool
	}{
		"RunWorker refuses its options": {cfg: memoryDriver(), run: func(app *App) error {
			return RunWorker(context.Background(), app, jobs.WorkerOptions{Concurrency: -1})
		}},
		"RunWorker gets a nil context": {cfg: memoryDriver(), run: func(app *App) error {
			return RunWorker(nil, app, jobs.WorkerOptions{}) //nolint:staticcheck // the nil context is the case under test
		}},
		"worker mode refuses the driver": {cfg: memoryDriver(), run: func(app *App) error {
			return runWorkerCommand(context.Background(), app, nil, nil)
		}},
		"worker mode refuses its flags": {cfg: memoryDriver(), run: func(app *App) error {
			return runWorkerCommand(context.Background(), app, []string{"--concurrency", "0"}, nil)
		}},
		"worker mode prints its help": {cfg: memoryDriver(), wantSuccess: true, run: func(app *App) error {
			return runWorkerCommand(context.Background(), app, []string{"-h"}, &strings.Builder{})
		}},
		"worker mode cannot bind its metrics": {cfg: failingStart(), run: func(app *App) error {
			return runWorkerCommand(context.Background(), app, []string{"--metrics-addr", busy.Addr().String()}, nil)
		}},
		"worker mode after RunWorker ran": {cfg: failingStart(), failStart: true, run: func(app *App) error {
			return runWorkerCommand(context.Background(), app, nil, nil)
		}},
		"worker mode with metrics after RunWorker ran": {cfg: failingStart(), failStart: true, run: func(app *App) error {
			return runWorkerCommand(context.Background(), app, []string{"--metrics-addr", "127.0.0.1:0"}, nil)
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var attach []Option
			if tc.cfg.Jobs.Driver == config.JobsDriverRedis {
				dispatcher, err := jobs.Open(memoryDriver().Jobs, tc.cfg.Cache.Redis, jobs.NewRegistry())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = dispatcher.Close() })
				attach = append(attach, WithJobs(dispatcher))
			}
			run := tc.run
			if tc.failStart {
				run = func(app *App) error {
					app.OnStart(func(context.Context) error { return context.Canceled })
					return tc.run(app)
				}
			}
			assertReleasedOnce(t, tc.cfg, attach, run, !tc.wantSuccess)
		})
	}
}

// assertReleasedOnce builds an app from cfg, runs it with run, checks run
// failed (or succeeded, when wantErr is false), and that the stop hooks ran
// exactly once and the app-owned cache and dispatcher are closed.
func assertReleasedOnce(t *testing.T, cfg config.Config, options []Option, run func(*App) error, wantErr bool) {
	t.Helper()
	app, err := New(append([]Option{WithConfig(cfg)}, options...)...)
	if err != nil {
		t.Fatal(err)
	}
	stopped := 0
	app.OnStop(func(context.Context) error { stopped++; return nil })
	if err := run(app); (err != nil) != wantErr {
		t.Fatalf("run = %v, want error: %v", err, wantErr)
	}
	if stopped != 1 {
		t.Errorf("stop hooks ran %d times, want exactly 1", stopped)
	}
	app.mu.RLock()
	defer app.mu.RUnlock()
	if app.cacheOwned || app.jobsOwned {
		t.Error("the app-owned cache and dispatcher must be closed")
	}
}
