//go:build !windows

package dev

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// TestRunProcessReportsHowTheChildStopped: a stop the caller asked for is
// success only when the child then exits cleanly.
func TestRunProcessReportsHowTheChildStopped(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		killAfter    time.Duration
		wantErr      string
	}{
		{"clean exit on SIGTERM", `trap 'exit 0' TERM; while :; do sleep 0.05; done`, 5 * time.Second, ""},
		{"failed shutdown", `trap 'exit 3' TERM; while :; do sleep 0.05; done`, 5 * time.Second, "exit status 3"},
		{"ignores SIGTERM", `trap '' TERM; while :; do sleep 0.05; done`, 200 * time.Millisecond, "was killed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				done <- RunProcess(ctx, ProcSpec{Name: "worker", Dir: ".", Path: "/bin/sh", Args: []string{"-c", tc.script}}, io.Discard, io.Discard, tc.killAfter)
			}()
			time.Sleep(200 * time.Millisecond) // the trap is set
			cancel()
			err := <-done
			if tc.wantErr == "" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("RunProcess = %v, want context.Canceled for a clean stop", err)
				}
				return
			}
			if err == nil || errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("RunProcess = %v, want the child's failure (%s)", err, tc.wantErr)
			}
		})
	}
}
