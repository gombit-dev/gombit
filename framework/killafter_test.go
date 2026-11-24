package framework

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

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
	if got := WorkerKillAfterFor(cfg, nil); got != 58*time.Second {
		t.Errorf("WorkerKillAfterFor(defaults) = %s, want 58s", got)
	}
	if got := WorkerKillAfter(nil); got != 58*time.Second {
		t.Errorf("WorkerKillAfter(nil) = %s, want 58s, unchanged", got)
	}
	cfg.Jobs.Worker.ShutdownTimeout = time.Minute
	cfg.Shutdown.Timeout = 15 * time.Second
	// The worker's 1m, 18s for the queue calls and the canceled jobs, and the
	// stop hooks' 15s.
	if got := WorkerKillAfterFor(cfg, nil); got != 93*time.Second {
		t.Errorf("WorkerKillAfterFor(1m worker, 15s hooks) = %s, want 93s", got)
	}
	if got := WorkerKillAfterFor(cfg, []string{"--shutdown-timeout", "5s"}); got != 38*time.Second {
		t.Errorf("WorkerKillAfterFor with --shutdown-timeout 5s = %s, want 38s: the flag wins over the setting", got)
	}
	if got := WorkerKillAfterFor(cfg, []string{"--nope"}); got != 93*time.Second {
		t.Errorf("WorkerKillAfterFor with unparsable args = %s, want the declared 93s", got)
	}
}

// The declared worker settings are the flags' defaults, and a flag
// overrides its own.
func TestParseWorkerFlagsForTakesTheDeclaredSettings(t *testing.T) {
	cfg := config.Default()
	cfg.Jobs.Worker = config.JobsWorkerConfig{
		Queues:          []string{"critical", "default"},
		Concurrency:     4,
		Lease:           2 * time.Minute,
		ShutdownTimeout: 45 * time.Second,
	}
	got, err := ParseWorkerFlagsFor(cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Queues, ",") != "critical,default" || got.Concurrency != 4 || got.Lease != 2*time.Minute || got.ShutdownTimeout != 45*time.Second {
		t.Fatalf("no flags: %+v, want the declared settings", got.WorkerOptions)
	}
	got, err = ParseWorkerFlagsFor(cfg, []string{"--queue", "mail", "--concurrency", "8"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Queues, ",") != "mail" || got.Concurrency != 8 || got.Lease != 2*time.Minute || got.ShutdownTimeout != 45*time.Second {
		t.Fatalf("--queue mail --concurrency 8: %+v, want those two flags over the declared settings", got.WorkerOptions)
	}
	var help bytes.Buffer
	_, _ = ParseWorkerFlagsFor(cfg, []string{"--help"}, &help)
	if !strings.Contains(help.String(), "(default 4)") || !strings.Contains(help.String(), "GOMBIT_JOBS_WORKER_CONCURRENCY") {
		t.Errorf("--help shows no declared default and setting:\n%s", help.String())
	}
	got.Queues[0] = "changed"
	if cfg.Jobs.Worker.Queues[0] != "critical" {
		t.Error("ParseWorkerFlagsFor's queues alias the config's")
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
	if limit := WebKillAfter(cfg) - webShutdownGrace; took > limit+slack {
		t.Errorf("the shutdown took %s, past the %s WebKillAfter allows before its exit grace", took, limit)
	}
}
