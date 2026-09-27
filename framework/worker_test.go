package framework

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/jobs"
)

func TestParseWorkerFlags(t *testing.T) {
	opts, err := ParseWorkerFlags([]string{"--queue", "critical,mail", "--queue=default", "--concurrency", "8", "--lease", "2m", "--shutdown-timeout", "45s"}, nil)
	if err != nil {
		t.Fatal(err)
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
	if got := WorkerKillAfter([]string{"--shutdown-timeout", "1m"}); got != time.Minute+workerShutdownGrace {
		t.Fatalf("WorkerKillAfter = %s", got)
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
