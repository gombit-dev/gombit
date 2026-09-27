package framework

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

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
// opts.Logger to the app's logger.
func RunWorker(ctx context.Context, app *App, opts jobs.WorkerOptions) error {
	if ctx == nil {
		return errors.New("framework: nil context")
	}
	if app == nil {
		return errors.New("framework: nil app")
	}
	dispatcher := app.Jobs()
	if len(opts.Queues) == 0 {
		opts.Queues = []string{dispatcher.DefaultQueue()}
	}
	if opts.Logger == nil {
		opts.Logger = app.Logger()
	}
	worker, err := jobs.NewWorker(dispatcher.Registry(), dispatcher.Queue(), opts)
	if err != nil {
		return err
	}
	if err := app.runStartHooks(ctx); err != nil {
		return errors.Join(err, app.runStopHooks())
	}
	runErr := worker.Run(ctx)
	return errors.Join(runErr, app.runStopHooks())
}

// runWorkerCommand is Run's `worker` mode: it parses the worker flags and
// refuses a driver a separate worker process cannot consume.
func runWorkerCommand(ctx context.Context, app *App, args []string, stderr io.Writer) error {
	opts, err := ParseWorkerFlags(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	switch app.Config().Jobs.Driver {
	case config.JobsDriverSync:
		return fmt.Errorf("framework: worker: GOMBIT_JOBS_DRIVER is sync, which runs every job when it is dispatched; there is no queue to work. Set GOMBIT_JOBS_DRIVER=redis")
	case config.JobsDriverMemory:
		return fmt.Errorf("framework: worker: GOMBIT_JOBS_DRIVER is memory, whose queue lives inside the process that dispatches; a separate worker process cannot see it. Set GOMBIT_JOBS_DRIVER=redis")
	}
	return RunWorker(ctx, app, opts)
}

// ParseWorkerFlags parses the worker flags `gombit worker` and `./server
// worker` share: --queue (repeatable or comma-separated, in priority order),
// --concurrency, --lease, and --shutdown-timeout.
func ParseWorkerFlags(args []string, output io.Writer) (jobs.WorkerOptions, error) {
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
	fs.Usage = func() {
		_, _ = fmt.Fprintf(output, "Usage: %s worker [flags]\n\nRun this app's background-job worker.\n\n", os.Args[0])
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return jobs.WorkerOptions{}, err
	}
	if fs.NArg() > 0 {
		return jobs.WorkerOptions{}, fmt.Errorf("framework: worker: unexpected argument %q", fs.Arg(0))
	}
	if *concurrency < 1 {
		return jobs.WorkerOptions{}, fmt.Errorf("framework: worker: --concurrency must be at least 1, got %d", *concurrency)
	}
	if *lease <= 0 || *shutdown <= 0 {
		return jobs.WorkerOptions{}, errors.New("framework: worker: --lease and --shutdown-timeout must be positive")
	}
	return jobs.WorkerOptions{
		Queues:          queues,
		Concurrency:     *concurrency,
		Lease:           *lease,
		ShutdownTimeout: *shutdown,
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
// timeout a supervisor should wait before killing it.
const workerShutdownGrace = 10 * time.Second

// WorkerKillAfter is how long a process supervisor should wait after asking a
// worker started with args to stop before killing it: its shutdown timeout
// plus a grace.
func WorkerKillAfter(args []string) time.Duration {
	opts, err := ParseWorkerFlags(args, io.Discard)
	if err != nil || opts.ShutdownTimeout <= 0 {
		return jobs.DefaultShutdownTimeout + workerShutdownGrace
	}
	return opts.ShutdownTimeout + workerShutdownGrace
}
