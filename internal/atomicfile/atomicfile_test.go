package atomicfile

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- under t.TempDir()
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func noTemp(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".gombit-tmp-") {
			t.Fatalf("a temp file was left behind: %s", e.Name())
		}
	}
}

// TestWrite: a new file is created, an existing one replaced whole, with
// the mode asked for, and no temp file is left.
func TestWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gombit.yaml")
	if err := Write(path, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "two\n" {
		t.Fatalf("content = %q, want the new content", got)
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v, %v; want 0600", info.Mode().Perm(), err)
		}
	}
	noTemp(t, dir)
}

// TestWriteRelativePath: a relative target is replaced in the working
// directory (on Windows the rename takes an absolute name).
func TestWriteRelativePath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile("rel.yaml", []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Write("rel.yaml", []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, "rel.yaml")); got != "new\n" {
		t.Fatalf("content = %q, want the new content", got)
	}
	noTemp(t, dir)
}

// TestWriteOverAnOpenFile: a reader holding the target keeps the version it
// opened, and the name gets the new content. On Windows that needs the
// POSIX-semantics rename (os.Rename fails while the target is open with
// delete sharing; see replace_windows.go).
func TestWriteOverAnOpenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gombit.yaml")
	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := openShared(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if err := Write(path, []byte("new\n"), 0o600); err != nil {
		t.Fatalf("Write over an open file: %v", err)
	}
	if got := readFile(t, path); got != "new\n" {
		t.Fatalf("content = %q, want the new content", got)
	}
	buf := make([]byte, 16)
	n, _ := r.Read(buf)
	if string(buf[:n]) != "old\n" {
		t.Fatalf("the open reader read %q, want the version it opened", buf[:n])
	}
	noTemp(t, dir)
}

// TestWriteRefusedLeavesTheTarget: a write that cannot complete leaves the
// target as it was and no temp file.
func TestWriteRefusedLeavesTheTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gombit.yaml")
	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A directory where the target should be: the rename cannot replace it.
	blocked := filepath.Join(dir, "blocked")
	if err := os.Mkdir(blocked, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "x"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Write(blocked, []byte("new\n"), 0o600); err == nil {
		t.Fatal("Write over a non-empty directory succeeded")
	}
	if got := readFile(t, filepath.Join(blocked, "x")); got != "x" {
		t.Fatalf("the directory's content changed: %q", got)
	}
	if got := readFile(t, path); got != "old\n" {
		t.Fatalf("an unrelated file changed: %q", got)
	}
	noTemp(t, dir)
}

// TestSyncDir: the directory sync that makes a rename durable on Unix
// succeeds on a directory and reports one it cannot open.
func TestSyncDir(t *testing.T) {
	if err := syncDir(t.TempDir()); err != nil {
		t.Fatalf("syncDir of a directory = %v", err)
	}
	if runtime.GOOS != "windows" {
		if err := syncDir(filepath.Join(t.TempDir(), "missing")); err == nil {
			t.Fatal("syncDir of a missing directory succeeded")
		}
	}
}
