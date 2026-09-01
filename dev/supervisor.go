package dev

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"sync"
	"time"
)

// ProcSpec is one supervised child process.
type ProcSpec struct {
	Name string
	Dir  string
	Env  []string
	Path string
	Args []string
}

// onCmdReady, when non-nil, is called synchronously right after a spec's
// Env/Dir/Stdout/Stderr are assigned to its *exec.Cmd, before Start. It is
// nil on every real `gombit dev` invocation; tests use it to observe a
// child's final Env without racing runProcesses's unsynchronized field
// writes (see dev/run_test.go TestRunChildEnvReplacesParentHTTPAddr).
func runProcesses(ctx context.Context, specs []ProcSpec, stdout, stderr io.Writer, command CommandFunc, wait time.Duration, onCmdReady func(*exec.Cmd)) error {
	if command == nil {
		command = exec.Command
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var mu sync.Mutex
	running := make([]*exec.Cmd, 0, len(specs))
	var wg sync.WaitGroup
	errCh := make(chan error, len(specs))

	for _, spec := range specs {
		if err := runCtx.Err(); err != nil {
			break
		}
		cmd := command(spec.Path, spec.Args...) //nolint:gosec // path/args come from LookPath and fixed flags
		if cmd == nil {
			cancel()
			_ = waitAll(&wg, &mu, running, wait)
			return fmt.Errorf("dev: nil command for %s", spec.Name)
		}
		cmd.Dir = spec.Dir
		if len(spec.Env) > 0 {
			cmd.Env = spec.Env
		}
		if cmd.Stdout == nil {
			cmd.Stdout = stdout
		}
		if cmd.Stderr == nil {
			cmd.Stderr = stderr
		}
		if onCmdReady != nil {
			onCmdReady(cmd)
		}
		prepareProcessGroup(cmd)
		if err := cmd.Start(); err != nil {
			cancel()
			_ = waitAll(&wg, &mu, running, wait)
			return fmt.Errorf("dev: start %s: %w", spec.Name, err)
		}
		mu.Lock()
		running = append(running, cmd)
		mu.Unlock()
		wg.Add(1)
		go func(name string, cmd *exec.Cmd) {
			defer wg.Done()
			err := cmd.Wait()
			if runCtx.Err() != nil {
				return
			}
			if err != nil {
				errCh <- fmt.Errorf("dev: %s: %w", name, err)
			} else {
				errCh <- fmt.Errorf("dev: %s exited unexpectedly", name)
			}
			cancel()
		}(spec.Name, cmd)
	}

	select {
	case <-runCtx.Done():
	case err := <-errCh:
		cancel()
		_ = waitAll(&wg, &mu, running, wait)
		return err
	}

	if err := waitAll(&wg, &mu, running, wait); err != nil {
		return err
	}
	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}

func waitAll(wg *sync.WaitGroup, mu *sync.Mutex, running []*exec.Cmd, wait time.Duration) error {
	mu.Lock()
	cmds := append([]*exec.Cmd(nil), running...)
	mu.Unlock()
	for _, cmd := range cmds {
		_ = signalProcessGroup(cmd)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-time.After(wait):
		mu.Lock()
		for _, cmd := range running {
			_ = killProcessGroup(cmd)
		}
		mu.Unlock()
		select {
		case <-done:
			return nil
		case <-time.After(wait):
			return fmt.Errorf("dev: timed out waiting for child processes to exit")
		}
	}
}

// windowsTaskkillArgs builds `taskkill` flags that terminate a PID and its
// descendants. Used by the Windows supervisor; tested on all platforms.
func windowsTaskkillArgs(pid int, force bool) []string {
	args := []string{"/T"}
	if force {
		args = append(args, "/F")
	}
	return append(args, "/PID", strconv.Itoa(pid))
}

func runOne(ctx context.Context, spec ProcSpec, stdout, stderr io.Writer, command CommandFunc) error {
	if command == nil {
		command = exec.Command
	}
	cmd := command(spec.Path, spec.Args...) //nolint:gosec // path/args come from LookPath and fixed flags
	if cmd == nil {
		return fmt.Errorf("dev: nil command for %s", spec.Name)
	}
	cmd.Dir = spec.Dir
	if len(spec.Env) > 0 {
		cmd.Env = spec.Env
	}
	if cmd.Stdout == nil {
		cmd.Stdout = stdout
	}
	if cmd.Stderr == nil {
		cmd.Stderr = stderr
	}
	prepareProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("dev: start %s: %w", spec.Name, err)
	}
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = signalProcessGroup(cmd)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = killProcessGroup(cmd)
			select {
			case <-done:
			case <-time.After(2 * time.Second):
			}
		}
		return ctx.Err()
	}
}
