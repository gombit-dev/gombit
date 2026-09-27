package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/gombit-dev/gombit/dev"
	"github.com/gombit-dev/gombit/framework"
)

func newWorkerCommand(stdout io.Writer, stderr io.Writer) *cobra.Command {
	return silence(&cobra.Command{
		Use:   "worker [--queue default] [--concurrency N] [--lease 5m] [--shutdown-timeout 30s]",
		Short: "Run this app's background-job worker",
		Long: `Run the app's jobs worker from an application directory: it builds ./cmd/server
and runs it as "server worker", with these flags passed through.

The worker is part of the app binary, because it runs the app's own job
handlers. In production run the built binary the same way:

  ./server worker --queue critical,default --concurrency 8

Flags:
  --queue             queue to consume; repeat or comma-separate for several,
                      highest priority first (default: GOMBIT_JOBS_QUEUE)
  --concurrency       jobs to run at once (default 1)
  --lease             how long a reserved job is held; renewed while it runs
                      (default 5m)
  --shutdown-timeout  how long in-flight jobs get to finish on SIGINT/SIGTERM
                      before their context is canceled (default 30s)

The worker needs a queue a separate process can reach: GOMBIT_JOBS_DRIVER=redis.
With the sync driver jobs run as they are dispatched; the memory driver's queue
lives inside the dispatching process.`,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, arg := range args {
				if arg == "-h" || arg == "--help" {
					return cmd.Help()
				}
			}
			if _, err := framework.ParseWorkerFlags(args, stderr); err != nil {
				if errors.Is(err, flag.ErrHelp) {
					return nil
				}
				return fmt.Errorf("gombit worker: %w", err)
			}
			if _, err := os.Stat(filepath.Join("cmd", "server")); err != nil {
				return errors.New("gombit worker: cmd/server not found; run it from a gombit new application directory, or run your built binary with: ./server worker")
			}
			// Build, then run the server binary itself: `go run` does not catch
			// SIGTERM, so it would die at once and leave the worker it started
			// unsupervised. The process signaled and waited on must be the one
			// framework.Run is shutting down.
			bin, cleanup, err := buildServer(cmd.Context(), stdout, stderr)
			if err != nil {
				return err
			}
			defer cleanup()
			spec := dev.ProcSpec{
				Name: "worker",
				Dir:  ".",
				Path: bin,
				Args: append([]string{framework.WorkerCommand}, args...),
			}
			err = runWorkerProcess(cmd.Context(), spec, stdout, stderr, framework.WorkerKillAfter(args))
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		},
	})
}

// runWorkerProcess starts the app's worker; tests replace it.
var runWorkerProcess = dev.RunProcess

// buildServer compiles ./cmd/server into a temporary directory; tests
// replace it.
var buildServer = func(ctx context.Context, stdout, stderr io.Writer) (bin string, cleanup func(), err error) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		return "", nil, fmt.Errorf("gombit worker: go not found on PATH: %w", err)
	}
	dir, err := os.MkdirTemp("", "gombit-worker-*")
	if err != nil {
		return "", nil, fmt.Errorf("gombit worker: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	bin = filepath.Join(dir, "server")
	build := exec.CommandContext(ctx, goBin, "build", "-o", bin, "./cmd/server") // #nosec G204 -- fixed go build of the app's own cmd/server
	build.Stdout, build.Stderr = stdout, stderr
	if err := build.Run(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("gombit worker: build ./cmd/server: %w", err)
	}
	return bin, cleanup, nil
}
