package local_test

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/storage"
	"github.com/gombit-dev/gombit/storage/local"
	"github.com/gombit-dev/gombit/storage/storagetest"
)

func newStore(t *testing.T) *local.Store {
	t.Helper()
	s, err := local.New(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestConformance(t *testing.T) {
	storagetest.Run(t, func(t *testing.T) storage.Storage { return newStore(t) })
}

func TestNewNeedsARoot(t *testing.T) {
	if _, err := local.New(""); err == nil {
		t.Fatal("New(\"\") succeeded")
	}
}

// TestNothingIsCreatedUntilPut: opening a store (as every app does at
// start) creates no directory; the first Put does.
func TestNothingIsCreatedUntilPut(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.Stat(ctx, "k"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Stat = %v", err)
	}
	if err := s.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete = %v", err)
	}
	if _, err := os.Stat(s.Root()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the root exists before any Put (%v)", err)
	}
	if _, err := s.Put(ctx, "k", strings.NewReader("x"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Root()); err != nil {
		t.Fatalf("the root was not created by Put: %v", err)
	}
}

// TestObjectsStayUnderTheRoot: whatever the key, every file the store writes
// is under its root.
func TestObjectsStayUnderTheRoot(t *testing.T) {
	parent := t.TempDir()
	s, err := local.New(filepath.Join(parent, "root"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, key := range []string{"a", "a/b", "CON/nul.txt", "c:/x", "..hidden/x", strings.Repeat("s", 400)} {
		if _, err := s.Put(ctx, key, strings.NewReader(key), storage.PutOptions{}); err != nil {
			t.Fatalf("Put(%q) = %v", key, err)
		}
	}
	for _, key := range []string{"../outside", "/etc/passwd", `..\outside`} {
		if _, err := s.Put(ctx, key, strings.NewReader("x"), storage.PutOptions{}); !errors.Is(err, storage.ErrInvalidKey) {
			t.Fatalf("Put(%q) = %v, want storage.ErrInvalidKey", key, err)
		}
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "root" {
		t.Fatalf("the parent of the root holds %v, want only the root", entries)
	}
}

// TestPutStreams: a 64 MiB Put allocates a small, fixed amount, not the
// object: the bytes go from the reader to disk in pieces.
func TestPutStreams(t *testing.T) {
	s := newStore(t)
	const size = 64 << 20
	src := io.LimitReader(zeros{}, size)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	info, err := s.Put(context.Background(), "big", src, storage.PutOptions{Size: size})
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != size {
		t.Fatalf("size = %d", info.Size)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 4<<20 {
		t.Fatalf("a %d MiB Put allocated %d KiB: it buffers the object", size>>20, allocated>>10)
	}
	body, _, err := s.Open(context.Background(), "big")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = body.Close() }()
	runtime.ReadMemStats(&before)
	n, err := io.CopyBuffer(io.Discard, body, make([]byte, 32<<10))
	runtime.ReadMemStats(&after)
	if err != nil || n != size {
		t.Fatalf("read %d, %v", n, err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 4<<20 {
		t.Fatalf("reading %d MiB allocated %d KiB: Open buffers the object", size>>20, allocated>>10)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// TestFailedPutLeavesNothingBehind: a Put that fails removes its temporary
// file.
func TestFailedPutLeavesNothingBehind(t *testing.T) {
	s := newStore(t)
	boom := errors.New("boom")
	_, err := s.Put(context.Background(), "k", io.MultiReader(strings.NewReader("partial"), errReader{boom}), storage.PutOptions{})
	if !errors.Is(err, boom) {
		t.Fatalf("Put = %v, want the reader's error", err)
	}
	_, err = s.Put(context.Background(), "k", strings.NewReader("123"), storage.PutOptions{Size: 5})
	if !errors.Is(err, storage.ErrSizeMismatch) {
		t.Fatalf("Put = %v, want storage.ErrSizeMismatch", err)
	}
	tmp, err := os.ReadDir(filepath.Join(s.Root(), "tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tmp) != 0 {
		t.Fatalf("failed Puts left %d temporary files", len(tmp))
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// objectFile finds the single object file under the root.
func objectFile(t *testing.T, s *local.Store) string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(filepath.Join(s.Root(), "objects"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			found = append(found, p)
		}
		return err
	})
	if err != nil || len(found) != 1 {
		t.Fatalf("object files = %v, %v", found, err)
	}
	return found[0]
}

// TestCorruptFilesAreErrors: a damaged object file is reported, not
// served or panicked on.
func TestCorruptFilesAreErrors(t *testing.T) {
	ctx := context.Background()
	for name, damage := range map[string]func(path string) error{
		"truncated": func(p string) error { return os.Truncate(p, 3) },
		"no trailer": func(p string) error {
			return os.WriteFile(p, []byte("just some bytes, no trailer at all"), 0o600)
		},
		"huge header length": func(p string) error {
			b, err := os.ReadFile(p) // #nosec G304 -- the test's own object file
			if err != nil {
				return err
			}
			copy(b[len(b)-12:], []byte{0xff, 0xff, 0xff, 0xff})
			return os.WriteFile(p, b, 0o600) // #nosec G703 -- the test's own object file
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := newStore(t)
			if _, err := s.Put(ctx, "k", strings.NewReader("hello"), storage.PutOptions{}); err != nil {
				t.Fatal(err)
			}
			if err := damage(objectFile(t, s)); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.Open(ctx, "k"); err == nil || errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("Open of a damaged file = %v, want an error that is not ErrNotFound", err)
			}
			if _, err := s.Stat(ctx, "k"); err == nil {
				t.Fatal("Stat of a damaged file succeeded")
			}
		})
	}
}

// TestSharedRoot: two stores on one root (two processes, say) see each
// other's objects.
func TestSharedRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	a, _ := local.New(root)
	b, _ := local.New(root)
	ctx := context.Background()
	if _, err := a.Put(ctx, "shared", strings.NewReader("from a"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	body, _, err := b.Open(ctx, "shared")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = body.Close() }()
	got, _ := io.ReadAll(body)
	if string(got) != "from a" {
		t.Fatalf("b read %q", got)
	}
}

func TestClockAndETag(t *testing.T) {
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	s, err := local.New(filepath.Join(t.TempDir(), "root"), local.WithClock(func() time.Time { return at }))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	info, err := s.Put(ctx, "k", strings.NewReader("abc"), storage.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	const sha256abc = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if !info.ModTime.Equal(at) || info.ETag != sha256abc {
		t.Fatalf("info = %+v", info)
	}
	stat, err := s.Stat(ctx, "k")
	if err != nil || !stat.ModTime.Equal(at) || stat.ETag != sha256abc {
		t.Fatalf("Stat = %+v, %v", stat, err)
	}
}

// TestSweepsAbandonedTempFiles: a store's first Put removes temporary files
// nothing has written to for an hour (left by a process killed mid-Put),
// and keeps fresh ones (another process's Put in progress) and anything
// that is not a temporary file.
func TestSweepsAbandonedTempFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	tmp := filepath.Join(root, "tmp")
	if err := os.MkdirAll(tmp, 0o750); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	for name, mtime := range map[string]time.Time{"put-abandoned": old, "put-in-progress": time.Now(), "notes.txt": old} {
		p := filepath.Join(tmp, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	s, _ := local.New(root)
	if _, err := s.Put(context.Background(), "k", strings.NewReader("x"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	left := map[string]bool{}
	entries, _ := os.ReadDir(tmp)
	for _, e := range entries {
		left[e.Name()] = true
	}
	if left["put-abandoned"] || !left["put-in-progress"] || !left["notes.txt"] {
		t.Fatalf("after the sweep tmp holds %v; want the abandoned temp file gone and the others kept", left)
	}
}

// rewriteHeader replaces the object file's trailer header with js.
func rewriteHeader(t *testing.T, s *local.Store, js string) {
	t.Helper()
	p := objectFile(t, s)
	b, err := os.ReadFile(p) // #nosec G304 -- the test's own object file
	if err != nil {
		t.Fatal(err)
	}
	hlen := int(binary.BigEndian.Uint32(b[len(b)-12:]))
	data := b[:len(b)-12-hlen]
	out := append(append([]byte{}, data...), js...)
	out = binary.BigEndian.AppendUint32(out, uint32(len(js))) // #nosec G115 -- a short test header
	out = append(out, "GOMBITOB"...)
	if err := os.WriteFile(p, out, 0o600); err != nil { // #nosec G703 -- the test's own object file
		t.Fatal(err)
	}
}

// TestTrailerIsForwardCompatible: a header field this version does not
// know (one a later version added) is ignored, so versions can share a
// root; a different format version is refused, not misread.
func TestTrailerIsForwardCompatible(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if _, err := s.Put(ctx, "k", strings.NewReader("hello"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	rewriteHeader(t, s, `{"v":1,"key":"k","content_type":"text/plain","mod_time":"2026-09-29T12:00:00Z","etag":"e","visibility":"private","added_later":{"x":1}}`)
	body, info, err := s.Open(ctx, "k")
	if err != nil {
		t.Fatalf("Open with an unknown header field = %v, want it ignored", err)
	}
	got, _ := io.ReadAll(body)
	_ = body.Close()
	if string(got) != "hello" || info.ContentType != "text/plain" {
		t.Fatalf("read %q as %q", got, info.ContentType)
	}
	rewriteHeader(t, s, `{"v":2,"key":"k","content_type":"text/plain","mod_time":"2026-09-29T12:00:00Z","etag":"e"}`)
	if _, err := s.Stat(ctx, "k"); err == nil || !strings.Contains(err.Error(), "format v2") {
		t.Fatalf("Stat of a v2 object = %v, want a format error", err)
	}
}

func TestFirstPutHook(t *testing.T) {
	var calls []string
	s, _ := local.New(filepath.Join(t.TempDir(), "root"), local.WithFirstPut(func(root string) { calls = append(calls, root) }))
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := s.Put(ctx, "k", strings.NewReader("x"), storage.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if len(calls) != 1 || calls[0] != s.Root() {
		t.Fatalf("hook calls = %v, want one with the root", calls)
	}
}
