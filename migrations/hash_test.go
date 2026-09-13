package migrations

import (
	"context"
	"io"
	"strings"
	"testing"
)

type recordRunner struct {
	calls [][]string
	err   error
}

func (r *recordRunner) Run(_ context.Context, _ string, _ string, args []string, _ io.Writer, _ io.Writer) error {
	r.calls = append(r.calls, append([]string{}, args...))
	return r.err
}

func argsContain(args []string, want ...string) bool {
	joined := " " + strings.Join(args, " ") + " "
	for _, w := range want {
		if !strings.Contains(joined, " "+w+" ") {
			return false
		}
	}
	return true
}

func TestHashRunsAtlasMigrateHash(t *testing.T) {
	r := &recordRunner{}
	err := Hash(context.Background(), ApplyOptions{
		WorkDir:      t.TempDir(),
		MigrationDir: "database/migrations",
		Stdout:       io.Discard,
		Stderr:       io.Discard,
		runner:       r,
	})
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if len(r.calls) != 1 {
		t.Fatalf("atlas calls = %d, want 1", len(r.calls))
	}
	call := r.calls[0]
	if len(call) < 2 || call[0] != "migrate" || call[1] != "hash" {
		t.Fatalf("args = %v, want [migrate hash ...]", call)
	}
	if !argsContain(call, "--dir") {
		t.Fatalf("args = %v, want a --dir", call)
	}
}
