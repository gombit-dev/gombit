package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/dev"
)

func TestWorkerCommandRunsTheAppsWorker(t *testing.T) {
	app := t.TempDir()
	t.Chdir(app)
	var got dev.ProcSpec
	var gotKill time.Duration
	prev := runWorkerProcess
	t.Cleanup(func() { runWorkerProcess = prev })
	runWorkerProcess = func(_ context.Context, spec dev.ProcSpec, _, _ io.Writer, killAfter time.Duration) error {
		got, gotKill = spec, killAfter
		return nil
	}
	prevBuild := buildServer
	t.Cleanup(func() { buildServer = prevBuild })
	built := false
	buildServer = func(context.Context, io.Writer, io.Writer) (string, func(), error) {
		built = true
		return "/tmp/built/server", func() {}, nil
	}
	run := func(args ...string) (string, error) {
		stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
		err := ExecuteRoot(context.Background(), NewRoot(stdout, stderr), append([]string{"worker"}, args...))
		return stdout.String() + stderr.String(), err
	}

	if _, err := run(); err == nil || !strings.Contains(err.Error(), "cmd/server not found") {
		t.Fatalf("worker outside an app = %v, want the cmd/server hint", err)
	}
	if err := os.MkdirAll(filepath.Join("cmd", "server"), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := run("--queue", "mail,default", "--concurrency", "4", "--shutdown-timeout", "1m"); err != nil {
		t.Fatal(err)
	}
	// The supervised process is the built server itself, not `go run`.
	if !built || got.Path != "/tmp/built/server" || strings.Join(got.Args, " ") != "worker --queue mail,default --concurrency 4 --shutdown-timeout 1m" {
		t.Fatalf("ran %s %v (built %v), want the built server with the worker args", got.Path, got.Args, built)
	}
	if gotKill <= time.Minute {
		t.Fatalf("kill after %s, want longer than the worker's 1m shutdown timeout", gotKill)
	}
	if _, err := run("--concurrency", "0"); err == nil {
		t.Fatal("worker accepted --concurrency 0")
	}
	out, err := run("--help")
	if err != nil || !strings.Contains(out, "./server worker") || !strings.Contains(out, "GOMBIT_JOBS_DRIVER=redis") {
		t.Fatalf("worker --help = %v:\n%s", err, out)
	}
}
