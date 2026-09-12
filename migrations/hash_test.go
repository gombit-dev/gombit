package migrations

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/config"
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

func TestLintSQLiteUsesInMemoryDevURL(t *testing.T) {
	r := &recordRunner{}
	err := Lint(context.Background(), LintOptions{
		ApplyOptions: ApplyOptions{
			WorkDir:  t.TempDir(),
			Database: config.DatabaseConfig{Driver: config.DatabaseDriverSQLite},
			Stdout:   io.Discard,
			Stderr:   io.Discard,
			runner:   r,
		},
	})
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if len(r.calls) != 1 {
		t.Fatalf("atlas calls = %d, want 1", len(r.calls))
	}
	call := r.calls[0]
	if len(call) < 2 || call[0] != "migrate" || call[1] != "lint" {
		t.Fatalf("args = %v, want [migrate lint ...]", call)
	}
	if !argsContain(call, "--dev-url", "sqlite://file?mode=memory", "--latest", "1") {
		t.Fatalf("args = %v, want sqlite in-memory dev-url + --latest 1", call)
	}
}

func TestLintPostgresRequiresDevURL(t *testing.T) {
	r := &recordRunner{}
	err := Lint(context.Background(), LintOptions{
		ApplyOptions: ApplyOptions{
			WorkDir:  t.TempDir(),
			Database: config.DatabaseConfig{Driver: config.DatabaseDriverPostgres},
			Stdout:   io.Discard,
			Stderr:   io.Discard,
			runner:   r,
		},
	})
	if err == nil {
		t.Fatal("want an error when no dev-url is given for postgres")
	}
	if len(r.calls) != 0 {
		t.Fatalf("atlas must not run without a dev-url; calls = %d", len(r.calls))
	}
}

func TestLintCustomDevURLAndLatest(t *testing.T) {
	r := &recordRunner{}
	err := Lint(context.Background(), LintOptions{
		ApplyOptions: ApplyOptions{
			WorkDir:  t.TempDir(),
			Database: config.DatabaseConfig{Driver: config.DatabaseDriverPostgres},
			Stdout:   io.Discard,
			Stderr:   io.Discard,
			runner:   r,
		},
		DevURL: "docker://postgres/16/dev?search_path=public",
		Latest: 3,
	})
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	call := r.calls[0]
	if !argsContain(call, "--dev-url", "docker://postgres/16/dev?search_path=public", "--latest", "3") {
		t.Fatalf("args = %v, want custom dev-url + --latest 3", call)
	}
}
