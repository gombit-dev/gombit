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
// HTTP. opts.Queues defaults to the dispatcher's default queue and
// opts.Logger to the app's logger. Like RunContext, every return runs the stop
// hooks and closes what the app opened, so the App cannot be run again.
func RunWorker(ctx context.Context, app *App, opts jobs.WorkerOptions) error {
	if app == nil {
		return errors.New("framework: nil app")
	}
	if ctx == nil {
		return errors.Join(errors.New("framework: nil context"), app.runStopHooks())
	}
	dispatcher := app.Jobs()
	if len(opts.Queues) == 0 {
		opts.Queues = []string{dispatcher.DefaultQueue()}
	}
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
	opts := flags.WorkerOptions
	switch app.Config().Jobs.Driver {
	case config.JobsDriverSync:
		return opts, nil, fmt.Errorf("framework: worker: GOMBIT_JOBS_DRIVER is sync, which runs every job when it is dispatched; there is no queue to work. Set GOMBIT_JOBS_DRIVER=redis")
	case config.JobsDriverMemory:
		return opts, nil, fmt.Errorf("framework: worker: GOMBIT_JOBS_DRIVER is memory, whose queue lives inside the process that dispatches; a separate worker process cannot see it. Set GOMBIT_JOBS_DRIVER=redis")
	}
	if flags.MetricsAddr == "" {
		return opts, func() error { return nil }, nil
	}
	queues := opts.Queues
	if len(queues) == 0 {
		queues = []string{app.Jobs().DefaultQueue()}
	}
	_, stop, err := serveWorkerMetrics(flags.MetricsAddr, app, queues)
	if err != nil {
		return opts, nil, err
	}
	return opts, stop, nil
}

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
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
// --concurrency, --lease, --shutdown-timeout, and --metrics-addr.
func ParseWorkerFlags(args []string, output io.Writer) (WorkerFlags, error) {
	fs := flag.NewFlagSet(WorkerCommand, flag.ContinueOnError)
	if output == nil {
		output = io.Discard
	}
	fs.SetOutput(output)
	var queues queueList
	fs.Var(&queues, "queue", "queue to consume; repeat or comma-separate for several, highest priority first (default: GOMBIT_JOBS_QUEUE)")
	concurrency := fs.Int("concurrency", 1, "jobs to run at once")
	lease := fs.Duration("lease", jobs.DefaultLease, "how long a reserved job is held; renewed while it runs")
	shutdown := fs.Duration("shutdown-timeout", jobs.DefaultShutdownTimeout, "how long in-flight jobs get to finish on shutdown")
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
	if *concurrency < 1 {
		return WorkerFlags{}, fmt.Errorf("framework: worker: --concurrency must be at least 1, got %d", *concurrency)
	}
	if *lease <= 0 || *shutdown <= 0 {
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

// workerShutdownGrace is how much longer than the worker's own shutdown
// timeout a supervisor must wait before killing it, the sum of what follows
// SIGTERM besides that timeout:
//
//   - a reserve already on the wire, and the release of what it returns
//     (2 × jobs.QueueCallTimeout), before the shutdown timeout starts;
//   - jobs.ShutdownGrace, for jobs canceled at the timeout to return and be
//     acknowledged or released;
//   - the app's stop hooks, which run with the default shutdown timeout.
const workerShutdownGrace = 2*jobs.QueueCallTimeout + jobs.ShutdownGrace + defaultShutdownTimeout

// WorkerKillAfter is how long a process supervisor should wait after asking a
// worker started with args to stop before killing it: its shutdown timeout
// plus workerShutdownGrace (28s).
func WorkerKillAfter(args []string) time.Duration {
	opts, err := ParseWorkerFlags(args, io.Discard)
	if err != nil || opts.ShutdownTimeout <= 0 {
		return jobs.DefaultShutdownTimeout + workerShutdownGrace
	}
	return opts.ShutdownTimeout + workerShutdownGrace
}
