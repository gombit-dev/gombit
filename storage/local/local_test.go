package local_test

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/storage"
	"github.com/gombit-dev/gombit/storage/local"
	"github.com/gombit-dev/gombit/storage/presign"
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
	for _, key := range []string{"a", "a/b", "CON/nul.txt", "c:/x", "..hidden/x", strings.Repeat("s", storage.MaxSegmentBytes)} {
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
	info, err := s.Put(context.Background(), "big", src, storage.PutOptions{Size: storage.KnownSize(size)})
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
	_, err = s.Put(context.Background(), "k", strings.NewReader("123"), storage.PutOptions{Size: storage.KnownSize(5)})
	if !errors.Is(err, storage.ErrSizeMismatch) {
		t.Fatalf("Put = %v, want storage.ErrSizeMismatch", err)
	}
	if left := tempFiles(t, s.Root()); len(left) != 0 {
		t.Fatalf("failed Puts left temporary files %v", left)
	}
}

// tempFiles lists the temporary files in every work directory under root.
func tempFiles(t *testing.T, root string) []string {
	t.Helper()
	found, err := filepath.Glob(filepath.Join(root, "tmp", "w-*", "put-*"))
	if err != nil {
		t.Fatal(err)
	}
	return found
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

// TestSweepSparesLiveStores: a store's first Put removes the work
// directories of stores that are gone (a process killed mid-Put) and keeps
// a live store's in the same process, however long its Put has been
// waiting on its source: a Put that has written part of its object, then
// blocks for longer than any age limit, still completes after another
// store's sweep. TestSweepSparesOtherProcesses covers stores in other
// processes, which only their lock speaks for.
func TestSweepSparesLiveStores(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	ctx := context.Background()
	live, _ := local.New(root)
	if _, err := live.Put(ctx, "warm", strings.NewReader("x"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	src, release := blockedSource("written before the pause, ")
	done := make(chan error, 1)
	go func() {
		_, err := live.Put(ctx, "paused", src, storage.PutOptions{})
		done <- err
	}()
	var inFlight []string
	for deadline := time.Now().Add(10 * time.Second); len(inFlight) == 0; {
		if time.Now().After(deadline) {
			t.Fatal("the paused Put never created its temporary file")
		}
		time.Sleep(time.Millisecond)
		inFlight = tempFiles(t, root)
	}
	tmp := filepath.Join(root, "tmp")
	long := time.Now().Add(-48 * time.Hour)
	age := func(p string) {
		t.Helper()
		if err := os.Chtimes(p, long, long); err != nil {
			t.Fatal(err)
		}
	}
	// The live store's files look untouched for two days.
	age(inFlight[0])
	age(filepath.Dir(inFlight[0]))
	// A store that died mid-Put: its owner file exists but nothing holds it.
	dead := filepath.Join(tmp, "w-dead")
	for _, name := range []string{"owner", "put-1"} {
		if err := os.MkdirAll(dead, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dead, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	age(dead)
	// A store that died before creating its owner file, one that is
	// creating it right now (neither provably gone, so both kept), and
	// something that is not a work directory.
	for _, dir := range []string{"w-unowned", "w-starting", "notes"} {
		if err := os.Mkdir(filepath.Join(tmp, dir), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	age(filepath.Join(tmp, "w-unowned"))
	age(filepath.Join(tmp, "notes"))

	next, _ := local.New(root)
	if _, err := next.Put(ctx, "k", strings.NewReader("x"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]bool{filepath.Dir(inFlight[0]): true, dead: false, filepath.Join(tmp, "w-unowned"): true, filepath.Join(tmp, "w-starting"): true, filepath.Join(tmp, "notes"): true} {
		if _, err := os.Stat(dir); (err == nil) != want {
			t.Errorf("after the sweep %s exists = %v, want %v", filepath.Base(dir), err == nil, want)
		}
	}
	if _, err := os.Stat(inFlight[0]); err != nil {
		t.Fatalf("the sweep removed the live store's temporary file: %v", err)
	}
	release("and after it")
	if err := <-done; err != nil {
		t.Fatalf("the paused Put = %v, want success after another store's sweep", err)
	}
	if got := readAll(t, live, "paused"); got != "written before the pause, and after it" {
		t.Fatalf("the paused Put stored %q", got)
	}
}

// helperRootEnv makes the test binary a helper process: a store on that
// root that Puts "helper" from stdin (see TestSweepSparesOtherProcesses).
const helperRootEnv = "GOMBIT_LOCAL_TEST_HELPER_ROOT"

func TestHelperProcess(t *testing.T) {
	root := os.Getenv(helperRootEnv)
	if root == "" {
		t.Skip("run as a helper process only")
	}
	s, err := local.New(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(context.Background(), "helper", os.Stdin, storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
}

// startHelper starts a helper process whose Put has written first and is
// blocked on its stdin, and returns it with the temporary file it writes.
func startHelper(t *testing.T, root, first string) (*exec.Cmd, io.WriteCloser, string) {
	t.Helper()
	before := tempFiles(t, root)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run=^TestHelperProcess$") // #nosec G204 -- the test binary itself
	cmd.Env = append(os.Environ(), helperRootEnv+"="+root)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if _, err := io.WriteString(stdin, first); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		for _, f := range tempFiles(t, root) {
			if !slices.Contains(before, f) {
				if info, err := os.Stat(f); err == nil && info.Size() == int64(len(first)) {
					return cmd, stdin, f
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the helper process never wrote its temporary file")
		}
	}
}

// TestSweepSparesOtherProcesses: a store in another process that is
// blocked mid-Put, its files untouched for two days, keeps its work
// directory through a sweep and completes its Put; once that process is
// killed, the next sweep removes what it left.
func TestSweepSparesOtherProcesses(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	ctx := context.Background()
	long := time.Now().Add(-48 * time.Hour)
	age := func(paths ...string) {
		t.Helper()
		for _, p := range paths {
			if err := os.Chtimes(p, long, long); err != nil {
				t.Fatal(err)
			}
		}
	}
	sweep := func() {
		t.Helper()
		s, _ := local.New(root)
		if _, err := s.Put(ctx, "sweeper", strings.NewReader("x"), storage.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	live, stdin, tmp := startHelper(t, root, "written before the pause, ")
	age(tmp, filepath.Dir(tmp))
	sweep()
	if _, err := os.Stat(tmp); err != nil {
		t.Fatalf("a sweep removed a live process's temporary file: %v", err)
	}
	if _, err := io.WriteString(stdin, "and after it"); err != nil {
		t.Fatal(err)
	}
	_ = stdin.Close()
	if err := live.Wait(); err != nil {
		t.Fatalf("the helper's Put failed after another store's sweep: %v", err)
	}
	s, _ := local.New(root)
	if got := readAll(t, s, "helper"); got != "written before the pause, and after it" {
		t.Fatalf("the helper stored %q", got)
	}

	dead, _, tmp := startHelper(t, root, "never finished")
	if err := dead.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = dead.Wait()
	age(tmp, filepath.Dir(tmp))
	sweep()
	if _, err := os.Stat(filepath.Dir(tmp)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the killed process's work directory survived the sweep (%v)", err)
	}
}

// TestRootRemovedWhileRunning: if the root is removed under a running
// store, the next Put creates it again.
func TestRootRemovedWhileRunning(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.Put(ctx, "k", strings.NewReader("before"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(s.Root()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, "k", strings.NewReader("after"), storage.PutOptions{}); err != nil {
		t.Fatalf("Put after the root was removed = %v", err)
	}
	if got := readAll(t, s, "k"); got != "after" {
		t.Fatalf("read %q", got)
	}
}

// blockedSource returns a reader that yields first, then blocks until
// release is called with the rest.
func blockedSource(first string) (io.Reader, func(rest string)) {
	rest := make(chan string, 1)
	return io.MultiReader(strings.NewReader(first), &gateReader{rest: rest}), func(r string) { rest <- r }
}

type gateReader struct {
	rest chan string
	r    io.Reader
}

func (g *gateReader) Read(p []byte) (int, error) {
	if g.r == nil {
		g.r = strings.NewReader(<-g.rest)
	}
	return g.r.Read(p)
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

// TestFlushFailureAfterCommitIsNotAFailure: once a rename has published an
// object (or a remove deleted one), a failing flush of its directory does
// not make the operation report failure (the caller would think nothing
// happened); it is retried and then reported through the warning hook.
func TestFlushFailureAfterCommitIsNotAFailure(t *testing.T) {
	var warned []error
	s, _ := local.New(filepath.Join(t.TempDir(), "root"), local.WithWarn(func(_ string, err error) { warned = append(warned, err) }))
	ctx := context.Background()
	leaf := filepath.Dir(s.Path("k"))
	calls := 0
	defer local.SetSyncDir(func(dir string) error {
		if dir == leaf {
			calls++
			return errors.New("fsync: I/O error")
		}
		return nil
	})()
	info, err := s.Put(ctx, "k", strings.NewReader("stored"), storage.PutOptions{})
	if err != nil {
		t.Fatalf("Put = %v, want success: the object was published", err)
	}
	if info.Size != 6 || calls != 2 || len(warned) != 1 {
		t.Fatalf("info %+v, %d flushes of the leaf, %d warnings; want size 6, 2, 1", info, calls, len(warned))
	}
	if st, err := s.Stat(ctx, "k"); err != nil || st.Size != 6 {
		t.Fatalf("Stat = %+v, %v", st, err)
	}
	if err := s.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete = %v, want success: the object was removed", err)
	}
	if calls != 4 || len(warned) != 2 {
		t.Fatalf("after Delete: %d leaf flushes, %d warnings; want 4, 2", calls, len(warned))
	}
}

// TestAncestorFlushFailureFailsBeforePublishing: if a new directory's entry
// cannot be flushed, Put fails while the object is still a temporary file,
// and the next Put flushes it again (a directory that exists is not taken
// to be durable).
func TestAncestorFlushFailureFailsBeforePublishing(t *testing.T) {
	s, _ := local.New(filepath.Join(t.TempDir(), "root"))
	ctx := context.Background()
	failRoot := true
	var synced []string
	defer local.SetSyncDir(func(dir string) error {
		synced = append(synced, dir)
		if dir == s.Root() && failRoot {
			return errors.New("fsync: I/O error")
		}
		return nil
	})()
	if _, err := s.Put(ctx, "k", strings.NewReader("x"), storage.PutOptions{}); err == nil {
		t.Fatal("Put succeeded although the root's entry for objects/ could not be flushed")
	}
	if _, err := s.Stat(ctx, "k"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("after the failed Put, Stat = %v; want nothing published", err)
	}
	if left := tempFiles(t, s.Root()); len(left) != 0 {
		t.Fatalf("the failed Put left temporary files %v", left)
	}
	failRoot, synced = false, nil
	if _, err := s.Put(ctx, "k", strings.NewReader("x"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(synced, s.Root()) {
		t.Fatalf("the retry did not flush the root again (flushed %v)", synced)
	}
}

// TestEveryEntryOnThePathIsFlushedOnce: the first Put flushes the root's
// entry in its parent and the entry of every directory under the root on
// the object's path, and nothing above the root's parent; a second Put
// into the same directory flushes only the directory it renames into; a
// second store sharing the root trusts the root marker for the root's
// entry, but flushes the directories under the root itself.
func TestEveryEntryOnThePathIsFlushedOnce(t *testing.T) {
	base := t.TempDir()
	s, _ := local.New(filepath.Join(base, "root"))
	ctx := context.Background()
	var synced []string
	defer local.SetSyncDir(func(dir string) error { synced = append(synced, dir); return nil })()
	if _, err := s.Put(ctx, "k", strings.NewReader("x"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Dir(s.Path("k"))
	objects := filepath.Join(s.Root(), "objects")
	want := []string{base, s.Root(), objects, filepath.Dir(leaf), leaf}
	if !slices.Equal(sorted(synced), sorted(want)) {
		t.Fatalf("the first Put flushed %v, want %v", synced, want)
	}
	synced = nil
	if _, err := s.Put(ctx, "k", strings.NewReader("y"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(synced, []string{leaf}) {
		t.Fatalf("a second Put into the same directory flushed %v, want only %s", synced, leaf)
	}
	synced = nil
	other, _ := local.New(s.Root())
	if _, err := other.Put(ctx, "k", strings.NewReader("z"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if want := []string{s.Root(), objects, filepath.Dir(leaf), leaf}; !slices.Equal(sorted(synced), sorted(want)) {
		t.Fatalf("a second store's first Put flushed %v, want %v", synced, want)
	}
}

func sorted(s []string) []string {
	s = slices.Clone(s)
	slices.Sort(s)
	return s
}

// TestRootWithoutMarkerIsFlushed: a root that exists without the marker
// (made by hand, or by a store that stopped before flushing it) is not
// taken for durable: the first Put flushes its entry in the parent, then
// writes the marker, and a later store trusts it.
func TestRootWithoutMarkerIsFlushed(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.Mkdir(root, 0o750); err != nil {
		t.Fatal(err)
	}
	var synced []string
	defer local.SetSyncDir(func(dir string) error { synced = append(synced, dir); return nil })()
	for i, wantParent := range []bool{true, false} {
		synced = nil
		s, _ := local.New(root)
		if _, err := s.Put(context.Background(), "k", strings.NewReader("x"), storage.PutOptions{}); err != nil {
			t.Fatal(err)
		}
		if got := slices.Contains(synced, base); got != wantParent {
			t.Fatalf("store %d flushed the root's parent = %v, want %v (flushed %v)", i, got, wantParent, synced)
		}
		if _, err := os.Stat(filepath.Join(root, ".durable")); err != nil {
			t.Fatalf("no marker after store %d's Put: %v", i, err)
		}
	}
}

// TestRootParentMustExist: the store creates its root, not the directories
// above it, whose durability it could not vouch for.
func TestRootParentMustExist(t *testing.T) {
	s, _ := local.New(filepath.Join(t.TempDir(), "missing", "root"))
	_, err := s.Put(context.Background(), "k", strings.NewReader("x"), storage.PutOptions{})
	if err == nil || !strings.Contains(err.Error(), "parent directory") {
		t.Fatalf("Put under a missing parent = %v, want an error naming the parent", err)
	}
}

// TestPathsDependOnlyOnTheKey: the key-to-path mapping is a pure function
// of the key's bytes, in lower-case hex, so no host's way of comparing
// names (case folding, Unicode normalization, reserved names) can make two
// keys share a file.
func TestPathsDependOnlyOnTheKey(t *testing.T) {
	s, _ := local.New("/root")
	seen := map[string]string{}
	for _, key := range []string{"Case/File.txt", "case/file.txt", "caf\u00e9/x", "cafe\u0301/x", "CON/nul.txt", "c:/aux", "a", "a/b"} {
		rel, err := filepath.Rel(filepath.Join(s.Root(), "objects"), s.Path(key))
		if err != nil {
			t.Fatal(err)
		}
		for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
			if strings.Trim(part, "0123456789abcdef") != "" {
				t.Fatalf("path component %q of %q is not lower-case hex", part, key)
			}
		}
		if other, ok := seen[rel]; ok {
			t.Fatalf("%q and %q map to the same path", key, other)
		}
		seen[rel] = key
	}
}

// TestSignedURLsInDevelopment: the local driver's URLs work end to end
// through presign.Handler, as framework.New mounts it.
func TestSignedURLsInDevelopment(t *testing.T) {
	signer, err := presign.New(presign.Config{Base: "/_storage", Secret: []byte(strings.Repeat("k", 32)), PublicPrefix: "public/"})
	if err != nil {
		t.Fatal(err)
	}
	store, err := local.New(t.TempDir(), local.WithURLs(signer))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(presign.Handler(store, signer))
	defer srv.Close()
	ctx := context.Background()
	for _, key := range []string{"private/report.pdf", "public/logo.png"} {
		if _, err := store.Put(ctx, key, strings.NewReader("bytes of "+key), storage.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	fetch := func(u string) (int, string) {
		resp, err := srv.Client().Get(srv.URL + u)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	signed, err := store.URL(ctx, "private/report.pdf", storage.SignedURL(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if code, body := fetch(signed); code != http.StatusOK || body != "bytes of private/report.pdf" {
		t.Fatalf("signed URL = %d %q", code, body)
	}
	if code, _ := fetch("/_storage/private/report.pdf"); code != http.StatusForbidden {
		t.Fatalf("the private object without a signature = %d, want 403", code)
	}
	public, err := store.URL(ctx, "public/logo.png", storage.PublicURL())
	if err != nil {
		t.Fatal(err)
	}
	if code, body := fetch(public); code != http.StatusOK || body != "bytes of public/logo.png" {
		t.Fatalf("public URL = %d %q", code, body)
	}
	if _, err := store.URL(ctx, "private/report.pdf", storage.PublicURL()); !errors.Is(err, storage.ErrNotPublic) {
		t.Fatalf("a public URL for a private object = %v", err)
	}
}

func TestListEdgeCases(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if err := s.List(ctx, "", func(storage.ObjectInfo) error { t.Fatal("an empty store listed something"); return nil }); err != nil {
		t.Fatalf("List before any Put = %v", err)
	}
	if _, err := s.Put(ctx, "a/b", strings.NewReader("x"), storage.PutOptions{ContentType: "text/plain", Metadata: map[string]string{"k": "v"}}); err != nil {
		t.Fatal(err)
	}
	var got []storage.ObjectInfo
	if err := s.List(ctx, "a/", func(o storage.ObjectInfo) error { got = append(got, o); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ContentType != "text/plain" || got[0].Metadata["k"] != "v" {
		t.Fatalf("List = %+v, want the full ObjectInfo", got)
	}
	// A damaged object file is skipped and reported; the rest still list.
	var warned []error
	s, err := local.New(s.Root(), local.WithWarn(func(_ string, err error) { warned = append(warned, err) }))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Root(), "objects", "zz"), []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	n := 0
	if err := s.List(ctx, "", func(storage.ObjectInfo) error { n++; return nil }); err != nil || n != 1 {
		t.Fatalf("List over a damaged file = %v, %d objects; want the good one", err, n)
	}
	if len(warned) != 1 || !strings.Contains(warned[0].Error(), "corrupt") {
		t.Fatalf("warnings = %v, want the damaged file reported", warned)
	}
}

// TestOpenReaderSurvivesOverwriteAndDelete: a reader keeps the version it
// opened while a Put replaces the object and a Delete removes it, as the
// contract promises, on every platform (Windows included: the open file
// does not lock its name).
func TestOpenReaderSurvivesOverwriteAndDelete(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.Put(ctx, "k", strings.NewReader("first version"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	body, _, err := s.Open(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = body.Close() }()
	head := make([]byte, 6)
	if _, err := io.ReadFull(body, head); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, "k", strings.NewReader("second version"), storage.PutOptions{}); err != nil {
		t.Fatalf("Put over an open reader = %v", err)
	}
	if got := readAll(t, s, "k"); got != "second version" {
		t.Fatalf("after the overwrite Open reads %q", got)
	}
	if err := s.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete of an object with an open reader = %v", err)
	}
	if _, err := s.Stat(ctx, "k"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("after Delete, Stat = %v", err)
	}
	if _, err := s.Put(ctx, "k", strings.NewReader("third"), storage.PutOptions{}); err != nil {
		t.Fatalf("Put after deleting an object with an open reader = %v", err)
	}
	rest, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(head) + string(rest); got != "first version" {
		t.Fatalf("the open reader read %q, want the version it opened", got)
	}
}

func readAll(t *testing.T, s *local.Store, key string) string {
	t.Helper()
	body, _, err := s.Open(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = body.Close() }()
	b, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestListDoesNotBlockWrites: a List reading an object's description does
// not keep a concurrent Put or Delete of that object from proceeding (on
// Windows, a file opened without delete sharing would).
func TestListDoesNotBlockWrites(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.Put(ctx, "k", strings.NewReader("x"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	listed := make(chan error, 1)
	go func() {
		for {
			select {
			case <-stop:
				listed <- nil
				return
			default:
			}
			if err := s.List(ctx, "", func(storage.ObjectInfo) error { return nil }); err != nil {
				listed <- err
				return
			}
		}
	}()
	for i := 0; i < 300; i++ {
		if _, err := s.Put(ctx, "k", strings.NewReader("version"), storage.PutOptions{}); err != nil {
			t.Fatalf("Put %d during a List = %v", i, err)
		}
		if i%3 == 0 {
			if err := s.Delete(ctx, "k"); err != nil {
				t.Fatalf("Delete %d during a List = %v", i, err)
			}
		}
	}
	close(stop)
	if err := <-listed; err != nil {
		t.Fatalf("List = %v", err)
	}
}

// TestReadsDuringWritesSeeAVersionOrNothing: Open and Stat racing Puts and
// Deletes of the same key see a whole version or ErrNotFound, never
// another error (on Windows, a file being deleted must read as gone, not
// as a permission failure).
func TestReadsDuringWritesSeeAVersionOrNothing(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	stop := make(chan struct{})
	failed := make(chan error, 1)
	go func() {
		defer close(failed)
		for {
			select {
			case <-stop:
				return
			default:
			}
			body, _, err := s.Open(ctx, "k")
			if err == nil {
				var b []byte
				b, err = io.ReadAll(body)
				_ = body.Close()
				if err == nil && string(b) != "version" {
					err = fmt.Errorf("read %q", b)
				}
			}
			if err == nil {
				_, err = s.Stat(ctx, "k")
			}
			if err != nil && !errors.Is(err, storage.ErrNotFound) {
				failed <- err
				return
			}
		}
	}()
	for i := 0; i < 300; i++ {
		if _, err := s.Put(ctx, "k", strings.NewReader("version"), storage.PutOptions{}); err != nil {
			t.Fatalf("Put %d = %v", i, err)
		}
		if err := s.Delete(ctx, "k"); err != nil {
			t.Fatalf("Delete %d = %v", i, err)
		}
	}
	close(stop)
	if err := <-failed; err != nil {
		t.Fatalf("a read racing writes = %v, want a version or ErrNotFound", err)
	}
}

// TestStoresDoNotTrustAnotherStoresUnflushedDirectories: store A creates
// the directories on an object's path and stops before flushing their
// entries (paused, or its flush failing); store B, sharing the root, must
// not take their existence for durability. It flushes every directory
// entry on the path itself before its Put returns.
func TestStoresDoNotTrustAnotherStoresUnflushedDirectories(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	a, _ := local.New(root)
	b, _ := local.New(root)
	ctx := context.Background()
	leaf := filepath.Dir(a.Path("k"))
	objects := filepath.Dir(filepath.Dir(leaf))

	var mu sync.Mutex
	var recording bool
	var byB []string
	paused, release := make(chan struct{}), make(chan struct{})
	pausedOnce := false
	defer local.SetSyncDir(func(dir string) error {
		mu.Lock()
		if dir == objects && !pausedOnce {
			pausedOnce = true
			mu.Unlock()
			close(paused)
			<-release
			return nil
		}
		if recording {
			byB = append(byB, dir)
		}
		mu.Unlock()
		return nil
	})()

	done := make(chan error, 1)
	go func() {
		_, err := a.Put(ctx, "k", strings.NewReader("from a"), storage.PutOptions{})
		done <- err
	}()
	<-paused // A has created objects/xx/yy but not flushed the entry of xx
	mu.Lock()
	recording = true
	mu.Unlock()
	if _, err := b.Put(ctx, "k", strings.NewReader("from b"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	recording = false
	got := slices.Clone(byB)
	mu.Unlock()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{root, objects, filepath.Dir(leaf), leaf} {
		if !slices.Contains(got, dir) {
			t.Errorf("B published into directories A had not flushed without flushing %s (B flushed %v)", dir, got)
		}
	}
}

// TestSweepSparesWorkDirectoriesBeingSetUp: a work directory whose owner
// file is not in place yet (its store is between creating the directory
// and locking the owner, or died there) is never taken for a dead
// store's, however old it looks: only a lock proves a store is gone.
func TestSweepSparesWorkDirectoriesBeingSetUp(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	tmp := filepath.Join(root, "tmp")
	long := time.Now().Add(-48 * time.Hour)
	for _, dir := range []string{"w-no-owner-yet", "w-owner-not-locked-yet"} {
		if err := os.MkdirAll(filepath.Join(tmp, dir), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(tmp, "w-owner-not-locked-yet", "owner.new"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"w-no-owner-yet", "w-owner-not-locked-yet"} {
		if err := os.Chtimes(filepath.Join(tmp, dir), long, long); err != nil {
			t.Fatal(err)
		}
	}
	s, _ := local.New(root)
	if _, err := s.Put(context.Background(), "k", strings.NewReader("x"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"w-no-owner-yet", "w-owner-not-locked-yet"} {
		if _, err := os.Stat(filepath.Join(tmp, dir)); err != nil {
			t.Errorf("the sweep removed %s, a store that may still be setting it up: %v", dir, err)
		}
	}
}

// TestLongRoot: a root whose object paths are longer than Windows' legacy
// 260-character limit works like any other (every path the store hands
// the system is in its long form there).
func TestLongRoot(t *testing.T) {
	parent := t.TempDir()
	for len(parent) < 250 {
		parent = filepath.Join(parent, strings.Repeat("d", 40))
	}
	if err := os.MkdirAll(parent, 0o750); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "root")
	s, _ := local.New(root)
	ctx := context.Background()
	if p := s.Path("k"); len(p) < 300 {
		t.Fatalf("object path is only %d characters", len(p))
	}
	if _, err := s.Put(ctx, "k", strings.NewReader("first"), storage.PutOptions{}); err != nil {
		t.Fatalf("Put = %v", err)
	}
	body, _, err := s.Open(ctx, "k")
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	defer func() { _ = body.Close() }()
	if _, err := s.Put(ctx, "k", strings.NewReader("second"), storage.PutOptions{}); err != nil {
		t.Fatalf("Put over an open reader = %v", err)
	}
	if got := readAll(t, s, "k"); got != "second" {
		t.Fatalf("read %q", got)
	}
	if err := s.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete = %v", err)
	}
	if _, err := s.Stat(ctx, "k"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Stat after Delete = %v", err)
	}
	other, _ := local.New(root) // its first Put sweeps, probing s's owner file
	if _, err := other.Put(ctx, "k2", strings.NewReader("x"), storage.PutOptions{}); err != nil {
		t.Fatalf("a second store's Put = %v", err)
	}
}

// TestIfAbsentAcrossStores: IfAbsent is atomic for stores that share a
// root (separate processes, say), not just within one store: of two stores
// racing to create one key, exactly one succeeds.
func TestIfAbsentAcrossStores(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	ctx := context.Background()
	for round := 0; round < 20; round++ {
		key := fmt.Sprintf("race/%d", round)
		results := make(chan error, 2)
		start := make(chan struct{})
		for i := 0; i < 2; i++ {
			s, _ := local.New(root)
			body := fmt.Sprintf("store %d", i)
			go func() {
				<-start
				_, err := s.Put(ctx, key, strings.NewReader(body), storage.PutOptions{IfAbsent: true})
				results <- err
			}()
		}
		close(start)
		won := 0
		for i := 0; i < 2; i++ {
			switch err := <-results; {
			case err == nil:
				won++
			case !errors.Is(err, storage.ErrExists):
				t.Fatalf("round %d: Put = %v, want success or ErrExists", round, err)
			}
		}
		if won != 1 {
			t.Fatalf("round %d: %d of 2 stores created %q, want exactly one", round, won, key)
		}
	}
}

// TestLinkPublishThatCannotRemoveItsTempIsASuccess: the hard-link fallback
// publishes the object when it links it; failing to remove the temporary
// name afterwards does not make the Put a failure (the key holds the new
// object, and a failure must leave it unchanged): Put succeeds and the
// warning hook reports the stray file.
func TestLinkPublishThatCannotRemoveItsTempIsASuccess(t *testing.T) {
	var warned []error
	s, _ := local.New(filepath.Join(t.TempDir(), "root"), local.WithWarn(func(_ string, err error) { warned = append(warned, err) }))
	ctx := context.Background()
	defer local.UseLinkPublish(func(string) error { return errors.New("remove: permission denied") })()
	info, err := s.Put(ctx, "k", strings.NewReader("linked"), storage.PutOptions{IfAbsent: true})
	if err != nil {
		t.Fatalf("Put = %v; the object was published, so the Put succeeded", err)
	}
	if got := readAll(t, s, "k"); got != "linked" || info.Size != 6 {
		t.Fatalf("the key holds %q (%d), want the new object", got, info.Size)
	}
	if len(warned) != 1 || !strings.Contains(warned[0].Error(), "temporary file") {
		t.Fatalf("warnings = %v, want the stray temporary file reported", warned)
	}
	if _, err := s.Put(ctx, "k", strings.NewReader("again"), storage.PutOptions{IfAbsent: true}); !errors.Is(err, storage.ErrExists) {
		t.Fatalf("a second IfAbsent Put through the link fallback = %v, want ErrExists", err)
	}
}
