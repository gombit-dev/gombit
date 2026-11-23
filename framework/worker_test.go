package framework

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
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

func TestRunWorkerInsideTheWebProcessLeavesTheAppLifecycleToTheServer(t *testing.T) {
	cfg := config.Default()
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.Jobs.Driver = config.JobsDriverMemory
	app, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	var starts, stops atomic.Int32
	serverStarted := make(chan struct{})
	app.OnStart(func(context.Context) error { starts.Add(1); close(serverStarted); return nil })
	app.OnStop(func(context.Context) error { stops.Add(1); return nil })
	jobStarted := make(chan struct{})
	finishJob := make(chan struct{})
	var finishJobOnce sync.Once
	finishJobHandler := func() { finishJobOnce.Do(func() { close(finishJob) }) }
	defer finishJobHandler()
	jobs.MustRegister(app.Jobs().Registry(), func(context.Context, workerJob) error {
		close(jobStarted)
		<-finishJob
		return nil
	})
	if _, err := app.Jobs().Dispatch(context.Background(), workerJob{N: 1}); err != nil {
		t.Fatal(err)
	}

	webCtx, stopWeb := context.WithCancel(context.Background())
	defer stopWeb()
	webDone := make(chan error, 1)
	go func() { webDone <- RunContext(webCtx, app) }()
	select {
	case <-serverStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("server start hook did not run")
	}
	waitForAppAddr(t, app)

	workerCtx, stopWorker := context.WithCancel(context.Background())
	workerDone := make(chan error, 1)
	go func() {
		workerDone <- RunWorker(workerCtx, app, jobs.WorkerOptions{PollInterval: 5 * time.Millisecond})
	}()
	select {
	case <-jobStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("embedded worker did not start its queued job")
	}
	finishJobHandler()
	stopWorker()
	if err := <-workerDone; err != nil {
		t.Fatalf("RunWorker() = %v", err)
	}

	if n := starts.Load(); n != 1 {
		t.Errorf("start hooks ran %d times, want 1", n)
	}
	if n := stops.Load(); n != 0 {
		t.Errorf("stop hooks ran %d times while the web server is still serving", n)
	}
	if _, err := app.Jobs().Dispatch(context.Background(), workerJob{N: 1}); err != nil {
		t.Errorf("Dispatch from the still-running web process: %v", err)
	}
	stopWeb()
	if err := <-webDone; err != nil {
		t.Fatalf("RunContext() = %v", err)
	}
	if n := stops.Load(); n != 1 {
		t.Errorf("stop hooks ran %d times after the app stopped, want 1", n)
	}
}

func TestRunWorkerStartedBeforeTheWebProcessLeavesTheAppLifecycleToTheServer(t *testing.T) {
	cfg := config.Default()
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.Jobs.Driver = config.JobsDriverMemory
	app, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	var starts, stops atomic.Int32
	workerStarted := make(chan struct{})
	app.OnStart(func(context.Context) error { starts.Add(1); close(workerStarted); return nil })
	app.OnStop(func(context.Context) error { stops.Add(1); return nil })
	jobs.MustRegister(app.Jobs().Registry(), func(context.Context, workerJob) error { return nil })

	workerCtx, stopWorker := context.WithCancel(context.Background())
	workerDone := make(chan error, 1)
	go func() {
		workerDone <- RunWorker(workerCtx, app, jobs.WorkerOptions{PollInterval: 5 * time.Millisecond})
	}()
	select {
	case <-workerStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("worker start hook did not run")
	}

	webCtx, stopWeb := context.WithCancel(context.Background())
	defer stopWeb()
	webDone := make(chan error, 1)
	go func() { webDone <- RunContext(webCtx, app) }()
	waitForAppAddr(t, app)

	stopWorker()
	if err := <-workerDone; err != nil {
		t.Fatalf("RunWorker() = %v", err)
	}

	if n := starts.Load(); n != 1 {
		t.Errorf("start hooks ran %d times, want 1", n)
	}
	if n := stops.Load(); n != 0 {
		t.Errorf("stop hooks ran %d times while the web server is still serving", n)
	}
	if _, err := app.Jobs().Dispatch(context.Background(), workerJob{N: 1}); err != nil {
		t.Errorf("Dispatch from the still-running web process: %v", err)
	}
	stopWeb()
	if err := <-webDone; err != nil {
		t.Fatalf("RunContext() = %v", err)
	}
	if n := stops.Load(); n != 1 {
		t.Errorf("stop hooks ran %d times after the app stopped, want 1", n)
	}
}

func TestConcurrentRunnersWaitForStartHooksAndShareTheirError(t *testing.T) {
	cfg := config.Default()
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.Jobs.Driver = config.JobsDriverMemory
	app, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	startEntered := make(chan struct{})
	finishStart := make(chan struct{})
	var finishStartOnce sync.Once
	finishStartHook := func() { finishStartOnce.Do(func() { close(finishStart) }) }
	defer finishStartHook()
	startErr := errors.New("start failed")
	var startCalls, stopCalls atomic.Int32
	app.OnStart(func(context.Context) error {
		startCalls.Add(1)
		close(startEntered)
		<-finishStart
		return startErr
	})
	app.OnStop(func(context.Context) error { stopCalls.Add(1); return nil })
	app.Router().GET("/lifecycle-probe", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	workerDone := make(chan error, 1)
	go func() {
		workerDone <- RunWorker(context.Background(), app, jobs.WorkerOptions{PollInterval: time.Millisecond})
	}()
	select {
	case <-startEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("start hook did not begin")
	}

	webCtx, cancelWeb := context.WithCancel(context.Background())
	defer cancelWeb()
	webDone := make(chan error, 1)
	go func() { webDone <- RunContext(webCtx, app) }()
	addr := waitForAppAddr(t, app)
	client := &http.Client{Timeout: 100 * time.Millisecond}
	response, requestErr := client.Get("http://" + addr + "/lifecycle-probe")
	if requestErr == nil {
		_ = response.Body.Close()
		t.Fatal("server began serving before the shared start hooks completed")
	}

	finishStartHook()
	for name, done := range map[string]<-chan error{"worker": workerDone, "web": webDone} {
		select {
		case got := <-done:
			if !errors.Is(got, startErr) {
				t.Errorf("%s runner error = %v, want wrapped start error", name, got)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("%s runner did not return", name)
		}
	}
	if got := startCalls.Load(); got != 1 {
		t.Errorf("start hooks ran %d times, want 1", got)
	}
	if got := stopCalls.Load(); got != 1 {
		t.Errorf("stop hooks ran %d times, want 1", got)
	}
}

func TestEmbeddedWorkerDoesNotRunBeforeServerStartHooksSucceed(t *testing.T) {
	cfg := config.Default()
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.Jobs.Driver = config.JobsDriverMemory
	app, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	startEntered := make(chan struct{})
	finishStart := make(chan struct{})
	var finishStartOnce sync.Once
	finishStartHook := func() { finishStartOnce.Do(func() { close(finishStart) }) }
	defer finishStartHook()
	startErr := errors.New("server start failed")
	var startCalls atomic.Int32
	app.OnStart(func(context.Context) error {
		startCalls.Add(1)
		close(startEntered)
		<-finishStart
		return startErr
	})
	jobRan := make(chan struct{}, 1)
	jobs.MustRegister(app.Jobs().Registry(), func(context.Context, workerJob) error {
		jobRan <- struct{}{}
		return nil
	})
	if _, err := app.Jobs().Dispatch(context.Background(), workerJob{N: 1}); err != nil {
		t.Fatal(err)
	}

	webCtx, cancelWeb := context.WithCancel(context.Background())
	defer cancelWeb()
	webDone := make(chan error, 1)
	go func() { webDone <- RunContext(webCtx, app) }()
	select {
	case <-startEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("server start hook did not begin")
	}
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	defer cancelWorker()
	workerDone := make(chan error, 1)
	go func() {
		workerDone <- RunWorker(workerCtx, app, jobs.WorkerOptions{PollInterval: time.Millisecond})
	}()
	waitForStartWaiters(t, app, 1)
	select {
	case <-jobRan:
		t.Fatal("embedded worker ran a job before the shared start hook succeeded")
	default:
	}

	finishStartHook()
	for name, done := range map[string]<-chan error{"web": webDone, "worker": workerDone} {
		select {
		case got := <-done:
			if !errors.Is(got, startErr) {
				t.Errorf("%s runner error = %v, want wrapped start error", name, got)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("%s runner did not return", name)
		}
	}
	if got := startCalls.Load(); got != 1 {
		t.Errorf("start hooks ran %d times, want 1", got)
	}
}

func TestServerShutdownWaitsForEmbeddedWorkerBeforeStopHooks(t *testing.T) {
	cfg := config.Default()
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.Jobs.Driver = config.JobsDriverMemory
	app, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	jobStarted := make(chan int, 2)
	finishJobs := map[int]chan struct{}{1: make(chan struct{}), 2: make(chan struct{})}
	var finishJobOnce [3]sync.Once
	finishJobHandler := func(n int) { finishJobOnce[n].Do(func() { close(finishJobs[n]) }) }
	defer func() { finishJobHandler(1); finishJobHandler(2) }()
	var startCalls, stopCalls atomic.Int32
	webStarted := make(chan struct{})
	app.OnStart(func(context.Context) error { startCalls.Add(1); close(webStarted); return nil })
	app.OnStop(func(context.Context) error { stopCalls.Add(1); return nil })
	jobs.MustRegister(app.Jobs().Registry(), func(_ context.Context, job workerJob) error {
		jobStarted <- job.N
		<-finishJobs[job.N]
		return nil
	})
	for _, n := range []int{1, 2} {
		if _, err := app.Jobs().Dispatch(context.Background(), workerJob{N: n}); err != nil {
			t.Fatal(err)
		}
	}

	sharedCtx, stopAll := context.WithCancel(context.Background())
	defer stopAll()
	webDone := make(chan error, 1)
	go func() { webDone <- RunContext(sharedCtx, app) }()
	select {
	case <-webStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("web server start hook did not run")
	}
	workerDones := []chan error{make(chan error, 1), make(chan error, 1)}
	for _, done := range workerDones {
		go func(done chan error) {
			done <- RunWorker(sharedCtx, app, jobs.WorkerOptions{PollInterval: time.Millisecond})
		}(done)
	}
	startedJobs := make(map[int]bool)
	for len(startedJobs) < 2 {
		select {
		case n := <-jobStarted:
			startedJobs[n] = true
		case <-time.After(5 * time.Second):
			t.Fatal("both workers did not start their queued jobs")
		}
	}

	stopAll()
	select {
	case err := <-webDone:
		if err != nil {
			t.Fatalf("RunContext() = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("web server did not stop")
	}
	if got := stopCalls.Load(); got != 0 {
		t.Fatalf("stop hooks ran %d times while the worker was still running", got)
	}

	finishJobHandler(1)
	finishedWorkers := 0
	for finishedWorkers < 1 {
		select {
		case err := <-workerDones[0]:
			if err != nil {
				t.Fatalf("RunWorker() = %v", err)
			}
			finishedWorkers++
		case err := <-workerDones[1]:
			if err != nil {
				t.Fatalf("RunWorker() = %v", err)
			}
			finishedWorkers++
		case <-time.After(5 * time.Second):
			t.Fatal("worker did not finish the released job")
		}
	}
	if got := stopCalls.Load(); got != 0 {
		t.Fatalf("stop hooks ran %d times while the second worker was still draining", got)
	}
	finishJobHandler(2)
	finishedWorkers = 0
	for finishedWorkers < 1 {
		select {
		case err := <-workerDones[0]:
			if err != nil {
				t.Fatalf("RunWorker() = %v", err)
			}
			finishedWorkers++
		case err := <-workerDones[1]:
			if err != nil {
				t.Fatalf("RunWorker() = %v", err)
			}
			finishedWorkers++
		case <-time.After(5 * time.Second):
			t.Fatal("last worker did not stop")
		}
	}
	if got := startCalls.Load(); got != 1 {
		t.Errorf("start hooks ran %d times, want 1", got)
	}
	if got := stopCalls.Load(); got != 1 {
		t.Errorf("stop hooks ran %d times after the last runner left, want 1", got)
	}
}

func TestRunWorkerRejectsStoppedApp(t *testing.T) {
	cfg := config.Default()
	cfg.Jobs.Driver = config.JobsDriverMemory
	app, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RunWorker(ctx, app, jobs.WorkerOptions{PollInterval: time.Millisecond}); err != nil {
		t.Fatalf("first RunWorker() = %v", err)
	}
	if err := RunWorker(context.Background(), app, jobs.WorkerOptions{}); !errors.Is(err, ErrAppAlreadyStopped) {
		t.Fatalf("RunWorker() on stopped App = %v, want ErrAppAlreadyStopped", err)
	}
	if err := RunContext(context.Background(), app); !errors.Is(err, ErrAppAlreadyStopped) {
		t.Fatalf("RunContext() on stopped App = %v, want ErrAppAlreadyStopped", err)
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

func waitForAppAddr(t *testing.T, app *App) string {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		if addr := app.Addr(); addr != "" {
			return addr
		}
		select {
		case <-timer.C:
			t.Fatal("timed out waiting for app listener")
		default:
			runtime.Gosched()
		}
	}
}

func waitForStartWaiters(t *testing.T, app *App, want int) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		app.mu.RLock()
		waiters := app.startWaiters
		changed := app.startWaitersChanged
		app.mu.RUnlock()
		if waiters >= want {
			return
		}
		select {
		case <-timer.C:
			t.Fatalf("start hook waiters = %d, want at least %d", waiters, want)
		case <-changed:
		}
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
