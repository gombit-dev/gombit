package framework

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/jobs"
)

// The figures a host reads from the application contract (HOST-4).
func TestWebKillAfter(t *testing.T) {
	cfg := config.Default()
	if got := WebKillAfter(cfg); got != 22*time.Second {
		t.Errorf("WebKillAfter(defaults) = %s, want 22s: 10s for the drain, 10s for the stop hooks, 2s for the exit", got)
	}
	cfg.Shutdown = config.ShutdownConfig{Timeout: 15 * time.Second, DrainDelay: 3 * time.Second}
	if got := WebKillAfter(cfg); got != 35*time.Second {
		t.Errorf("WebKillAfter(15s timeout, 3s drain delay) = %s, want 35s", got)
	}
	cfg.Shutdown = config.ShutdownConfig{}
	if got := WebKillAfter(cfg); got != 22*time.Second {
		t.Errorf("WebKillAfter(zero shutdown settings) = %s, want the defaults' 22s", got)
	}
}

func TestWorkerKillAfterFor(t *testing.T) {
	cfg := config.Default()
	if got := WorkerKillAfterFor(cfg, nil); got != time.Minute {
		t.Errorf("WorkerKillAfterFor(defaults) = %s, want 60s: the 30s shutdown timeout, 18s for the queue calls and the canceled jobs, 10s for the stop hooks, 2s for the exit", got)
	}
	if got := WorkerKillAfter(nil); got != time.Minute {
		t.Errorf("WorkerKillAfter(nil) = %s, want WorkerKillAfterFor's 60s at the defaults", got)
	}
	cfg.Jobs.Worker.ShutdownTimeout = time.Minute
	cfg.Shutdown.Timeout = 15 * time.Second
	// The worker's 1m, 18s for the queue calls and the canceled jobs, the
	// stop hooks' 15s, and the exit's 2s.
	if got := WorkerKillAfterFor(cfg, nil); got != 95*time.Second {
		t.Errorf("WorkerKillAfterFor(1m worker, 15s hooks) = %s, want 95s", got)
	}
	if got := WorkerKillAfterFor(cfg, []string{"--shutdown-timeout", "5s"}); got != 40*time.Second {
		t.Errorf("WorkerKillAfterFor with --shutdown-timeout 5s = %s, want 40s: the flag wins over the setting", got)
	}
	if got := WorkerKillAfterFor(cfg, []string{"--metrics-addr", ":9091"}); got != 100*time.Second {
		t.Errorf("WorkerKillAfterFor with --metrics-addr = %s, want 100s: 5s more for the metrics endpoint to stop", got)
	}
	if got := WorkerKillAfterFor(cfg, []string{"--nope"}); got != 95*time.Second {
		t.Errorf("WorkerKillAfterFor with unparsable args = %s, want the declared 95s", got)
	}
}

// A flag not given is left zero for RunWorker to resolve against the app's
// declared settings, so no caller of ParseWorkerFlags gets gombit's defaults
// in their place; --help names the settings.
func TestParseWorkerFlagsLeavesTheSettingsToTheApp(t *testing.T) {
	got, err := ParseWorkerFlags(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Queues != nil || got.Concurrency != 0 || got.Lease != 0 || got.ShutdownTimeout != 0 {
		t.Fatalf("no flags: %+v, want every option zero", got.WorkerOptions)
	}
	got, err = ParseWorkerFlags([]string{"--queue", "mail", "--concurrency", "8"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Queues, ",") != "mail" || got.Concurrency != 8 || got.Lease != 0 || got.ShutdownTimeout != 0 {
		t.Fatalf("--queue mail --concurrency 8: %+v, want those two and the rest zero", got.WorkerOptions)
	}
	var help bytes.Buffer
	_, _ = ParseWorkerFlags([]string{"--help"}, &help)
	for _, want := range []string{"GOMBIT_JOBS_WORKER_QUEUES", "GOMBIT_JOBS_WORKER_CONCURRENCY, else 1", "GOMBIT_JOBS_WORKER_LEASE, else 5m", "GOMBIT_JOBS_WORKER_SHUTDOWN_TIMEOUT, else 30s"} {
		if !strings.Contains(help.String(), want) {
			t.Errorf("--help lacks %q:\n%s", want, help.String())
		}
	}
	if strings.Contains(help.String(), "(default 0") {
		t.Errorf("--help shows a zero default:\n%s", help.String())
	}
}

// What a host starts, `./server worker` with no flags, and RunWorker with
// zero options run on the app's declared concurrency, lease and shutdown
// timeout: the settings the application contract computes the worker's stop
// timeout from.
func TestTheWorkerRunsOnTheDeclaredSettings(t *testing.T) {
	const shutdown = 300 * time.Millisecond
	declared := config.JobsWorkerConfig{Concurrency: 3, Lease: 2 * time.Minute, ShutdownTimeout: shutdown}
	memoryJobs := func() config.Config {
		cfg := config.Default()
		cfg.Environment = config.EnvironmentTest
		cfg.Jobs.Driver = config.JobsDriverMemory
		cfg.Jobs.Worker = declared
		return cfg
	}
	cases := map[string]struct {
		cfg func() config.Config
		run func(context.Context, *App) error
	}{
		"./server worker": {
			// A redis-driver config passes worker mode's driver check; the
			// memory dispatcher attached below keeps the test off Redis.
			cfg: func() config.Config {
				cfg := memoryJobs()
				cfg.Jobs.Driver = config.JobsDriverRedis
				return cfg
			},
			run: func(ctx context.Context, app *App) error { return runWorkerCommand(ctx, app, nil, io.Discard) },
		},
		"RunWorker": {
			cfg: memoryJobs,
			run: func(ctx context.Context, app *App) error { return RunWorker(ctx, app, jobs.WorkerOptions{}) },
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := tc.cfg()
			core, logs := observer.New(zap.InfoLevel)
			opts := []Option{WithConfig(cfg), WithLogger(zap.New(core))}
			if cfg.Jobs.Driver == config.JobsDriverRedis {
				dispatcher, err := jobs.Open(memoryJobs().Jobs, cfg.Cache.Redis, jobs.NewRegistry())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = dispatcher.Close() })
				opts = append(opts, WithJobs(dispatcher))
			}
			app, err := New(opts...)
			if err != nil {
				t.Fatal(err)
			}
			entered := make(chan struct{})
			canceled := make(chan struct{})
			jobs.MustRegister(app.Jobs().Registry(), func(ctx context.Context, _ workerJob) error {
				close(entered)
				<-ctx.Done()
				close(canceled)
				return ctx.Err()
			})
			if _, err := app.Jobs().Dispatch(context.Background(), workerJob{}); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- tc.run(ctx, app) }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("the worker never ran the job")
			}
			started := logs.FilterMessage("jobs worker started").All()
			if len(started) != 1 {
				t.Fatalf("logged %d worker starts, want 1", len(started))
			}
			fields := started[0].ContextMap()
			if fields["concurrency"] != int64(3) || fields["lease"] != 2*time.Minute {
				t.Errorf("the worker started with concurrency %v and lease %v, want the declared 3 and 2m0s", fields["concurrency"], fields["lease"])
			}

			// The job ignores the stop until its context is canceled, which
			// the declared shutdown timeout does, not gombit's 30s.
			begin := time.Now()
			cancel()
			select {
			case <-canceled:
			case <-time.After(10 * shutdown):
				t.Fatalf("the in-flight job was not canceled within %s: the worker does not use the declared %s shutdown timeout", 10*shutdown, shutdown)
			}
			if took := time.Since(begin); took < shutdown {
				t.Errorf("the in-flight job was canceled after %s, before the declared %s shutdown timeout", took, shutdown)
			}
			if err := <-done; err != nil {
				t.Fatalf("the worker returned %v", err)
			}
		})
	}
}

// RunWorker given no queues consumes the declared ones, not the default
// queue.
func TestRunWorkerConsumesTheDeclaredQueues(t *testing.T) {
	cfg := config.Default()
	cfg.Jobs.Driver = config.JobsDriverMemory
	cfg.Jobs.Worker.Queues = []string{"mail"}
	app, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	var ran atomic.Int32
	jobs.MustRegister(app.Jobs().Registry(), func(context.Context, workerJob) error {
		ran.Add(1)
		return nil
	})
	if _, err := app.Jobs().Dispatch(context.Background(), workerJob{N: 1}, jobs.OnQueue("mail")); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Jobs().Dispatch(context.Background(), workerJob{N: 2}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunWorker(ctx, app, jobs.WorkerOptions{PollInterval: 5 * time.Millisecond}) }()
	deadline := time.Now().Add(5 * time.Second)
	for ran.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // the default queue's job, were it consumed
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunWorker() = %v", err)
	}
	if got := ran.Load(); got != 1 {
		t.Fatalf("ran %d jobs, want 1: the mail queue's, not the default queue's", got)
	}
}

// The configured shutdown settings reach the App, and an Option wins over
// them.
func TestShutdownSettingsFromConfigAndOptions(t *testing.T) {
	cfg := config.Default()
	cfg.Shutdown = config.ShutdownConfig{Timeout: 3 * time.Second, DrainDelay: 2 * time.Second}
	app, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if app.shutdownTimeout != 3*time.Second || app.shutdownDrainDelay != 2*time.Second {
		t.Errorf("from config: timeout %s, drain delay %s; want 3s and 2s", app.shutdownTimeout, app.shutdownDrainDelay)
	}
	app, err = New(WithConfig(cfg), WithShutdownTimeout(time.Second), WithShutdownDrainDelay(0))
	if err != nil {
		t.Fatal(err)
	}
	if app.shutdownTimeout != time.Second || app.shutdownDrainDelay != 0 {
		t.Errorf("with options: timeout %s, drain delay %s; want 1s and 0", app.shutdownTimeout, app.shutdownDrainDelay)
	}
	cfg.Shutdown = config.ShutdownConfig{}
	app, err = New(WithConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if app.shutdownTimeout != defaultShutdownTimeout {
		t.Errorf("zero config: timeout %s, want the default %s", app.shutdownTimeout, defaultShutdownTimeout)
	}
}

// WebKillAfter covers a real shutdown at the configured settings: a request
// that never finishes and stop hooks that wait out their context use every
// budget, and the shutdown must still end before WebKillAfter minus its exit
// grace. A release that adds a shutdown stage, or gives each hook a budget of
// its own (two hooks block here), fails this instead of shipping a figure
// that cuts the shutdown off.
func TestWebKillAfterCoversTheShutdown(t *testing.T) {
	const (
		timeout = 400 * time.Millisecond
		delay   = 300 * time.Millisecond
		// slack is the measurement's allowance: half a budget, so one more
		// budget anywhere in the shutdown is past it.
		slack = timeout / 2
	)
	cfg := config.Default()
	cfg.Environment = config.EnvironmentTest
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.Shutdown = config.ShutdownConfig{Timeout: timeout, DrainDelay: delay}
	app, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	app.Router().GET("/hang", func(c *gin.Context) {
		close(entered)
		<-release
		c.Status(http.StatusOK)
	})
	for range 2 {
		app.OnStop(func(ctx context.Context) error {
			<-ctx.Done()
			return nil
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunContext(ctx, app) }()
	waitForHTTP(t, app, "/livez")
	go func() {
		if resp, err := http.Get("http://" + app.Addr() + "/hang"); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-entered
	begin := time.Now()
	cancel()
	_ = waitRun(done) // the cut-off drain is reported, and expected
	took := time.Since(begin)

	if floor := delay + 2*timeout; took < floor {
		t.Errorf("the shutdown took %s, under the %s its budgets add up to: the test no longer uses them all", took, floor)
	}
	if limit := WebKillAfter(cfg) - shutdownExitGrace; took > limit+slack {
		t.Errorf("the shutdown took %s, past the %s WebKillAfter allows before its exit grace", took, limit)
	}
}
