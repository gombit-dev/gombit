package dev

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const (
	errMissingFrontend = "dev: missing frontend/package.json; gombit dev requires the Vite frontend (backend-only is not supported). Re-run gombit new or add a frontend/package.json"
	errMissingServer   = "dev: missing cmd/server; run gombit dev from a Gombit application directory"
)

// Run starts the backend, Vite frontend, and OpenAPI client watcher until ctx is cancelled.
func Run(ctx context.Context, opts Options) error {
	if ctx == nil {
		return errors.New("dev: nil context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := opts.normalize(); err != nil {
		return err
	}
	if err := opts.validate(); err != nil {
		return err
	}

	frontendPkg := filepath.Join(opts.WorkDir, "frontend", "package.json")
	if _, err := os.Stat(frontendPkg); err != nil {
		if os.IsNotExist(err) {
			return errors.New(errMissingFrontend)
		}
		return fmt.Errorf("dev: stat frontend/package.json: %w", err)
	}
	serverDir := filepath.Join(opts.WorkDir, "cmd", "server")
	if info, err := os.Stat(serverDir); err != nil || !info.IsDir() {
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("dev: stat cmd/server: %w", err)
		}
		return errors.New(errMissingServer)
	}

	backend, err := planBackend(opts.WorkDir, opts.LookPath)
	if err != nil {
		return err
	}
	frontend, err := planFrontend(opts.WorkDir, opts.LookPath, opts.FrontendHost, opts.FrontendPort)
	if err != nil {
		return err
	}

	services := Services{
		Backend:  originFromAddr(opts.HTTPAddr),
		Frontend: frontendOrigin(opts.FrontendHost, opts.FrontendPort),
		Admin:    opts.AdminURL,
	}
	if _, err := fmt.Fprint(opts.Stdout, FormatServiceTable(services)); err != nil {
		return err
	}
	if backend.Hint != "" {
		if _, err := fmt.Fprintln(opts.Stderr, backend.Hint); err != nil {
			return err
		}
	}

	frontendDir := filepath.Join(opts.WorkDir, "frontend")
	if !frontendDepsInstalled(frontendDir) {
		install := ProcSpec{
			Name: "frontend-install",
			Dir:  frontendDir,
			Path: frontend.Path,
			Args: []string{"install"},
			Env:  os.Environ(),
		}
		if _, err := fmt.Fprintf(opts.Stdout, "installing frontend dependencies with %s\n", frontend.Manager); err != nil {
			return err
		}
		if err := runOne(ctx, install, opts.Stdout, opts.Stderr, opts.Command); err != nil {
			return fmt.Errorf("dev: %s install: %w", frontend.Manager, err)
		}
	}

	env := childEnv(os.Environ(), opts, services.Backend)

	procs := []ProcSpec{
		{
			Name: "backend",
			Dir:  opts.WorkDir,
			Path: backend.Path,
			Args: backend.Args,
			Env:  env,
		},
		{
			Name: "frontend",
			Dir:  frontendDir,
			Path: frontend.Path,
			Args: frontend.Args,
			Env:  env,
		},
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		_ = watchOpenAPI(runCtx, opts, services.Backend+"/openapi.json")
	}()

	err = runProcesses(runCtx, procs, opts.Stdout, opts.Stderr, opts.Command, opts.ShutdownWait, opts.onCmdReady)
	cancel()
	<-watchDone
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func frontendDepsInstalled(frontendDir string) bool {
	info, err := os.Stat(filepath.Join(frontendDir, "node_modules"))
	return err == nil && info.IsDir()
}

// childEnv copies parent then replaces the keys gombit dev owns so Linux
// first-wins getenv cannot keep a parent GOMBIT_HTTP_ADDR / VITE_API_URL /
// GOMBIT_API_PREFIX.
func childEnv(parent []string, opts Options, backendOrigin string) []string {
	prefix := strings.TrimSuffix(strings.TrimSpace(opts.APIPrefix), "/")
	if prefix == "" {
		prefix = "/api/v1"
	}
	return overlayEnv(parent,
		"GOMBIT_HTTP_ADDR="+opts.HTTPAddr,
		"GOMBIT_API_PREFIX="+prefix,
		"GOMBIT_DEV_BACKEND="+backendOrigin,
		"GOMBIT_DEV_FRONTEND_HOST="+opts.FrontendHost,
		"GOMBIT_DEV_FRONTEND_PORT="+strconv.Itoa(opts.FrontendPort),
		"VITE_API_URL=",
	)
}

func overlayEnv(parent []string, pairs ...string) []string {
	drop := make(map[string]struct{}, len(pairs))
	for _, pair := range pairs {
		drop[envKey(pair)] = struct{}{}
	}
	out := make([]string, 0, len(parent)+len(pairs))
	for _, entry := range parent {
		if _, skip := drop[envKey(entry)]; skip {
			continue
		}
		out = append(out, entry)
	}
	return append(out, pairs...)
}

func envKey(entry string) string {
	key, _, _ := strings.Cut(entry, "=")
	if runtime.GOOS == "windows" {
		return strings.ToUpper(key)
	}
	return key
}
