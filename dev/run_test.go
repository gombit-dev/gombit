package dev

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunMissingFrontendPackageJSON(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workDir, "cmd", "server"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	err := Run(context.Background(), Options{WorkDir: workDir})
	if err == nil {
		t.Fatal("Run() error = nil, want missing frontend/package.json")
	}
	if !strings.Contains(err.Error(), "frontend/package.json") {
		t.Fatalf("Run() error = %q, want frontend/package.json", err)
	}
	if !strings.Contains(err.Error(), "backend-only") {
		t.Fatalf("Run() error = %q, want backend-only hint", err)
	}
}

func TestRunMissingServer(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	writeFile(t, filepath.Join(workDir, "frontend", "package.json"), `{"name":"frontend"}`)
	err := Run(context.Background(), Options{WorkDir: workDir})
	if err == nil {
		t.Fatal("Run() error = nil, want missing cmd/server")
	}
	if !strings.Contains(err.Error(), "cmd/server") {
		t.Fatalf("Run() error = %q, want cmd/server", err)
	}
}

func TestRunPrintsServiceTableAndShutsDown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh process-group shutdown")
	}

	workDir := writeDevApp(t)
	stdout := &syncBuffer{}
	stderr := &syncBuffer{}
	var started atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	startedCh := make(chan struct{}, 2)
	opts := Options{
		WorkDir:      workDir,
		HTTPAddr:     ":18080",
		FrontendPort: 15173,
		PollInterval: 50 * time.Millisecond,
		ShutdownWait: 2 * time.Second,
		Stdout:       stdout,
		Stderr:       stderr,
		LookPath: func(file string) (string, error) {
			switch file {
			case "go", "npm":
				return file, nil
			default:
				return "", errors.New("not found")
			}
		},
		Command: func(name string, args ...string) *exec.Cmd {
			started.Add(1)
			startedCh <- struct{}{}
			return exec.Command("sh", "-c", "echo ready; trap 'exit 0' TERM; sleep 60")
		},
		HTTPGet: func(ctx context.Context, rawURL string) ([]byte, error) {
			return nil, errors.New("backend not ready")
		},
		Generate: func(ctx context.Context, spec []byte) error {
			t.Fatal("Generate should not run when HTTPGet fails")
			return nil
		},
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, opts)
	}()

	waitStarts(t, startedCh, 2)
	got := stdout.String()
	if !strings.Contains(got, "/docs") || !strings.Contains(got, "/openapi.json") {
		t.Fatalf("stdout missing service table:\n%s", got)
	}
	if !strings.Contains(got, "http://127.0.0.1:18080") {
		t.Fatalf("stdout missing backend URL:\n%s", got)
	}
	if !strings.Contains(got, "http://127.0.0.1:15173") {
		t.Fatalf("stdout missing frontend URL:\n%s", got)
	}
	if strings.Contains(got, "Admin") {
		t.Fatalf("stdout included Admin without AdminURL:\n%s", got)
	}
	if !strings.Contains(stderr.String(), "without reload") {
		t.Fatalf("stderr = %q, want reload hint", stderr.String())
	}
	if started.Load() < 2 {
		t.Fatalf("started %d child commands, want at least 2", started.Load())
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil after cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after cancel")
	}
}

func TestRunProcessesShutdownOnCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh process-group shutdown")
	}

	var started atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	startedCh := make(chan struct{}, 2)
	specs := []ProcSpec{
		{Name: "backend", Path: "sh", Args: []string{"-c", "echo ready; trap 'exit 0' TERM; sleep 60"}},
		{Name: "frontend", Path: "sh", Args: []string{"-c", "echo ready; trap 'exit 0' TERM; sleep 60"}},
	}
	command := func(name string, args ...string) *exec.Cmd {
		started.Add(1)
		startedCh <- struct{}{}
		return exec.Command(name, args...) //nolint:gosec // test helper rebuilds the injected sh command
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- runProcesses(ctx, specs, ioDiscard{}, ioDiscard{}, command, 2*time.Second, nil)
	}()

	waitStarts(t, startedCh, 2)
	if started.Load() != 2 {
		t.Fatalf("started = %d, want 2", started.Load())
	}
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("runProcesses() error = %v, want nil after cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runProcesses() did not return after cancel")
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) {
	return len(p), nil
}

// syncBuffer is a bytes.Buffer safe for concurrent Write/String. Real
// `gombit dev` always shares its Stdout/Stderr *os.File across child
// processes (os/exec wires those directly, no in-process copy goroutine),
// but a mocked test process makes os/exec spin up a copy goroutine per
// child, so an in-memory sink shared across children (and read from the
// test goroutine while they run) needs its own locking.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func writeDevApp(t *testing.T) string {
	t.Helper()
	workDir := t.TempDir()
	writeFile(t, filepath.Join(workDir, "frontend", "package.json"), `{"name":"frontend","scripts":{"dev":"vite"}}`)
	if err := os.MkdirAll(filepath.Join(workDir, "frontend", "node_modules"), 0o750); err != nil {
		t.Fatalf("mkdir node_modules: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(workDir, "cmd", "server"), 0o750); err != nil {
		t.Fatalf("mkdir cmd/server: %v", err)
	}
	writeFile(t, filepath.Join(workDir, "cmd", "server", "main.go"), "package main\nfunc main() {}\n")
	return workDir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func waitStarts(t *testing.T, startedCh <-chan struct{}, n int) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case <-startedCh:
		case <-deadline:
			t.Fatalf("timed out waiting for %d child starts (got %d)", n, i)
		}
	}
}

// waitCmdsReady receives n cmds sent by runProcesses's onCmdReady hook, which
// fires after cmd.Env is assigned — so each receive has a real happens-before
// edge to that write (Go memory model: a send happens before the
// corresponding receive completes), unlike polling cmd.Env directly.
func waitCmdsReady(t *testing.T, readyCh <-chan *exec.Cmd, n int) []*exec.Cmd {
	t.Helper()
	deadline := time.After(3 * time.Second)
	captured := make([]*exec.Cmd, 0, n)
	for i := 0; i < n; i++ {
		select {
		case cmd := <-readyCh:
			captured = append(captured, cmd)
		case <-deadline:
			t.Fatalf("timed out waiting for %d child cmds to be ready (got %d)", n, i)
		}
	}
	return captured
}

func TestPlanBackendPrefersAir(t *testing.T) {
	t.Parallel()
	plan, err := planBackend(t.TempDir(), func(file string) (string, error) {
		if file == "air" {
			return "/usr/bin/air", nil
		}
		return "", errors.New("missing")
	})
	if err != nil {
		t.Fatalf("planBackend() error = %v", err)
	}
	if plan.Name != "air" {
		t.Fatalf("plan.Name = %q, want air", plan.Name)
	}
	if plan.Hint != "" {
		t.Fatalf("plan.Hint = %q, want empty", plan.Hint)
	}
}

func TestPlanBackendPrefersWatchexecOverGoRun(t *testing.T) {
	t.Parallel()
	plan, err := planBackend(t.TempDir(), func(file string) (string, error) {
		switch file {
		case "watchexec":
			return "/usr/bin/watchexec", nil
		case "go":
			return "/usr/bin/go", nil
		default:
			return "", errors.New("missing")
		}
	})
	if err != nil {
		t.Fatalf("planBackend() error = %v", err)
	}
	if plan.Name != "watchexec" {
		t.Fatalf("plan.Name = %q, want watchexec", plan.Name)
	}
}

func TestPlanBackendUsesAirConfigWhenPresent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".air.toml"), "root = \".\"\n")
	plan, err := planBackend(dir, func(file string) (string, error) {
		if file == "air" {
			return "/usr/bin/air", nil
		}
		return "", errors.New("missing")
	})
	if err != nil {
		t.Fatalf("planBackend() error = %v", err)
	}
	if len(plan.Args) != 2 || plan.Args[0] != "-c" || plan.Args[1] != ".air.toml" {
		t.Fatalf("plan.Args = %v, want -c .air.toml", plan.Args)
	}
}

func TestPlanBackendFallsBackToGoRun(t *testing.T) {
	t.Parallel()
	plan, err := planBackend(t.TempDir(), func(file string) (string, error) {
		if file == "go" {
			return "/usr/bin/go", nil
		}
		return "", errors.New("missing")
	})
	if err != nil {
		t.Fatalf("planBackend() error = %v", err)
	}
	if plan.Name != "go" {
		t.Fatalf("plan.Name = %q, want go", plan.Name)
	}
	if !strings.Contains(plan.Hint, "air and watchexec") {
		t.Fatalf("plan.Hint = %q, want reload hint", plan.Hint)
	}
}

func TestDetectPackageManagerPrefersPnpm(t *testing.T) {
	t.Parallel()
	name, _, err := detectPackageManager(t.TempDir(), func(file string) (string, error) {
		switch file {
		case "pnpm":
			return "/usr/bin/pnpm", nil
		case "npm":
			return "/usr/bin/npm", nil
		default:
			return "", errors.New("missing")
		}
	})
	if err != nil {
		t.Fatalf("detectPackageManager() error = %v", err)
	}
	if name != "pnpm" {
		t.Fatalf("manager = %q, want pnpm", name)
	}
}

func TestDetectPackageManagerRequiresNode(t *testing.T) {
	t.Parallel()
	_, _, err := detectPackageManager(t.TempDir(), func(string) (string, error) {
		return "", errors.New("missing")
	})
	if err == nil {
		t.Fatal("detectPackageManager() error = nil, want npm/pnpm missing")
	}
	if !strings.Contains(err.Error(), "npm and pnpm") {
		t.Fatalf("error = %q, want npm and pnpm", err)
	}
}

func TestDetectPackageManagerFallsBackToNpm(t *testing.T) {
	t.Parallel()
	name, path, err := detectPackageManager(t.TempDir(), func(file string) (string, error) {
		if file == "npm" {
			return "/usr/bin/npm", nil
		}
		return "", errors.New("missing")
	})
	if err != nil {
		t.Fatalf("detectPackageManager() error = %v", err)
	}
	if name != "npm" {
		t.Fatalf("manager = %q, want npm", name)
	}
	if path != "/usr/bin/npm" {
		t.Fatalf("path = %q, want /usr/bin/npm", path)
	}
}

func TestPlanFrontendUsesHostPortAndPnpm(t *testing.T) {
	t.Parallel()
	plan, err := planFrontend(t.TempDir(), func(file string) (string, error) {
		if file == "pnpm" {
			return "/usr/bin/pnpm", nil
		}
		return "", errors.New("missing")
	}, "0.0.0.0", 4173)
	if err != nil {
		t.Fatalf("planFrontend() error = %v", err)
	}
	if plan.Manager != "pnpm" {
		t.Fatalf("plan.Manager = %q, want pnpm", plan.Manager)
	}
	want := []string{"run", "dev", "--", "--host", "0.0.0.0", "--port", "4173"}
	if strings.Join(plan.Args, " ") != strings.Join(want, " ") {
		t.Fatalf("plan.Args = %v, want %v", plan.Args, want)
	}
}

func TestPlanFrontendFallsBackToNpm(t *testing.T) {
	t.Parallel()
	plan, err := planFrontend(t.TempDir(), func(file string) (string, error) {
		if file == "npm" {
			return "/usr/bin/npm", nil
		}
		return "", errors.New("missing")
	}, "127.0.0.1", 5173)
	if err != nil {
		t.Fatalf("planFrontend() error = %v", err)
	}
	if plan.Manager != "npm" {
		t.Fatalf("plan.Manager = %q, want npm", plan.Manager)
	}
	if plan.Path != "/usr/bin/npm" {
		t.Fatalf("plan.Path = %q, want /usr/bin/npm", plan.Path)
	}
	want := []string{"run", "dev", "--", "--host", "127.0.0.1", "--port", "5173"}
	if strings.Join(plan.Args, " ") != strings.Join(want, " ") {
		t.Fatalf("plan.Args = %v, want %v", plan.Args, want)
	}
}

func TestFrontendDepsInstalledRequiresDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if frontendDepsInstalled(dir) {
		t.Fatal("missing node_modules reported as installed")
	}
	writeFile(t, filepath.Join(dir, "node_modules"), "not a directory")
	if frontendDepsInstalled(dir) {
		t.Fatal("node_modules file reported as installed")
	}
	if err := os.Remove(filepath.Join(dir, "node_modules")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "node_modules"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if !frontendDepsInstalled(dir) {
		t.Fatal("node_modules directory reported as missing")
	}
}

func TestOverlayEnvReplacesParentKeys(t *testing.T) {
	t.Parallel()
	parent := []string{
		"PATH=/usr/bin",
		"GOMBIT_HTTP_ADDR=:8080",
		"VITE_API_URL=http://localhost:8080/api/v1",
		"GOMBIT_HTTP_ADDR=:8080",
		"HOME=/tmp",
	}
	got := overlayEnv(parent, "GOMBIT_HTTP_ADDR=:9090", "VITE_API_URL=")
	if values := envKeyValues(got, "GOMBIT_HTTP_ADDR"); len(values) != 1 || values[0] != ":9090" {
		t.Fatalf("GOMBIT_HTTP_ADDR = %v, want single :9090", values)
	}
	if values := envKeyValues(got, "VITE_API_URL"); len(values) != 1 || values[0] != "" {
		t.Fatalf("VITE_API_URL = %v, want single empty same-origin value", values)
	}
	if values := envKeyValues(got, "PATH"); len(values) != 1 || values[0] != "/usr/bin" {
		t.Fatalf("PATH = %v, want preserved /usr/bin", values)
	}
}

func TestChildEnvReplacesParentKeysOnProcSpec(t *testing.T) {
	t.Parallel()
	parent := []string{
		"GOMBIT_HTTP_ADDR=:8080",
		"VITE_API_URL=http://localhost:8080/api/v1",
		"GOMBIT_DEV_FRONTEND_HOST=0.0.0.0",
		"GOMBIT_API_PREFIX=/api/v1",
	}
	opts := Options{
		HTTPAddr:     ":9090",
		APIPrefix:    "/svc/v2",
		FrontendHost: "127.0.0.1",
		FrontendPort: 4173,
	}
	spec := ProcSpec{Name: "backend", Env: childEnv(parent, opts, "http://127.0.0.1:9090")}
	if values := envKeyValues(spec.Env, "GOMBIT_HTTP_ADDR"); len(values) != 1 || values[0] != opts.HTTPAddr {
		t.Fatalf("ProcSpec.Env GOMBIT_HTTP_ADDR = %v, want single %s", values, opts.HTTPAddr)
	}
	if values := envKeyValues(spec.Env, "VITE_API_URL"); len(values) != 1 || values[0] != "" {
		t.Fatalf("ProcSpec.Env VITE_API_URL = %v, want single empty same-origin value", values)
	}
	if values := envKeyValues(spec.Env, "GOMBIT_DEV_FRONTEND_HOST"); len(values) != 1 || values[0] != "127.0.0.1" {
		t.Fatalf("ProcSpec.Env GOMBIT_DEV_FRONTEND_HOST = %v, want 127.0.0.1", values)
	}
	if values := envKeyValues(spec.Env, "GOMBIT_API_PREFIX"); len(values) != 1 || values[0] != "/svc/v2" {
		t.Fatalf("ProcSpec.Env GOMBIT_API_PREFIX = %v, want single /svc/v2", values)
	}
}

func TestRunChildEnvReplacesParentHTTPAddr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh process-group shutdown")
	}

	t.Setenv("GOMBIT_HTTP_ADDR", ":8080")
	t.Setenv("VITE_API_URL", "http://localhost:8080/api/v1")

	workDir := writeDevApp(t)
	// readyCh (not startedCh+polling) is the synchronization point: onCmdReady
	// fires after runProcesses assigns cmd.Env, so the channel receive below
	// has a real happens-before edge to that write. Reading cmd.Env off a
	// racily-polled slice (the previous approach) trips -race even though the
	// values eventually observed are correct.
	readyCh := make(chan *exec.Cmd, 2)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	opts := Options{
		WorkDir:      workDir,
		HTTPAddr:     ":9090",
		FrontendPort: 15174,
		PollInterval: 50 * time.Millisecond,
		ShutdownWait: 2 * time.Second,
		Stdout:       ioDiscard{},
		Stderr:       ioDiscard{},
		LookPath: func(file string) (string, error) {
			switch file {
			case "go", "npm":
				return file, nil
			default:
				return "", errors.New("not found")
			}
		},
		Command: func(name string, args ...string) *exec.Cmd {
			return exec.Command("sh", "-c", "echo ready; trap 'exit 0' TERM; sleep 60")
		},
		HTTPGet: func(ctx context.Context, rawURL string) ([]byte, error) {
			return nil, errors.New("backend not ready")
		},
		onCmdReady: func(cmd *exec.Cmd) { readyCh <- cmd },
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, opts)
	}()

	captured := waitCmdsReady(t, readyCh, 2)
	for _, cmd := range captured {
		if values := envKeyValues(cmd.Env, "GOMBIT_HTTP_ADDR"); len(values) != 1 || values[0] != ":9090" {
			t.Fatalf("child Env GOMBIT_HTTP_ADDR = %v, want single :9090", values)
		}
		if values := envKeyValues(cmd.Env, "VITE_API_URL"); len(values) != 1 || values[0] != "" {
			t.Fatalf("child Env VITE_API_URL = %v, want single empty same-origin value", values)
		}
		if values := envKeyValues(cmd.Env, "GOMBIT_API_PREFIX"); len(values) != 1 || values[0] != "/api/v1" {
			t.Fatalf("child Env GOMBIT_API_PREFIX = %v, want single /api/v1", values)
		}
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil after cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after cancel")
	}
}

func envKeyValues(env []string, key string) []string {
	var values []string
	for _, entry := range env {
		k, v, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if k == key || (runtime.GOOS == "windows" && strings.EqualFold(k, key)) {
			values = append(values, v)
		}
	}
	return values
}
