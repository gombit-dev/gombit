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
	{"NoPartialReads", checkNoPartialReads},
	{"MetadataIsOwned", checkMetadataIsOwned},
	{"OpenFollowsContext", checkOpenFollowsContext},
	{"URL", checkURL},
	{"DirectUpload", checkDirectUpload},
	{"List", checkList},
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
	wantErr(t, err, storage.ErrNotFound, "open", key)
}

// wantErr fails t unless err is want (errors.Is) inside the *storage.Error
// of op on key: every driver failure carries that envelope, so the
// operation and the key reach the logs whatever the driver.
func wantErr(t testing.TB, err, want error, op, key string) {
	t.Helper()
	label := fmt.Sprintf("%q", key)
	if len(label) > 40 {
		label = label[:40] + "…"
	}
	if !errors.Is(err, want) {
		t.Fatalf("%s(%s) = %v, want %v", op, label, err, want)
	}
	var se *storage.Error
	if !errors.As(err, &se) || se.Op != op || se.Key != key {
		t.Fatalf("%s(%s) = %v: want it inside a *storage.Error with Op %q and the key (the envelope drivers must return)", op, label, err, op)
	}
}

func checkRoundTrip(t testing.TB, s storage.Storage) {
	data := []byte("hello, storage")
	md := map[string]string{"original-name": "résumé final.pdf", "owner": "42", "note": "two  spaces inside"}
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
	checkETagVersions(t, s)
}

// checkETagVersions: ETags are one capability. A driver reports the same
// ETag from Put, Open, and Stat for one version of an object, or reports
// none from any of them; and a driver that reports them gives different
// bytes a different ETag, so a conditional read never takes changed
// content for unchanged.
func checkETagVersions(t testing.TB, s storage.Storage) {
	first := versionETag(t, s, "version one")
	second := versionETag(t, s, "version two")
	if (first == "") != (second == "") {
		t.Fatalf("the first version has ETag %q and the second %q: a driver reports ETags always or never", first, second)
	}
	if first != "" && first == second {
		t.Fatalf("different bytes kept the ETag %q: a conditional read would take the new content for the old", first)
	}
}

// versionETag stores data under "versioned" and returns its ETag, failing
// t unless Put, Open, and Stat agree on it (all empty, or all the same).
func versionETag(t testing.TB, s storage.Storage, data string) string {
	t.Helper()
	put := put(t, s, "versioned", []byte(data), storage.PutOptions{})
	_, opened := read(t, s, "versioned")
	stat, err := s.Stat(ctxFor(t), "versioned")
	if err != nil {
		t.Fatal(err)
	}
	if put.ETag != stat.ETag || opened.ETag != stat.ETag {
		t.Fatalf("one version of an object has ETag %q from Put, %q from Open, and %q from Stat: they must agree (or all be empty, for no ETags)", put.ETag, opened.ETag, stat.ETag)
	}
	return stat.ETag
}

func checkMissing(t testing.TB, s storage.Storage) {
	wantNotFound(t, s, "nope")
	_, err := s.Stat(ctxFor(t), "nope")
	wantErr(t, err, storage.ErrNotFound, "stat", "nope")
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
	"a/" + strings.Repeat("s", storage.MaxSegmentBytes+1),
	"...",
	".. ",
	"foo/.. /bar",
	"a./b",
	"a /b",
}

func checkInvalidKeys(t testing.TB, s storage.Storage) {
	for _, key := range invalidKeys {
		_, err := s.Put(ctxFor(t), key, strings.NewReader("x"), storage.PutOptions{})
		wantErr(t, err, storage.ErrInvalidKey, "put", key)
		body, _, err := s.Open(ctxFor(t), key)
		if body != nil {
			_ = body.Close()
		}
		wantErr(t, err, storage.ErrInvalidKey, "open", key)
		_, err = s.Stat(ctxFor(t), key)
		wantErr(t, err, storage.ErrInvalidKey, "stat", key)
		wantErr(t, s.Delete(ctxFor(t), key), storage.ErrInvalidKey, "delete", key)
		_, err = s.URL(ctxFor(t), key, storage.PublicURL())
		wantErr(t, err, storage.ErrInvalidKey, "url", key)
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
// filesystem would conflate or refuse: case variants, Unicode
// normalization variants (APFS folds NFC and NFD), a key that is a prefix
// of another, a long segment, and names Windows reserves. A round trip can
// only see what the host it runs on does: Linux keeps all of these apart, so
// a filesystem driver must also show that its key-to-path mapping never
// depends on how the host compares names (the local driver hashes keys, and
// tests that its paths are hex).
func checkPortableKeys(t testing.TB, s storage.Storage) {
	keys := []string{
		"Case/File.txt",
		"case/file.txt",
		"caf\u00e9/nfc",  // é precomposed (NFC)
		"cafe\u0301/nfc", // e + combining acute (NFD): a different key
		"prefix",
		"prefix/child",
		"prefix/child/grandchild",
		strings.Repeat("l", storage.MaxSegmentBytes) + "/long-segment",
		"CON/nul.txt",
		"c:/aux",
		"dot.x/ space/x",
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
	if got, _ := read(t, s, "prefix/child"); string(got) != "5" {
		t.Fatalf("deleting \"prefix\" changed \"prefix/child\" to %q", got)
	}
}

// checkCanceledContext: every method called with a context that has
// already ended fails with the context's error.
func checkCanceledContext(t testing.TB, s storage.Storage) {
	put(t, s, "present", []byte("x"), storage.PutOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	body, _, err := s.Open(ctx, "present")
	if body != nil {
		_ = body.Close()
	}
	wantErr(t, err, context.Canceled, "open", "present")
	_, err = s.Stat(ctx, "present")
	wantErr(t, err, context.Canceled, "stat", "present")
	wantErr(t, s.Delete(ctx, "present"), context.Canceled, "delete", "present")
	_, err = s.URL(ctx, "present", storage.SignedURL(time.Minute))
	wantErr(t, err, context.Canceled, "url", "present")
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
	wantErr(t, err, errBrokenReader, "put", "kept")
	got, info := read(t, s, "kept")
	if string(got) != "the previous version" || info.ContentType != "text/plain" {
		t.Fatalf("after a failed overwrite the key holds %d bytes of %q, want the previous version whole", len(got), info.ContentType)
	}

	_, err = s.Put(ctxFor(t), "never", &failingReader{n: 1, err: errBrokenReader}, storage.PutOptions{})
	wantErr(t, err, errBrokenReader, "put", "never")
	// A source that fails with another operation's envelope (reading another
	// object): the failure is still this Put's.
	foreign := storage.Wrap("open", "some/source", errBrokenReader)
	_, err = s.Put(ctxFor(t), "copied", &failingReader{n: 1, err: foreign}, storage.PutOptions{})
	wantErr(t, err, errBrokenReader, "put", "copied")
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
	_, err := s.Put(ctx, "canceled", strings.NewReader("x"), storage.PutOptions{})
	wantErr(t, err, context.Canceled, "put", "canceled")
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
		wantErr(t, err, context.Canceled, "put", "streaming")
	case <-time.After(20 * time.Second):
		t.Fatal("a Put canceled while streaming did not return: it ignores its context")
	}
	if got, _ := read(t, s, "streaming"); string(got) != "before" {
		t.Fatalf("after a canceled overwrite the key holds %d bytes, want the previous version", len(got))
	}

	// Canceled while a source Read is blocked: Put cannot interrupt that
	// Read (the contract), but once it returns, the Put fails with ctx's
	// error and stores nothing, even though the source then ends cleanly.
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	g := &gate{started: make(chan struct{}), release: make(chan struct{})}
	src := io.MultiReader(strings.NewReader("blocked-first-half"), g, strings.NewReader("-second-half"))
	done = make(chan error, 1)
	go func() {
		_, err := s.Put(ctx, "blocked", src, storage.PutOptions{})
		done <- err
	}()
	select {
	case <-g.started:
	case err := <-done:
		t.Fatalf("the Put returned (%v) before reading past the first half of its source", err)
	case <-time.After(20 * time.Second):
		t.Fatal("the Put never read past the first half of its source")
	}
	cancel() // the Put is blocked in its source's Read
	close(g.release)
	select {
	case err := <-done:
		wantErr(t, err, context.Canceled, "put", "blocked")
	case <-time.After(20 * time.Second):
		t.Fatal("a Put whose context ended while its source was blocked did not return once the source did")
	}
	wantNotFound(t, s, "blocked")
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
		_, err := s.Put(ctxFor(t), "sized/"+tc.name, strings.NewReader(tc.data), storage.PutOptions{Size: storage.KnownSize(tc.size)})
		wantErr(t, err, storage.ErrSizeMismatch, "put", "sized/"+tc.name)
		wantNotFound(t, s, "sized/"+tc.name)
		// Over an existing object, the mismatch leaves it whole.
		put(t, s, "sized/kept-"+tc.name, []byte("previous version"), storage.PutOptions{ContentType: "text/plain"})
		if _, err := s.Put(ctxFor(t), "sized/kept-"+tc.name, strings.NewReader(tc.data), storage.PutOptions{Size: storage.KnownSize(tc.size), ContentType: "image/png"}); !errors.Is(err, storage.ErrSizeMismatch) {
			t.Fatalf("a mismatched overwrite = %v, want storage.ErrSizeMismatch", err)
		}
		if got, info := read(t, s, "sized/kept-"+tc.name); string(got) != "previous version" || info.ContentType != "text/plain" {
			t.Fatalf("a mismatched overwrite left %q (%s), want the previous object", got, info.ContentType)
		}
	}
	if _, err := s.Put(ctxFor(t), "sized/not-empty", strings.NewReader("x"), storage.PutOptions{Size: storage.KnownSize(0)}); !errors.Is(err, storage.ErrSizeMismatch) {
		t.Fatalf("Put of 1 byte declared empty = %v, want storage.ErrSizeMismatch", err)
	}
	wantNotFound(t, s, "sized/not-empty")
	if info := put(t, s, "sized/empty", nil, storage.PutOptions{Size: storage.KnownSize(0)}); info.Size != 0 {
		t.Fatalf("an empty Put declared empty reports size %d", info.Size)
	}
	info := put(t, s, "sized/exact", []byte("0123456789"), storage.PutOptions{Size: storage.KnownSize(10)})
	if info.Size != 10 {
		t.Fatalf("an exactly sized Put reports size %d, want 10", info.Size)
	}
}

func checkInvalidOptions(t testing.TB, s storage.Storage) {
	for _, opts := range []storage.PutOptions{
		{ContentType: "not a media type"},
		{ContentType: "text/"},
		{ContentType: "text"},
		{ContentType: "text/plain; x=" + strings.Repeat("y", storage.MaxContentTypeBytes)},
		{ContentType: "text/plain\r\n\r\n"},
		{ContentType: "text/plain\u2028"},
		{Size: storage.KnownSize(-1)},
		{Metadata: map[string]string{"Upper": "x"}},
		{Metadata: map[string]string{"under_score": "x"}},
		{Metadata: map[string]string{"": "x"}},
		{Metadata: map[string]string{"ok": "new\nline"}},
		{Metadata: map[string]string{"big": strings.Repeat("v", storage.MaxMetadataBytes)}},
		// 1400 UTF-8 bytes, over 2 KB once RFC 2047 encoded as S3 needs.
		{Metadata: map[string]string{"name": strings.Repeat("é", 700)}},
		{Metadata: map[string]string{"name": "a =? b"}},
		{Metadata: map[string]string{"name": "invoice\u202efdp.exe"}},
		{Metadata: map[string]string{"name": "line\u2028break"}},
		{Metadata: map[string]string{"name": " padded"}}, // HTTP trims header values
		{Metadata: map[string]string{"name": "padded "}},
	} {
		_, err := s.Put(ctxFor(t), "opts", strings.NewReader("x"), opts)
		if !errors.Is(err, storage.ErrInvalidOptions) {
			t.Fatalf("Put with %+v = %v, want storage.ErrInvalidOptions", opts, err)
		}
		wantErr(t, err, storage.ErrInvalidOptions, "put", "opts")
		wantNotFound(t, s, "opts")
	}
	// A refused overwrite leaves the previous object whole.
	put(t, s, "opts-kept", []byte("previous"), storage.PutOptions{ContentType: "text/plain"})
	if _, err := s.Put(ctxFor(t), "opts-kept", strings.NewReader("new"), storage.PutOptions{ContentType: "not a media type"}); !errors.Is(err, storage.ErrInvalidOptions) {
		t.Fatalf("an invalid overwrite = %v, want storage.ErrInvalidOptions", err)
	}
	if got, info := read(t, s, "opts-kept"); string(got) != "previous" || info.ContentType != "text/plain" {
		t.Fatalf("a refused overwrite left %q (%s), want the previous object", got, info.ContentType)
	}
	for _, opts := range []storage.URLOptions{storage.SignedURL(0), storage.SignedURL(-time.Second), {Expires: time.Minute}} {
		_, err := s.URL(ctxFor(t), "opts", opts)
		if !errors.Is(err, storage.ErrInvalidOptions) {
			t.Fatalf("URL(%+v) = %v, want storage.ErrInvalidOptions: a signed URL must never widen into a permanent one", opts, err)
		}
		wantErr(t, err, storage.ErrInvalidOptions, "url", "opts")
	}
	if _, err := s.URL(ctxFor(t), "opts", storage.SignedURL(storage.MaxURLExpiry+time.Second)); !errors.Is(err, storage.ErrInvalidOptions) {
		t.Fatalf("URL with a lifetime over storage.MaxURLExpiry = %v, want storage.ErrInvalidOptions", err)
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

// gate is a reader that blocks its first Read until release is closed,
// telling started when it is reached, and then reports EOF: between two
// byte readers in an io.MultiReader, it holds a Put in the middle of its
// source, whatever buffer size the driver reads with.
type gate struct {
	started, release chan struct{}
	once             sync.Once
}

func (g *gate) Read([]byte) (int, error) {
	g.once.Do(func() { close(g.started) })
	<-g.release
	return 0, io.EOF
}

// checkNoPartialReads: while a Put is streaming, a reader sees the previous
// object whole, never the part written so far; for a key stored for the
// first time, it sees no object at all.
func checkNoPartialReads(t testing.TB, s storage.Storage) {
	put(t, s, "streaming", []byte("the previous version"), storage.PutOptions{ContentType: "text/plain"})
	midPut(t, s, "streaming", func(data []byte, info storage.ObjectInfo, err error) {
		if err != nil {
			t.Fatalf("Open while a Put was streaming = %v", err)
		}
		if string(data) != "the previous version" || info.ContentType != "text/plain" {
			t.Fatalf("mid-Put, a reader saw %q (%s): part of the object being written", data, info.ContentType)
		}
	})
	midPut(t, s, "first-write", func(data []byte, _ storage.ObjectInfo, err error) {
		if err == nil {
			t.Fatalf("mid-Put of a new key, a reader saw %q: part of the object being written", data)
		}
		wantErr(t, err, storage.ErrNotFound, "open", "first-write")
	})
}

// midPut Puts to key from a source held in the middle, calls check with
// what an Open of key reads meanwhile, then lets the Put finish and
// requires the whole new object.
func midPut(t testing.TB, s storage.Storage, key string, check func([]byte, storage.ObjectInfo, error)) {
	t.Helper()
	g := &gate{started: make(chan struct{}), release: make(chan struct{})}
	defer func() {
		select {
		case <-g.release:
		default:
			close(g.release)
		}
	}()
	src := io.MultiReader(strings.NewReader("NEW-first-half"), g, strings.NewReader("-second-half"))
	done := make(chan error, 1)
	go func() {
		_, err := s.Put(ctxFor(t), key, src, storage.PutOptions{ContentType: "application/json"})
		done <- err
	}()
	select {
	case <-g.started:
	case err := <-done:
		t.Fatalf("the Put returned (%v) before reading past the first half of its source", err)
	case <-time.After(20 * time.Second):
		t.Fatal("the Put never read past the first half of its source")
	}
	// Read while the Put is held mid-source; bounded, so a driver that
	// blocks readers during a Put fails here rather than hanging.
	type result struct {
		data []byte
		info storage.ObjectInfo
		err  error
	}
	mid := make(chan result, 1)
	go func() {
		body, info, err := s.Open(ctxFor(t), key)
		if err != nil {
			mid <- result{err: err}
			return
		}
		data, err := io.ReadAll(body)
		_ = body.Close()
		mid <- result{data, info, err}
	}()
	var got result
	select {
	case got = <-mid:
	case <-time.After(20 * time.Second):
		close(g.release)
		t.Fatal("Open blocked while a Put was streaming: readers must see the previous object, not wait for the write")
	}
	close(g.release)
	if err := <-done; err != nil {
		t.Fatalf("Put = %v", err)
	}
	check(got.data, got.info, got.err)
	if final, _ := read(t, s, key); string(final) != "NEW-first-half-second-half" {
		t.Fatalf("after the Put the key holds %q", final)
	}
}

// checkMetadataIsOwned: Put copies the caller's metadata, and every
// ObjectInfo returned owns its map: changing any of them changes neither
// the stored object nor another result (an S3 driver cannot share them).
func checkMetadataIsOwned(t testing.TB, s storage.Storage) {
	md := map[string]string{"owner": "alice"}
	info := put(t, s, "owned", []byte("x"), storage.PutOptions{Metadata: md})
	md["owner"] = "changed through the Put input"
	if info.Metadata["owner"] != "alice" {
		t.Fatalf("changing the Put input changed the Put result to %q: the result shares the caller's map", info.Metadata["owner"])
	}
	info.Metadata["owner"] = "changed through the Put result"
	if md["owner"] != "changed through the Put input" {
		t.Fatalf("changing the Put result changed the caller's map to %q", md["owner"])
	}
	stat := func(label string) storage.ObjectInfo {
		t.Helper()
		st, err := s.Stat(ctxFor(t), "owned")
		if err != nil {
			t.Fatal(err)
		}
		if st.Metadata["owner"] != "alice" {
			t.Fatalf("after changing %s, Stat reports owner %q: the stored metadata changed without a Put", label, st.Metadata["owner"])
		}
		return st
	}
	stat("the Put input and result").Metadata["owner"] = "changed through a Stat result"
	body, opened, err := s.Open(ctxFor(t), "owned")
	if err != nil {
		t.Fatal(err)
	}
	_ = body.Close()
	stat("a Stat result")
	opened.Metadata["owner"] = "changed through an Open result"
	stat("an Open result")
}

// checkOpenFollowsContext: the reader Open returns stays bound to its
// context: once the context ends, reading fails with its error (as an HTTP
// response body does), on every driver.
func checkOpenFollowsContext(t testing.TB, s storage.Storage) {
	put(t, s, "bound", bytes.Repeat([]byte("b"), 1<<20), storage.PutOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body, _, err := s.Open(ctx, "bound")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = body.Close() }()
	if _, err := io.ReadFull(body, make([]byte, 16)); err != nil {
		t.Fatalf("reading before the context ended = %v", err)
	}
	cancel()
	if _, err := io.Copy(io.Discard, body); !errors.Is(err, context.Canceled) {
		t.Fatalf("reading after Open's context ended = %v, want context.Canceled: the reader must follow the context it was opened with", err)
	}
}

// checkURL: a driver either produces URLs or says it cannot; it never
// returns an empty URL without an error, and it never checks that the
// object exists (a URL for an upload names a key not stored yet). A public
// URL may be refused as storage.ErrNotPublic (the suite's keys are not
// under a public prefix); a signed one never is.
func checkURL(t testing.TB, s storage.Storage) {
	put(t, s, "linked/file.txt", []byte("x"), storage.PutOptions{})
	for _, key := range []string{"linked/file.txt", "linked/never-stored.txt"} {
		for _, opts := range []storage.URLOptions{storage.PublicURL(), storage.SignedURL(time.Minute)} {
			checkOneURL(t, s, key, opts)
		}
	}
}

func checkOneURL(t testing.TB, s storage.Storage, key string, opts storage.URLOptions) {
	t.Helper()
	u, err := s.URL(ctxFor(t), key, opts)
	switch {
	case errors.Is(err, storage.ErrUnsupported):
		wantErr(t, err, storage.ErrUnsupported, "url", key)
	case errors.Is(err, storage.ErrNotPublic) && !opts.Signed:
		wantErr(t, err, storage.ErrNotPublic, "url", key)
	case errors.Is(err, storage.ErrNotPublic):
		t.Fatalf("URL(%q, %+v) = %v: a signed URL works for a private object", key, opts, err)
	case errors.Is(err, storage.ErrNotFound):
		t.Fatalf("URL(%q, %+v) = %v: URL must not check that the object exists", key, opts, err)
	case err != nil:
		t.Fatalf("URL(%q, %+v) = %v, want a URL or storage.ErrUnsupported", key, opts, err)
	case u == "":
		t.Fatalf("URL(%q, %+v) returned an empty URL and no error", key, opts)
	}
}

// checkDirectUpload: a store that offers direct uploads
// (storage.DirectUploader) validates the key and the options as URL and
// Put do, and either says it cannot or returns a request to make: a
// method, a URL, and a lifetime within the one asked for. Whether the
// backend then enforces the grant needs a real HTTP round trip, which each
// driver's own tests make.
func checkDirectUpload(t testing.TB, s storage.Storage) {
	d, ok := s.(storage.DirectUploader)
	if !ok {
		return
	}
	valid := storage.UploadURLOptions{Expires: time.Minute, Size: 3, ContentType: "text/plain"}
	for _, key := range invalidKeys {
		_, err := d.UploadURL(ctxFor(t), key, valid)
		wantErr(t, err, storage.ErrInvalidKey, "upload url", key)
	}
	for _, opts := range []storage.UploadURLOptions{
		{Expires: 0, Size: 3},
		{Expires: -time.Minute, Size: 3},
		{Expires: storage.MaxURLExpiry + time.Second, Size: 3},
		{Expires: time.Minute, Size: -1},
		{Expires: time.Minute, ContentType: "not a media type"},
		{Expires: time.Minute, Metadata: map[string]string{"Upper": "x"}},
	} {
		_, err := d.UploadURL(ctxFor(t), "direct", opts)
		if !errors.Is(err, storage.ErrInvalidOptions) {
			t.Fatalf("UploadURL with %+v = %v, want storage.ErrInvalidOptions", opts, err)
		}
		wantErr(t, err, storage.ErrInvalidOptions, "upload url", "direct")
	}
	before := time.Now()
	req, err := d.UploadURL(ctxFor(t), "direct/never-stored", valid)
	switch {
	case errors.Is(err, storage.ErrUnsupported):
		return
	case err != nil:
		t.Fatalf("UploadURL = %v, want a request or storage.ErrUnsupported", err)
	case req.Method == "" || req.URL == "":
		t.Fatalf("UploadURL = %+v: no method or URL", req)
	case req.Expires.Before(before) || req.Expires.After(time.Now().Add(valid.Expires+time.Second)):
		t.Fatalf("UploadURL expires at %s, want within %s", req.Expires, valid.Expires)
	}
	wantNotFound(t, s, "direct/never-stored") // asking for a grant stores nothing
}

// checkList: a store that lists its objects (storage.Lister) lists exactly
// those under the prefix, each once with its key, size, and modification
// time; stops at fn's error and returns it; lets fn delete what it is
// given; and honors an ended context.
func checkList(t testing.TB, s storage.Storage) {
	l, ok := s.(storage.Lister)
	if !ok {
		return
	}
	for _, key := range []string{"list/a", "list/b/c", "list/b/d", "listing", "other/list/a"} {
		put(t, s, key, []byte("bytes of "+key), storage.PutOptions{})
	}
	collect := func(prefix string) map[string]storage.ObjectInfo {
		got := map[string]storage.ObjectInfo{}
		if err := l.List(ctxFor(t), prefix, func(o storage.ObjectInfo) error {
			if _, dup := got[o.Key]; dup {
				t.Fatalf("List(%q) gave %q twice", prefix, o.Key)
			}
			got[o.Key] = o
			return nil
		}); err != nil {
			t.Fatalf("List(%q) = %v", prefix, err)
		}
		return got
	}
	got := collect("list/")
	if len(got) != 3 {
		t.Fatalf("List(\"list/\") = %v, want exactly list/a, list/b/c, list/b/d", keysOf(got))
	}
	for _, key := range []string{"list/a", "list/b/c", "list/b/d"} {
		o, ok := got[key]
		if !ok || o.Size != int64(len("bytes of "+key)) || o.ModTime.IsZero() {
			t.Fatalf("List(\"list/\") gave %q as %+v", key, o)
		}
		st, err := s.Stat(ctxFor(t), key)
		if err != nil {
			t.Fatal(err)
		}
		// To the second: an S3 listing has milliseconds, its HEAD not.
		if o.ETag != st.ETag || !o.ModTime.Truncate(time.Second).Equal(st.ModTime.Truncate(time.Second)) {
			t.Fatalf("List gave %q ETag %q at %s; Stat says %q at %s (Sweep decides on these)", key, o.ETag, o.ModTime, st.ETag, st.ModTime)
		}
	}
	if all := collect(""); len(all) < 5 {
		t.Fatalf("List(\"\") = %v, want every object", keysOf(all))
	}
	stop := errors.New("stop here")
	calls := 0
	if err := l.List(ctxFor(t), "list/", func(storage.ObjectInfo) error { calls++; return stop }); !errors.Is(err, stop) || calls != 1 {
		t.Fatalf("List stopped by fn = %v after %d calls, want fn's error after 1", err, calls)
	}
	if err := l.List(ctxFor(t), "list/b/", func(o storage.ObjectInfo) error { return s.Delete(ctxFor(t), o.Key) }); err != nil {
		t.Fatalf("List deleting as it goes = %v", err)
	}
	if left := collect("list/"); len(left) != 1 {
		t.Fatalf("after deleting list/b/*, List = %v", keysOf(left))
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	wantErr(t, l.List(canceled, "list/", func(storage.ObjectInfo) error { return nil }), context.Canceled, "list", "list/")
}

func keysOf(m map[string]storage.ObjectInfo) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
