package framework

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/jobs"
)

// WorkerCommand is the first argument that makes Run start a jobs worker
// instead of the HTTP server: `./server worker --queue mail --concurrency 8`.
// One binary deploys as both the web and the worker process.
const WorkerCommand = "worker"

// RunWorker runs app as a jobs worker until ctx is canceled: the start
// hooks, a jobs.Worker over App.Jobs(), then the stop hooks. It serves no
// HTTP. A zero option takes the app's declared worker setting
// (config.JobsWorkerConfig: GOMBIT_JOBS_WORKER_*): opts.Queues defaults to
// its queues, else the dispatcher's default queue, and opts.Concurrency,
// Lease and ShutdownTimeout to its values. opts.Logger defaults to the app's
// logger. Like RunContext, every return runs the stop hooks and closes what
// the app opened, so the App cannot be run again.
func RunWorker(ctx context.Context, app *App, opts jobs.WorkerOptions) error {
	if app == nil {
		return errors.New("framework: nil app")
	}
	if ctx == nil {
		return errors.Join(errors.New("framework: nil context"), app.runStopHooks())
	}
	dispatcher := app.Jobs()
	opts = app.declaredWorkerOptions(opts)
	if opts.Logger == nil {
		opts.Logger = app.Logger()
	}
	if opts.Metrics == nil {
		opts.Metrics = app.JobMetrics()
	}
	if opts.Metrics == app.JobMetrics() && dispatcher.Queue() != nil {
		// The app's /metrics reports these queues' depth too.
		app.addWorkerQueues(opts.Queues)
	}
	worker, err := jobs.NewWorker(dispatcher.Registry(), dispatcher.Queue(), opts)
	if err != nil {
		// Released as on every other return (issue #435).
		return errors.Join(err, app.runStopHooks())
	}
	if err := app.runStartHooks(ctx); err != nil {
		return errors.Join(err, app.runStopHooks())
	}
	runErr := worker.Run(ctx)
	return errors.Join(runErr, app.runStopHooks())
}

// declaredWorkerOptions fills opts's zero queues, concurrency, lease and
// shutdown timeout from the app's declared worker settings. It is the one
// place a worker's settings default: ParseWorkerFlags leaves a flag not given
// zero, so `./server worker` and RunWorker resolve it here alike.
func (a *App) declaredWorkerOptions(opts jobs.WorkerOptions) jobs.WorkerOptions {
	declared := a.Config().Jobs.Worker
	if len(opts.Queues) == 0 {
		opts.Queues = slices.Clone(declared.Queues)
	}
	if len(opts.Queues) == 0 {
		opts.Queues = []string{a.Jobs().DefaultQueue()}
	}
	if opts.Concurrency == 0 {
		opts.Concurrency = declared.EffectiveConcurrency()
	}
	if opts.Lease == 0 {
		opts.Lease = declared.EffectiveLease()
	}
	if opts.ShutdownTimeout == 0 {
		opts.ShutdownTimeout = declared.EffectiveShutdownTimeout()
	}
	return opts
}

// runWorkerCommand is Run's `worker` mode: it parses the worker flags and
// refuses a driver a separate worker process cannot consume. Like RunWorker,
// it releases the app on every return: once RunWorker runs, RunWorker does;
// before that, this does, -h included (issue #435).
func runWorkerCommand(ctx context.Context, app *App, args []string, stderr io.Writer) error {
	opts, stopMetrics, err := prepareWorkerCommand(app, args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			err = nil
		}
		return errors.Join(err, app.runStopHooks())
	}
	runErr := RunWorker(ctx, app, opts)
	return errors.Join(runErr, stopMetrics())
}

// prepareWorkerCommand does everything worker mode needs before the worker
// runs: it parses the flags, refuses a driver a worker cannot consume, and
// starts the --metrics-addr endpoint. stopMetrics is a no-op without one.
// flag.ErrHelp means -h printed the usage.
func prepareWorkerCommand(app *App, args []string, stderr io.Writer) (_ jobs.WorkerOptions, stopMetrics func() error, _ error) {
	flags, err := ParseWorkerFlags(args, stderr)
	if err != nil {
		return jobs.WorkerOptions{}, nil, err
	}
	// The options the worker runs with, so the metrics endpoint reports the
	// queues it consumes.
	opts := app.declaredWorkerOptions(flags.WorkerOptions)
	// One predicate decides whether a separate worker can consume the
	// driver's queue, the one the application contract lists a worker by.
	if driver := app.Config().Jobs.Driver; !driver.NeedsWorker() {
		switch driver {
		case config.JobsDriverSync:
			return opts, nil, fmt.Errorf("framework: worker: GOMBIT_JOBS_DRIVER is sync, which runs every job when it is dispatched; there is no queue to work. Set GOMBIT_JOBS_DRIVER=redis")
		case config.JobsDriverMemory:
			return opts, nil, fmt.Errorf("framework: worker: GOMBIT_JOBS_DRIVER is memory, whose queue lives inside the process that dispatches; a separate worker process cannot see it. Set GOMBIT_JOBS_DRIVER=redis")
		default:
			return opts, nil, fmt.Errorf("framework: worker: GOMBIT_JOBS_DRIVER is %s, whose queue no separate worker process consumes. Set GOMBIT_JOBS_DRIVER=redis", driver)
		}
	}
	if flags.MetricsAddr == "" {
		return opts, func() error { return nil }, nil
	}
	_, stop, err := serveWorkerMetrics(flags.MetricsAddr, app, opts.Queues)
	if err != nil {
		return opts, nil, err
	}
	return opts, stop, nil
}

// workerMetricsShutdownTimeout bounds the stop of a worker's --metrics-addr
// endpoint, after the worker and its stop hooks.
const workerMetricsShutdownTimeout = 5 * time.Second

// serveWorkerMetrics serves a worker process's /metrics (its job outcomes,
// then each queue's depth, read at scrape time) and /livez on addr, until the
// returned stop is called.
// queueStatsTimeout bounds the queue reads of one metrics scrape.
const queueStatsTimeout = 2 * time.Second

// readQueueStats reads each queue's depth for the queue gauges. It returns
// what it read and an error naming each queue that did not answer.
func readQueueStats(ctx context.Context, q jobs.Queue, queues []string) (map[string]jobs.QueueStats, error) {
	stats := map[string]jobs.QueueStats{}
	if q == nil {
		return stats, nil
	}
	var errs []error
	for _, name := range queues {
		st, err := q.Stats(ctx, name)
		if err != nil {
			errs = append(errs, fmt.Errorf("queue %s: %w", name, err))
			continue
		}
		stats[name] = st
	}
	return stats, errors.Join(errs...)
}

func serveWorkerMetrics(addr string, app *App, queues []string) (bound string, stop func() error, err error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return "", nil, fmt.Errorf("framework: worker: metrics listener: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"status":"ok"}}`)
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), queueStatsTimeout)
		defer cancel()
		stats, err := readQueueStats(ctx, app.Jobs().Queue(), queues)
		if err != nil {
			// Fail the scrape rather than drop the queue gauges: Prometheus
			// keeps the last good sample instead of reading an empty queue.
			app.Logger().Warn("jobs worker: metrics scrape failed reading queue stats", zap.Error(err))
			http.Error(w, "jobs: queue stats: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_ = app.JobMetrics().WritePrometheus(w, stats, time.Now())
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			app.Logger().Error("jobs worker: metrics server stopped", zap.String("addr", addr), zap.Error(err))
		}
	}()
	return listener.Addr().String(), func() error {
		ctx, cancel := context.WithTimeout(context.Background(), workerMetricsShutdownTimeout)
		defer cancel()
		return server.Shutdown(ctx)
	}, nil
}

// WorkerFlags are the parsed worker flags: the worker's options, plus where
// to serve its metrics.
type WorkerFlags struct {
	jobs.WorkerOptions
	// MetricsAddr, when set, serves /metrics (job outcomes and queue depth)
	// and /livez on that address while the worker runs.
	MetricsAddr string
}

// ParseWorkerFlags parses the worker flags `gombit worker` and `./server
// worker` share: --queue (repeatable or comma-separated, in priority order),
// --concurrency, --lease, --shutdown-timeout, and --metrics-addr. A flag not
// given is left zero, and RunWorker takes the app's declared setting for it
// (GOMBIT_JOBS_WORKER_*), else gombit's default: a flag overrides its setting
// for one run. A flag given must be valid: --concurrency at least 1, --lease
// and --shutdown-timeout positive.
func ParseWorkerFlags(args []string, output io.Writer) (WorkerFlags, error) {
	fs := flag.NewFlagSet(WorkerCommand, flag.ContinueOnError)
	if output == nil {
		output = io.Discard
	}
	fs.SetOutput(output)
	var queues queueList
	fs.Var(&queues, "queue", "queue to consume; repeat or comma-separate for several, highest priority first (default: GOMBIT_JOBS_WORKER_QUEUES, else GOMBIT_JOBS_QUEUE)")
	concurrency := fs.Int("concurrency", 0, "jobs to run at once (default: GOMBIT_JOBS_WORKER_CONCURRENCY, else 1)")
	lease := fs.Duration("lease", 0, "how long a reserved job is held; renewed while it runs (default: GOMBIT_JOBS_WORKER_LEASE, else 5m)")
	shutdown := fs.Duration("shutdown-timeout", 0, "how long in-flight jobs get to finish on shutdown (default: GOMBIT_JOBS_WORKER_SHUTDOWN_TIMEOUT, else 30s)")
	metricsAddr := fs.String("metrics-addr", "", "serve /metrics and /livez on this address (e.g. :9091); off by default")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(output, "Usage: %s worker [flags]\n\nRun this app's background-job worker.\n\n", os.Args[0])
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return WorkerFlags{}, err
	}
	if fs.NArg() > 0 {
		return WorkerFlags{}, fmt.Errorf("framework: worker: unexpected argument %q", fs.Arg(0))
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	if given["concurrency"] && *concurrency < 1 {
		return WorkerFlags{}, fmt.Errorf("framework: worker: --concurrency must be at least 1, got %d", *concurrency)
	}
	if (given["lease"] && *lease <= 0) || (given["shutdown-timeout"] && *shutdown <= 0) {
		return WorkerFlags{}, errors.New("framework: worker: --lease and --shutdown-timeout must be positive")
	}
	return WorkerFlags{
		WorkerOptions: jobs.WorkerOptions{
			Queues:          queues,
			Concurrency:     *concurrency,
			Lease:           *lease,
			ShutdownTimeout: *shutdown,
		},
		MetricsAddr: *metricsAddr,
	}, nil
}

type queueList []string

func (q *queueList) String() string { return strings.Join(*q, ",") }

func (q *queueList) Set(value string) error {
	for _, name := range strings.Split(value, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !jobs.ValidName(name) {
			return fmt.Errorf("invalid queue name %q", name)
		}
		*q = append(*q, name)
	}
	return nil
}

// workerQueueGrace is the part of a worker's stop that is neither its own
// shutdown timeout nor its stop hooks:
//
//   - a reserve already on the wire, and the release of what it returns
//     (2 × jobs.QueueCallTimeout), before the shutdown timeout starts;
//   - jobs.ShutdownGrace, for jobs canceled at the timeout to return and be
//     acknowledged or released.
const workerQueueGrace = 2*jobs.QueueCallTimeout + jobs.ShutdownGrace

// WorkerKillAfterFor is how long a process supervisor should wait, after
// asking a worker of an app configured by cfg and started with args to stop,
// before it kills it:
//
//   - the worker's shutdown timeout (its --shutdown-timeout, else cfg's
//     Jobs.Worker.ShutdownTimeout);
//   - 18s for the queue calls and the canceled jobs to settle;
//   - cfg's Shutdown.Timeout for the stop hooks;
//   - 5s for the --metrics-addr endpoint to stop, when args serve one;
//   - 2s for the exit, as for the web process (WebKillAfter).
//
// That is 60s at gombit's defaults with no flags. A worker killed sooner can
// lose a job's ack or release, and the job then waits out its lease. Args it
// can't parse count as none. The application contract declares this for a
// worker started with no flags.
func WorkerKillAfterFor(cfg config.Config, args []string) time.Duration {
	shutdown := cfg.Jobs.Worker.EffectiveShutdownTimeout()
	var metrics time.Duration
	if flags, err := ParseWorkerFlags(args, io.Discard); err == nil {
		if flags.ShutdownTimeout > 0 {
			shutdown = flags.ShutdownTimeout
		}
		if flags.MetricsAddr != "" {
			metrics = workerMetricsShutdownTimeout
		}
	}
	return shutdown + workerQueueGrace + cfg.Shutdown.EffectiveTimeout() + metrics + shutdownExitGrace
}

// WorkerKillAfter is WorkerKillAfterFor at gombit's default configuration.
//
// Deprecated: since v0.11.0, use WorkerKillAfterFor with the app's config:
// the app's declared worker and shutdown settings change the figure, and
// WorkerKillAfter counts gombit's defaults instead.
func WorkerKillAfter(args []string) time.Duration {
	return WorkerKillAfterFor(config.Default(), args)
}
