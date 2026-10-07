package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// TestWriteRefusesWithoutAtomicRename: on a volume without POSIX rename
// semantics (FAT, exFAT, Windows before 10 1709; played here by the error
// such a volume returns) Write refuses rather than replace without the
// guarantee: ErrNoAtomicRename, the target untouched, no temp file, and
// not committed.
func TestWriteRefusesWithoutAtomicRename(t *testing.T) {
	for _, volumeErr := range []error{windows.ERROR_NOT_SUPPORTED, windows.ERROR_INVALID_PARAMETER, windows.ERROR_INVALID_FUNCTION} {
		dir := t.TempDir()
		path := filepath.Join(dir, "gombit.yaml")
		if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		prev := renameHook
		renameHook = func(src, dst string, _ uint32) error {
			return &os.LinkError{Op: "rename", Old: src, New: dst, Err: volumeErr}
		}
		err := Write(path, []byte("new\n"), 0o600)
		renameHook = prev
		if !errors.Is(err, ErrNoAtomicRename) || !errors.Is(err, errors.ErrUnsupported) || Committed(err) {
			t.Fatalf("%v: Write = %v; want an uncommitted ErrNoAtomicRename", volumeErr, err)
		}
		if got := readFile(t, path); got != "old\n" {
			t.Fatalf("%v: content = %q; want the target untouched", volumeErr, got)
		}
		noTemp(t, dir)

		// AllowNonAtomic replaces it anyway, with os.Rename.
		renameHook = func(src, dst string, _ uint32) error {
			return &os.LinkError{Op: "rename", Old: src, New: dst, Err: volumeErr}
		}
		err = Write(path, []byte("new\n"), 0o600, AllowNonAtomic())
		renameHook = prev
		if err != nil {
			t.Fatalf("%v: Write with AllowNonAtomic = %v", volumeErr, err)
		}
		if got := readFile(t, path); got != "new\n" {
			t.Fatalf("%v: content = %q; want the new content", volumeErr, got)
		}
		noTemp(t, dir)
	}
}
