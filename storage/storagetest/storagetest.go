// Package storagetest is the conformance suite every storage.Storage driver
// must pass. A driver's test calls Run with a function that returns a fresh,
// empty store:
//
//	func TestConformance(t *testing.T) {
//		storagetest.Run(t, func(t *testing.T) storage.Storage {
//			return local.New(t.TempDir())
//		})
//	}
//
// The suite checks the contract the storage package documents: streaming,
// atomic writes, the key rules, idempotent deletes, and error
// classification. It needs no network and makes no assumption about how a
// driver stores bytes.
package storagetest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/storage"
)

// check is one property of the contract. It gets a fresh, empty store.
type check struct {
	name string
	run  func(t testing.TB, s storage.Storage)
}

// Run runs every conformance check against stores from newStorage, each
// check on a fresh, empty store.
func Run(t *testing.T, newStorage func(t *testing.T) storage.Storage) {
	t.Helper()
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			c.run(t, newStorage(t))
		})
	}
}

var checks = []check{
	{"RoundTrip", checkRoundTrip},
	{"DefaultContentType", checkDefaultContentType},
	{"Overwrite", checkOverwrite},
	{"Missing", checkMissing},
	{"Delete", checkDelete},
	{"InvalidKeys", checkInvalidKeys},
	{"NestedKeys", checkNestedKeys},
	{"PortableKeys", checkPortableKeys},
	{"CanceledContext", checkCanceledContext},
	{"Streaming", checkStreaming},
	{"EmptyObject", checkEmptyObject},
	{"FailedPutKeepsPrevious", checkFailedPutKeepsPrevious},
	{"CanceledPut", checkCanceledPut},
	{"SizeMismatch", checkSizeMismatch},
	{"InvalidOptions", checkInvalidOptions},
	{"ConcurrentPuts", checkConcurrentPuts},
	{"URL", checkURL},
}

func ctxFor(t testing.TB) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func put(t testing.TB, s storage.Storage, key string, data []byte, opts storage.PutOptions) storage.ObjectInfo {
	t.Helper()
	info, err := s.Put(ctxFor(t), key, bytes.NewReader(data), opts)
	if err != nil {
		t.Fatalf("Put(%q) = %v", key, err)
	}
	return info
}

// read returns the object's bytes and info, failing t on any error.
func read(t testing.TB, s storage.Storage, key string) ([]byte, storage.ObjectInfo) {
	t.Helper()
	body, info, err := s.Open(ctxFor(t), key)
	if err != nil {
		t.Fatalf("Open(%q) = %v", key, err)
	}
	defer func() { _ = body.Close() }()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("reading %q: %v", key, err)
	}
	return data, info
}

func wantNotFound(t testing.TB, s storage.Storage, key string) {
	t.Helper()
	body, _, err := s.Open(ctxFor(t), key)
	if err == nil {
		_ = body.Close()
		t.Fatalf("Open(%q) succeeded, want storage.ErrNotFound", key)
	}
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Open(%q) = %v, want storage.ErrNotFound", key, err)
	}
}

func checkRoundTrip(t testing.TB, s storage.Storage) {
	data := []byte("hello, storage")
	md := map[string]string{"original-name": "résumé final.pdf", "owner": "42"}
	info := put(t, s, "docs/report.txt", data, storage.PutOptions{ContentType: "text/plain; charset=utf-8", Metadata: md})
	want := storage.ObjectInfo{Key: "docs/report.txt", Size: int64(len(data)), ContentType: "text/plain; charset=utf-8", Metadata: md}
	sameInfo(t, "Put", info, want, false)

	got, opened := read(t, s, "docs/report.txt")
	if !bytes.Equal(got, data) {
		t.Fatalf("Open read %q, want %q", got, data)
	}
	sameInfo(t, "Open", opened, want, true)

	stat, err := s.Stat(ctxFor(t), "docs/report.txt")
	if err != nil {
		t.Fatalf("Stat = %v", err)
	}
	sameInfo(t, "Stat", stat, want, true)
	if ok, err := storage.Exists(ctxFor(t), s, "docs/report.txt"); err != nil || !ok {
		t.Fatalf("Exists = %v, %v; want true", ok, err)
	}
}

// sameInfo compares the fields every driver must report. ETag is
// driver-defined; ModTime must be set by Stat and Open (withTime), and may
// be zero in what Put returns.
func sameInfo(t testing.TB, op string, got, want storage.ObjectInfo, withTime bool) {
	t.Helper()
	if got.Key != want.Key || got.Size != want.Size || got.ContentType != want.ContentType {
		t.Fatalf("%s info = {Key:%q Size:%d ContentType:%q}, want {Key:%q Size:%d ContentType:%q}",
			op, got.Key, got.Size, got.ContentType, want.Key, want.Size, want.ContentType)
	}
	if len(got.Metadata) != len(want.Metadata) {
		t.Fatalf("%s metadata = %v, want %v", op, got.Metadata, want.Metadata)
	}
	for k, v := range want.Metadata {
		if got.Metadata[k] != v {
			t.Fatalf("%s metadata = %v, want %v", op, got.Metadata, want.Metadata)
		}
	}
	if withTime && got.ModTime.IsZero() {
		t.Fatalf("%s info has a zero ModTime", op)
	}
}

func checkDefaultContentType(t testing.TB, s storage.Storage) {
	info := put(t, s, "blob", []byte{1, 2, 3}, storage.PutOptions{})
	if info.ContentType != storage.DefaultContentType {
		t.Fatalf("Put without a content type stored %q, want %q", info.ContentType, storage.DefaultContentType)
	}
	if _, opened := read(t, s, "blob"); opened.ContentType != storage.DefaultContentType {
		t.Fatalf("Open reports %q, want %q", opened.ContentType, storage.DefaultContentType)
	}
}

func checkOverwrite(t testing.TB, s storage.Storage) {
	put(t, s, "k", []byte("first version, longer"), storage.PutOptions{ContentType: "text/plain", Metadata: map[string]string{"v": "1"}})
	put(t, s, "k", []byte("second"), storage.PutOptions{ContentType: "application/json"})
	got, info := read(t, s, "k")
	if string(got) != "second" {
		t.Fatalf("after an overwrite Open read %q, want %q", got, "second")
	}
	sameInfo(t, "Open after overwrite", info, storage.ObjectInfo{Key: "k", Size: 6, ContentType: "application/json"}, true)
}

func checkMissing(t testing.TB, s storage.Storage) {
	wantNotFound(t, s, "nope")
	if _, err := s.Stat(ctxFor(t), "nope"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Stat of a missing key = %v, want storage.ErrNotFound", err)
	}
	if ok, err := storage.Exists(ctxFor(t), s, "nope"); err != nil || ok {
		t.Fatalf("Exists of a missing key = %v, %v; want false, nil", ok, err)
	}
	if err := s.Delete(ctxFor(t), "nope"); err != nil {
		t.Fatalf("Delete of a missing key = %v, want nil (deletes are idempotent)", err)
	}
}

func checkDelete(t testing.TB, s storage.Storage) {
	put(t, s, "a/b", []byte("x"), storage.PutOptions{})
	put(t, s, "a/c", []byte("y"), storage.PutOptions{})
	if err := s.Delete(ctxFor(t), "a/b"); err != nil {
		t.Fatalf("Delete = %v", err)
	}
	wantNotFound(t, s, "a/b")
	if err := s.Delete(ctxFor(t), "a/b"); err != nil {
		t.Fatalf("a second Delete = %v, want nil", err)
	}
	if got, _ := read(t, s, "a/c"); string(got) != "y" {
		t.Fatalf("Delete of a/b changed a/c to %q", got)
	}
}

// invalidKeys break ValidateKey; every method must refuse them before
// touching the backend.
var invalidKeys = []string{
	"",
	"/abs",
	"trailing/",
	"a//b",
	".",
	"..",
	"../escape",
	"a/../../escape",
	"a/./b",
	`a\b`,
	"nul\x00byte",
	"new\nline",
	"bad\xffutf8",
	"c1\u0085control",
	"bidi\u202eoverride",
	"line\u2028separator",
	strings.Repeat("k", storage.MaxKeyBytes+1),
}

func checkInvalidKeys(t testing.TB, s storage.Storage) {
	for _, key := range invalidKeys {
		label := fmt.Sprintf("%q", key)
		if len(label) > 40 {
			label = label[:40] + "…"
		}
		if _, err := s.Put(ctxFor(t), key, strings.NewReader("x"), storage.PutOptions{}); !errors.Is(err, storage.ErrInvalidKey) {
			t.Fatalf("Put(%s) = %v, want storage.ErrInvalidKey", label, err)
		}
		if body, _, err := s.Open(ctxFor(t), key); !errors.Is(err, storage.ErrInvalidKey) {
			if body != nil {
				_ = body.Close()
			}
			t.Fatalf("Open(%s) = %v, want storage.ErrInvalidKey", label, err)
		}
		if _, err := s.Stat(ctxFor(t), key); !errors.Is(err, storage.ErrInvalidKey) {
			t.Fatalf("Stat(%s) = %v, want storage.ErrInvalidKey", label, err)
		}
		if err := s.Delete(ctxFor(t), key); !errors.Is(err, storage.ErrInvalidKey) {
			t.Fatalf("Delete(%s) = %v, want storage.ErrInvalidKey", label, err)
		}
		if _, err := s.URL(ctxFor(t), key, storage.PublicURL()); !errors.Is(err, storage.ErrInvalidKey) {
			t.Fatalf("URL(%s) = %v, want storage.ErrInvalidKey", label, err)
		}
	}
}

// checkNestedKeys: keys with several segments and unusual but valid
// characters store and read back as themselves.
func checkNestedKeys(t testing.TB, s storage.Storage) {
	keys := []string{
		"one/two/three/four.txt",
		"spaces in name/file (1).pdf",
		"ünïcødé/数据.bin",
		"dots/..hidden/.also/name..ext",
		"x",
	}
	for i, key := range keys {
		put(t, s, key, []byte(fmt.Sprint(i)), storage.PutOptions{})
	}
	for i, key := range keys {
		got, info := read(t, s, key)
		if string(got) != fmt.Sprint(i) || info.Key != key {
			t.Fatalf("Open(%q) read %q with key %q, want %q with the same key", key, got, info.Key, fmt.Sprint(i))
		}
	}
}

// checkPortableKeys: every valid key is its own object, including keys a
// filesystem would conflate or refuse: case variants, a key that is a
// prefix of another, a long segment, names Windows reserves, and segments
// ending in a dot or a space.
func checkPortableKeys(t testing.TB, s storage.Storage) {
	keys := []string{
		"Case/File.txt",
		"case/file.txt",
		"prefix",
		"prefix/child",
		"prefix/child/grandchild",
		strings.Repeat("l", 300) + "/long-segment",
		"CON/nul.txt",
		"c:/aux",
		"dot./space /x",
	}
	for i, key := range keys {
		put(t, s, key, []byte(fmt.Sprint(i)), storage.PutOptions{})
	}
	for i, key := range keys {
		got, info := read(t, s, key)
		if string(got) != fmt.Sprint(i) || info.Key != key {
			t.Fatalf("Open(%q) read %q with key %q, want %q with the same key: two keys share one object, or one did not store", key, got, info.Key, fmt.Sprint(i))
		}
	}
	if err := s.Delete(ctxFor(t), "prefix"); err != nil {
		t.Fatalf("Delete(prefix) = %v", err)
	}
	if got, _ := read(t, s, "prefix/child"); string(got) != "3" {
		t.Fatalf("deleting \"prefix\" changed \"prefix/child\" to %q", got)
	}
}

// checkCanceledContext: every method called with a context that has
// already ended fails with the context's error.
func checkCanceledContext(t testing.TB, s storage.Storage) {
	put(t, s, "present", []byte("x"), storage.PutOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if body, _, err := s.Open(ctx, "present"); !errors.Is(err, context.Canceled) {
		if body != nil {
			_ = body.Close()
		}
		t.Fatalf("Open on a canceled context = %v, want context.Canceled", err)
	}
	if _, err := s.Stat(ctx, "present"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stat on a canceled context = %v, want context.Canceled", err)
	}
	if err := s.Delete(ctx, "present"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Delete on a canceled context = %v, want context.Canceled", err)
	}
	if _, err := s.URL(ctx, "present", storage.SignedURL(time.Minute)); !errors.Is(err, context.Canceled) {
		t.Fatalf("URL on a canceled context = %v, want context.Canceled", err)
	}
	if got, _ := read(t, s, "present"); string(got) != "x" {
		t.Fatalf("a canceled Delete removed the object")
	}
}

// streamSize is large enough that a driver cannot pass by accident with a
// single read or write, and small enough to keep the suite fast.
const streamSize = 8 << 20

// checkStreaming: Put accepts a non-seekable reader of unknown length fed
// in small pieces, and Open's reader delivers the same bytes read in small
// pieces. The test never holds the whole object in memory itself.
func checkStreaming(t testing.TB, s storage.Storage) {
	pr, pw := io.Pipe()
	wantSum := make(chan []byte, 1)
	go func() {
		h := sha256.New()
		rng := rand.New(rand.NewPCG(1, 2)) // #nosec G404 -- test data, not security
		chunk := make([]byte, 32<<10)
		for written := 0; written < streamSize; written += len(chunk) {
			for i := range chunk {
				chunk[i] = byte(rng.Uint32()) // #nosec G115 -- truncation to a byte is the point
			}
			h.Write(chunk)
			if _, err := pw.Write(chunk); err != nil {
				pw.CloseWithError(err)
				wantSum <- nil
				return
			}
		}
		wantSum <- h.Sum(nil)
		_ = pw.Close()
	}()
	info, err := s.Put(ctxFor(t), "big/stream.bin", pr, storage.PutOptions{})
	_ = pr.Close()
	if err != nil {
		t.Fatalf("Put of a streamed object = %v", err)
	}
	want := <-wantSum
	if info.Size != streamSize {
		t.Fatalf("Put reports size %d, want %d", info.Size, streamSize)
	}
	body, opened, err := s.Open(ctxFor(t), "big/stream.bin")
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	defer func() { _ = body.Close() }()
	if opened.Size != streamSize {
		t.Fatalf("Open reports size %d, want %d", opened.Size, streamSize)
	}
	h := sha256.New()
	n, err := io.CopyBuffer(h, body, make([]byte, 4<<10))
	if err != nil {
		t.Fatalf("reading the stream: %v", err)
	}
	if n != streamSize || !bytes.Equal(h.Sum(nil), want) {
		t.Fatalf("Open streamed %d bytes with a different digest, want %d identical bytes", n, streamSize)
	}
}

func checkEmptyObject(t testing.TB, s storage.Storage) {
	info := put(t, s, "empty", nil, storage.PutOptions{})
	if info.Size != 0 {
		t.Fatalf("an empty Put reports size %d", info.Size)
	}
	if got, opened := read(t, s, "empty"); len(got) != 0 || opened.Size != 0 {
		t.Fatalf("an empty object reads back %d bytes, size %d", len(got), opened.Size)
	}
}

var errBrokenReader = errors.New("storagetest: the upload's reader failed")

// failingReader returns n bytes, then err.
type failingReader struct {
	n   int
	err error
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.n == 0 {
		return 0, f.err
	}
	if len(p) > f.n {
		p = p[:f.n]
	}
	for i := range p {
		p[i] = 'z'
	}
	f.n -= len(p)
	return len(p), nil
}

// checkFailedPutKeepsPrevious: a Put whose reader fails part-way returns
// that error and leaves the key as it was: the previous object whole, or
// still absent.
func checkFailedPutKeepsPrevious(t testing.TB, s storage.Storage) {
	put(t, s, "kept", []byte("the previous version"), storage.PutOptions{ContentType: "text/plain"})
	_, err := s.Put(ctxFor(t), "kept", &failingReader{n: 64 << 10, err: errBrokenReader}, storage.PutOptions{ContentType: "image/png"})
	if !errors.Is(err, errBrokenReader) {
		t.Fatalf("a Put whose reader failed = %v, want that reader's error", err)
	}
	got, info := read(t, s, "kept")
	if string(got) != "the previous version" || info.ContentType != "text/plain" {
		t.Fatalf("after a failed overwrite the key holds %d bytes of %q, want the previous version whole", len(got), info.ContentType)
	}

	if _, err := s.Put(ctxFor(t), "never", &failingReader{n: 1, err: errBrokenReader}, storage.PutOptions{}); !errors.Is(err, errBrokenReader) {
		t.Fatalf("a failed first Put = %v, want the reader's error", err)
	}
	wantNotFound(t, s, "never")
}

// cancelingReader returns n bytes, then cancels ctx and keeps returning
// data (up to a bound, then EOF): only the context can stop the Put, and a
// driver that ignores it finishes the Put instead of running forever.
type cancelingReader struct {
	n, after int
	cancel   context.CancelFunc
}

func (c *cancelingReader) Read(p []byte) (int, error) {
	if c.n <= 0 {
		c.cancel()
		if c.after <= 0 {
			return 0, io.EOF
		}
		if len(p) > c.after {
			p = p[:c.after]
		}
		c.after -= len(p)
	} else {
		if len(p) > c.n {
			p = p[:c.n]
		}
		c.n -= len(p)
	}
	for i := range p {
		p[i] = 'c'
	}
	return len(p), nil
}

// checkCanceledPut: a Put whose context ends, before it starts or while it
// streams, fails with the context's error and stores nothing.
func checkCanceledPut(t testing.TB, s storage.Storage) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Put(ctx, "canceled", strings.NewReader("x"), storage.PutOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Put on a canceled context = %v, want context.Canceled", err)
	}
	wantNotFound(t, s, "canceled")

	put(t, s, "streaming", []byte("before"), storage.PutOptions{})
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := s.Put(ctx, "streaming", &cancelingReader{n: 256 << 10, after: 16 << 20, cancel: cancel}, storage.PutOptions{})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a Put canceled while streaming = %v, want context.Canceled", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a Put canceled while streaming did not return: it ignores its context")
	}
	if got, _ := read(t, s, "streaming"); string(got) != "before" {
		t.Fatalf("after a canceled overwrite the key holds %d bytes, want the previous version", len(got))
	}
}

func checkSizeMismatch(t testing.TB, s storage.Storage) {
	for _, tc := range []struct {
		name string
		data string
		size int64
	}{
		{"short", "12345", 10},
		{"long", "123456789012345", 10},
	} {
		_, err := s.Put(ctxFor(t), "sized/"+tc.name, strings.NewReader(tc.data), storage.PutOptions{Size: tc.size})
		if !errors.Is(err, storage.ErrSizeMismatch) {
			t.Fatalf("Put of %d bytes declared as %d = %v, want storage.ErrSizeMismatch", len(tc.data), tc.size, err)
		}
		wantNotFound(t, s, "sized/"+tc.name)
	}
	info := put(t, s, "sized/exact", []byte("0123456789"), storage.PutOptions{Size: 10})
	if info.Size != 10 {
		t.Fatalf("an exactly sized Put reports size %d, want 10", info.Size)
	}
}

func checkInvalidOptions(t testing.TB, s storage.Storage) {
	for _, opts := range []storage.PutOptions{
		{ContentType: "not a media type"},
		{ContentType: "text/"},
		{Size: -1},
		{Metadata: map[string]string{"Upper": "x"}},
		{Metadata: map[string]string{"under_score": "x"}},
		{Metadata: map[string]string{"": "x"}},
		{Metadata: map[string]string{"ok": "new\nline"}},
		{Metadata: map[string]string{"big": strings.Repeat("v", storage.MaxMetadataBytes)}},
		// 1400 bytes of UTF-8, 4200 once percent-encoded: over S3's limit.
		{Metadata: map[string]string{"name": strings.Repeat("é", 700)}},
		{Metadata: map[string]string{"name": "invoice\u202efdp.exe"}},
		{Metadata: map[string]string{"name": "line\u2028break"}},
	} {
		if _, err := s.Put(ctxFor(t), "opts", strings.NewReader("x"), opts); !errors.Is(err, storage.ErrInvalidOptions) {
			t.Fatalf("Put with %+v = %v, want storage.ErrInvalidOptions", opts, err)
		}
		wantNotFound(t, s, "opts")
	}
	if _, err := s.URL(ctxFor(t), "opts", storage.URLOptions{Expires: -time.Second}); !errors.Is(err, storage.ErrInvalidOptions) {
		t.Fatalf("URL with a negative expiry = %v, want storage.ErrInvalidOptions", err)
	}
}

// checkConcurrentPuts: concurrent Puts to one key leave exactly one of
// them, whole.
func checkConcurrentPuts(t testing.TB, s storage.Storage) {
	const writers = 8
	payload := func(i int) []byte { return bytes.Repeat([]byte{byte('a' + i)}, 256<<10) } // #nosec G115 -- i < writers
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.Put(ctxFor(t), "contended", bytes.NewReader(payload(i)), storage.PutOptions{})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a concurrent Put = %v", err)
		}
	}
	got, _ := read(t, s, "contended")
	for i := 0; i < writers; i++ {
		if bytes.Equal(got, payload(i)) {
			return
		}
	}
	t.Fatalf("after concurrent Puts the key holds %d bytes matching none of the writers: a torn write", len(got))
}

// checkURL: a driver either produces URLs or says it cannot; it never
// returns an empty URL without an error.
func checkURL(t testing.TB, s storage.Storage) {
	put(t, s, "linked/file.txt", []byte("x"), storage.PutOptions{})
	for _, opts := range []storage.URLOptions{storage.PublicURL(), storage.SignedURL(time.Minute)} {
		u, err := s.URL(ctxFor(t), "linked/file.txt", opts)
		switch {
		case errors.Is(err, storage.ErrUnsupported):
		case err != nil:
			t.Fatalf("URL(%+v) = %v, want a URL or storage.ErrUnsupported", opts, err)
		case u == "":
			t.Fatalf("URL(%+v) returned an empty URL and no error", opts)
		}
	}
}
