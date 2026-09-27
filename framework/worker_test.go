package framework

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/jobs"
)

func TestParseWorkerFlags(t *testing.T) {
	opts, err := ParseWorkerFlags([]string{"--queue", "critical,mail", "--queue=default", "--concurrency", "8", "--lease", "2m", "--shutdown-timeout", "45s", "--metrics-addr", ":9091"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if opts.MetricsAddr != ":9091" {
		t.Fatalf("MetricsAddr = %q", opts.MetricsAddr)
	}
	if strings.Join(opts.Queues, ",") != "critical,mail,default" || opts.Concurrency != 8 || opts.Lease != 2*time.Minute || opts.ShutdownTimeout != 45*time.Second {
		t.Fatalf("opts = %+v", opts)
	}
	defaults, err := ParseWorkerFlags(nil, nil)
	if err != nil || len(defaults.Queues) != 0 || defaults.Concurrency != 1 || defaults.Lease != jobs.DefaultLease || defaults.ShutdownTimeout != jobs.DefaultShutdownTimeout {
		t.Fatalf("defaults = %+v, %v", defaults, err)
	}
	for _, args := range [][]string{
		{"--queue", "Bad Queue"},
		{"--concurrency", "0"},
		{"--lease", "-1s"},
		{"--shutdown-timeout", "0s"},
		{"extra"},
		{"--nope"},
	} {
		if _, err := ParseWorkerFlags(args, nil); err == nil {
			t.Errorf("ParseWorkerFlags(%v) accepted", args)
		}
	}
	var help bytes.Buffer
	if _, err := ParseWorkerFlags([]string{"--help"}, &help); !errors.Is(err, flag.ErrHelp) || !strings.Contains(help.String(), "-concurrency") {
		t.Fatalf("--help = %v, output %q", err, help.String())
	}
	// Everything after SIGTERM: a reserve on the wire and its release, the
	// shutdown timeout, the grace, and the stop hooks.
	want := time.Minute + 2*jobs.QueueCallTimeout + jobs.ShutdownGrace + defaultShutdownTimeout
	if got := WorkerKillAfter([]string{"--shutdown-timeout", "1m"}); got != want {
		t.Fatalf("WorkerKillAfter = %s, want %s", got, want)
	}
}

type workerJob struct {
	N int `json:"n"`
}

func (workerJob) JobName() string { return "worker_job" }

// TestRunWorkerRunsTheAppsJobs runs the worker mode in-process: start hooks,
// the app's registered jobs from its queue, stop hooks.
func TestRunWorkerRunsTheAppsJobs(t *testing.T) {
	cfg := config.Default()
	cfg.Jobs.Driver = config.JobsDriverMemory
	app, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	var started, stopped atomic.Bool
	app.OnStart(func(context.Context) error { started.Store(true); return nil })
	app.OnStop(func(context.Context) error { stopped.Store(true); return nil })
	var ran atomic.Int32
	var requestID atomic.Value
	jobs.MustRegister(app.Jobs().Registry(), func(ctx context.Context, _ workerJob) error {
		requestID.Store(GetRequestIDFromContext(ctx))
		ran.Add(1)
		return nil
	})
	reqCtx := context.WithValue(context.Background(), requestMetaKey{}, &requestMeta{requestID: "req-w"})
	for i := 0; i < 3; i++ {
		if _, err := app.Jobs().Dispatch(reqCtx, workerJob{N: i}); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunWorker(ctx, app, jobs.WorkerOptions{PollInterval: 5 * time.Millisecond}) }()
	deadline := time.Now().Add(5 * time.Second)
	for ran.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunWorker() = %v", err)
	}
	if ran.Load() != 3 || !started.Load() || !stopped.Load() || requestID.Load() != "req-w" {
		t.Fatalf("ran %d, start hook %v, stop hook %v, request %v", ran.Load(), started.Load(), stopped.Load(), requestID.Load())
	}
}

func TestWorkerModeRefusesDriversAWorkerCannotConsume(t *testing.T) {
	for driver, want := range map[config.JobsDriver]string{
		config.JobsDriverSync:   "no queue to work",
		config.JobsDriverMemory: "cannot see it",
	} {
		cfg := config.Default()
		cfg.Jobs.Driver = driver
		app, err := New(WithConfig(cfg))
		if err != nil {
			t.Fatal(err)
		}
		// A regression would start the worker; the deadline turns that into a
		// failure instead of a hang.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err = runWorkerCommand(ctx, app, nil, nil)
		cancel()
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "GOMBIT_JOBS_DRIVER=redis") {
			t.Fatalf("worker mode with %s = %v, want a refusal pointing at redis", driver, err)
		}
	}
	cfg := config.Default()
	app, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if err := runWorkerCommand(context.Background(), app, []string{"--concurrency", "0"}, nil); err == nil {
		t.Fatal("worker mode accepted invalid flags")
	}
	if err := runWorkerCommand(context.Background(), app, []string{"-h"}, nil); err != nil {
		t.Fatalf("worker -h = %v, want help and no error", err)
	}
	if err := RunWorker(context.Background(), app, jobs.WorkerOptions{}); !errors.Is(err, jobs.ErrNoQueue) {
		t.Fatalf("RunWorker(sync) = %v, want ErrNoQueue", err)
	}
}

// TestJobMetricsOnTheAppAndTheWorkerEndpoint: jobs run by an in-process
// worker show on the app's /metrics, and a worker's --metrics-addr endpoint
// adds each queue's depth.
func TestJobMetricsOnTheAppAndTheWorkerEndpoint(t *testing.T) {
	cfg := config.Default()
	cfg.Jobs.Driver = config.JobsDriverMemory
	app, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	var ran atomic.Int32
	jobs.MustRegister(app.Jobs().Registry(), func(context.Context, workerJob) error { ran.Add(1); return nil })
	for i := 0; i < 2; i++ {
		if _, err := app.Jobs().Dispatch(context.Background(), workerJob{N: i}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := app.Jobs().Dispatch(context.Background(), workerJob{N: 9}, jobs.Delay(time.Hour)); err != nil {
		t.Fatal(err)
	}

	addr, stop, err := serveWorkerMetrics("127.0.0.1:0", app, []string{"default"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop() }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunWorker(ctx, app, jobs.WorkerOptions{PollInterval: 5 * time.Millisecond}) }()
	deadline := time.Now().Add(5 * time.Second)
	for ran.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	get := func(url string) string {
		t.Helper()
		resp, err := http.Get(url) // #nosec G107 -- the test's own listener
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	}
	worker := get("http://" + addr + "/metrics")
	for _, want := range []string{
		`gombit_jobs_processed_total{job_name="worker_job",queue="default",result="succeeded"} 2`,
		`gombit_jobs_queued{queue="default",state="scheduled"} 1`,
	} {
		if !strings.Contains(worker, want) {
			t.Errorf("worker /metrics missing %s:\n%s", want, worker)
		}
	}
	if live := get("http://" + addr + "/livez"); !strings.Contains(live, `"ok"`) {
		t.Errorf("worker /livez = %s", live)
	}

	rec := httptest.NewRecorder()
	app.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, want := range []string{
		`gombit_jobs_processed_total{job_name="worker_job",queue="default",result="succeeded"} 2`,
		`gombit_jobs_queued{queue="default",state="scheduled"} 1`,
		`gombit_jobs_queue_stats_up{queue="default"} 1`,
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("app /metrics lacks the in-process worker's %s:\n%s", want, rec.Body.String())
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// statsDownQueue cannot read its stats, as in a Redis outage.
type statsDownQueue struct{ *jobs.MemoryQueue }

func (statsDownQueue) Stats(context.Context, string) (jobs.QueueStats, error) {
	return jobs.QueueStats{}, errors.New("redis: i/o timeout")
}

// TestJobMetricsWhenQueueStatsFail: the worker endpoint fails the scrape, so
// Prometheus keeps the last good sample instead of an empty queue; the app's
// /metrics keeps its HTTP series and says the queue's stats are down.
func TestJobMetricsWhenQueueStatsFail(t *testing.T) {
	dispatcher := jobs.NewDispatcher(jobs.NewRegistry(), statsDownQueue{jobs.NewMemoryQueue()})
	app, err := New(WithConfig(config.Default()), WithJobs(dispatcher))
	if err != nil {
		t.Fatal(err)
	}
	app.addWorkerQueues([]string{"default"})
	addr, stop, err := serveWorkerMetrics("127.0.0.1:0", app, []string{"default"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop() }()
	resp, err := http.Get("http://" + addr + "/metrics") // #nosec G107 -- the test's own listener
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("worker /metrics with unreadable stats = %d, want 503", resp.StatusCode)
	}

	rec := httptest.NewRecorder()
	app.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `gombit_jobs_queue_stats_up{queue="default"} 0`) ||
		strings.Contains(body, "gombit_jobs_queued{") || !strings.Contains(body, "gombit_http_") {
		t.Fatalf("app /metrics with unreadable stats = %d:\n%s", rec.Code, body)
	}
}
