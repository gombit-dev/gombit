package resourcegen

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/gombit-dev/gombit/internal/atomicfile"
)

// TestApplyWritesRollsBackANotDurableWrite: a write whose directory sync
// fails after the rename (atomicfile.ErrNotDurable) has changed the tree.
// The transaction must undo it with the rest, so the tree is left as it was
// found: the existing file restored, the new file and its directory gone.
func TestApplyWritesRollsBackANotDurableWrite(t *testing.T) {
	for _, failOn := range []int64{1, 2} { // the failing write: the first or a later one
		dir := t.TempDir()
		existing := filepath.Join(dir, "cmd", "server", "main.go")
		if err := os.MkdirAll(filepath.Dir(existing), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(existing, []byte("package main // before\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var syncs atomic.Int64
		restore := atomicfile.SetSyncDirForTest(func(string) error {
			if syncs.Add(1) == failOn {
				return errors.New("injected: fsync failed")
			}
			return nil
		})
		err := applyWrites(Options{WorkDir: dir, Stdout: io.Discard}, []plannedFile{
			{relPath: "cmd/server/main.go", display: "cmd/server/main.go", content: []byte("package main // after\n"), action: "update", write: true},
			{relPath: "internal/book/book.go", display: "internal/book/book.go", content: []byte("package book\n"), action: "create", write: true},
		})
		restore()
		if !errors.Is(err, atomicfile.ErrNotDurable) {
			t.Fatalf("failing write %d: applyWrites = %v; want the ErrNotDurable failure", failOn, err)
		}
		if got, _ := os.ReadFile(existing); string(got) != "package main // before\n" { // #nosec G304 -- under t.TempDir()
			t.Fatalf("failing write %d: cmd/server/main.go = %q; want it restored", failOn, got)
		}
		if _, err := os.Stat(filepath.Join(dir, "internal")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failing write %d: internal/ is left behind (%v)", failOn, err)
		}
	}
}
