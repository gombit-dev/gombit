package storage_test

import (
	"context"
	"errors"
	"fmt"
	"go/build"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/storage"
)

func TestValidateKey(t *testing.T) {
	valid := []string{
		"a",
		"avatars/42.png",
		"one/two/three/four.txt",
		"spaces in name/file (1).pdf",
		"ünïcødé/数据.bin",
		"dots/..hidden/.also/name..ext",
		"trailing dot inside/a.b/ leading space",
		"c:/drive-like/segment",
		strings.Repeat("k", storage.MaxSegmentBytes) + "/" + strings.Repeat("k", storage.MaxSegmentBytes),
	}
	for _, key := range valid {
		if err := storage.ValidateKey(key); err != nil {
			t.Errorf("ValidateKey(%q) = %v, want nil", key, err)
		}
	}
	invalid := map[string]string{
		"":                "empty",
		"/abs":            "starts with '/'",
		"trailing/":       "ends with '/'",
		"a//b":            "empty segment",
		".":               `"." segment`,
		"..":              `".." segment`,
		"../escape":       `".." segment`,
		"a/../../escape":  `".." segment`,
		"a/./b":           `"." segment`,
		`a\b`:             "backslash",
		"nul\x00byte":     "control character",
		"new\nline":       "control character",
		"del\x7fchar":     "control character",
		"bad\xffutf8":     "UTF-8",
		"c1\u0085control": "U+0085",
		"bidi\u202eflip":  "U+202E",
		"line\u2028break": "U+2028",
		"para\u2029break": "U+2029",
		"lrm\u200emark":   "U+200E",
		"...":             "ending with '.'",
		".. ":             "ending with ' '",
		"foo/.. /bar":     "ending with ' '",
		"a./b":            "ending with '.'",
		"a /b":            "ending with ' '",
		"name.":           "ending with '.'",
		strings.Repeat("k", storage.MaxKeyBytes+1):                   "longer than",
		"a/" + strings.Repeat("s", storage.MaxSegmentBytes+1) + "/b": "segment, longer than",
	}
	for key, reason := range invalid {
		err := storage.ValidateKey(key)
		if !errors.Is(err, storage.ErrInvalidKey) || !strings.Contains(err.Error(), reason) {
			t.Errorf("ValidateKey(%q) = %v, want storage.ErrInvalidKey mentioning %q", key, err, reason)
		}
	}
}

func TestValidatePutOptions(t *testing.T) {
	ok := []storage.PutOptions{
		{},
		{ContentType: "image/png", Size: storage.KnownSize(10)},
		{Size: storage.KnownSize(0)},
		{ContentType: "text/plain; charset=utf-8"},
		{Metadata: map[string]string{"original-name": "résumé final.pdf", "v2": ""}},
		// "x-amz-meta-n" (12) + 2036 = 2048, as MinIO counts it.
		{Metadata: map[string]string{"n": strings.Repeat("v", 2036)}},
		{Metadata: map[string]string{"n": strings.Repeat("é", maxRunes("é"))}},
		// Printable ASCII is sent as is: '%' counts one byte.
		{Metadata: map[string]string{"n": strings.Repeat("%", 2036)}},
	}
	for _, opts := range ok {
		if err := storage.ValidatePutOptions(opts); err != nil {
			t.Errorf("ValidatePutOptions(%+v) = %v, want nil", opts, err)
		}
	}
	bad := []storage.PutOptions{
		{ContentType: "not a media type"},
		{ContentType: "text/"},
		{ContentType: "text"},
		{ContentType: "/plain"},
		{ContentType: "a/b/c"},
		{ContentType: "text/plain\r\n\r\n"},
		{ContentType: "text/plain;\r\n charset=utf-8"},
		{ContentType: "text/plain\u2028"},
		{ContentType: "\u2028text/plain"},
		{ContentType: "text/plain\x05"},
		{ContentType: "text/plain; x=" + strings.Repeat("y", storage.MaxContentTypeBytes)},
		{Size: storage.KnownSize(-1)},
		{Metadata: map[string]string{"": "x"}},
		{Metadata: map[string]string{"Upper": "x"}},
		{Metadata: map[string]string{"under_score": "x"}},
		{Metadata: map[string]string{"ok": "tab\there"}},
		{Metadata: map[string]string{"ok": "bad\xffutf8"}},
		{Metadata: map[string]string{"n": strings.Repeat("v", storage.MaxMetadataBytes)}},
		{Metadata: map[string]string{"n": strings.Repeat("é", maxRunes("é")+1)}},
		{Metadata: map[string]string{"n": strings.Repeat("v", 2037)}},
		// "=?" starts an RFC 2047 encoded word.
		{Metadata: map[string]string{"n": "a =? b"}},
		{Metadata: map[string]string{"n": "bidi\u202eflip"}},
	}
	for _, opts := range bad {
		if err := storage.ValidatePutOptions(opts); !errors.Is(err, storage.ErrInvalidOptions) {
			t.Errorf("ValidatePutOptions(%+v) = %v, want storage.ErrInvalidOptions", opts, err)
		}
	}
	if err := storage.ValidateURLOptions(storage.SignedURL(time.Minute)); err != nil {
		t.Errorf("ValidateURLOptions(SignedURL) = %v", err)
	}
	if err := storage.ValidateURLOptions(storage.PublicURL()); err != nil {
		t.Errorf("ValidateURLOptions(PublicURL) = %v", err)
	}
	// A signed URL never widens into a permanent one: a zero or negative
	// lifetime is refused, not treated as public.
	for _, opts := range []storage.URLOptions{
		storage.SignedURL(0),
		storage.SignedURL(-time.Second),
		{Expires: time.Minute}, // a lifetime on a public URL
	} {
		if err := storage.ValidateURLOptions(opts); !errors.Is(err, storage.ErrInvalidOptions) {
			t.Errorf("ValidateURLOptions(%+v) = %v, want storage.ErrInvalidOptions", opts, err)
		}
	}
	if storage.SignedURL(0) == storage.PublicURL() {
		t.Error("SignedURL(0) is the same request as PublicURL()")
	}
}

func TestExpectSize(t *testing.T) {
	read := func(r io.Reader) (string, error) {
		b, err := io.ReadAll(r)
		return string(b), err
	}
	if got, err := read(storage.ExpectSize(strings.NewReader("12345"), 5)); err != nil || got != "12345" {
		t.Errorf("exact = %q, %v", got, err)
	}
	if _, err := read(storage.ExpectSize(strings.NewReader("123"), 5)); !errors.Is(err, storage.ErrSizeMismatch) {
		t.Errorf("short = %v, want storage.ErrSizeMismatch", err)
	}
	if _, err := read(storage.ExpectSize(strings.NewReader("1234567"), 5)); !errors.Is(err, storage.ErrSizeMismatch) {
		t.Errorf("long = %v, want storage.ErrSizeMismatch", err)
	}
	// One byte at a time, and a reader that returns data with EOF.
	if got, err := read(storage.ExpectSize(iotest.OneByteReader(strings.NewReader("12345")), 5)); err != nil || got != "12345" {
		t.Errorf("one byte at a time = %q, %v", got, err)
	}
	if got, err := read(storage.ExpectSize(iotest.DataErrReader(strings.NewReader("12345")), 5)); err != nil || got != "12345" {
		t.Errorf("data with EOF = %q, %v", got, err)
	}
	if _, err := read(storage.ExpectSize(iotest.DataErrReader(strings.NewReader("123456")), 5)); !errors.Is(err, storage.ErrSizeMismatch) {
		t.Errorf("long, data with EOF = %v, want storage.ErrSizeMismatch", err)
	}
	// A source error passes through unchanged.
	boom := errors.New("boom")
	if _, err := read(storage.ExpectSize(iotest.ErrReader(boom), 5)); !errors.Is(err, boom) {
		t.Errorf("source error = %v, want it passed through", err)
	}
	// Zero is an exact empty size, not "unknown".
	if got, err := read(storage.ExpectSize(strings.NewReader(""), 0)); err != nil || got != "" {
		t.Errorf("empty, size 0 = %q, %v", got, err)
	}
	if _, err := read(storage.ExpectSize(strings.NewReader("x"), 0)); !errors.Is(err, storage.ErrSizeMismatch) {
		t.Errorf("one byte, size 0 = %v, want storage.ErrSizeMismatch", err)
	}
	if _, err := read(storage.ExpectSize(strings.NewReader("x"), -1)); !errors.Is(err, storage.ErrInvalidOptions) {
		t.Errorf("negative size = %v, want storage.ErrInvalidOptions", err)
	}
	// The read that reaches the size reports excess itself: a caller that
	// reads exactly the declared length in one Read still sees it.
	buf := make([]byte, 10)
	if n, err := storage.ExpectSize(strings.NewReader("123456789012345"), 10).Read(buf); n != 10 || !errors.Is(err, storage.ErrSizeMismatch) {
		t.Errorf("one Read of the declared length from a long source = %d, %v; want 10 and storage.ErrSizeMismatch", n, err)
	}
	if n, err := storage.ExpectSize(strings.NewReader("1234567890"), 10).Read(buf); n != 10 || err != io.EOF {
		t.Errorf("one Read of an exact source = %d, %v; want 10 and io.EOF", n, err)
	}
	if got := storage.SizeFromContentLength(-1); got != nil {
		t.Errorf("SizeFromContentLength(-1) = %d, want nil (unknown)", *got)
	}
	if got := storage.SizeFromContentLength(0); got == nil || *got != 0 {
		t.Error("SizeFromContentLength(0) should be an exact empty size")
	}
}

func TestErrorAndWrap(t *testing.T) {
	err := storage.Wrap("open", "a/b", storage.ErrNotFound)
	if !errors.Is(err, storage.ErrNotFound) || err.Error() != `storage: open "a/b": storage: object not found` {
		t.Errorf("Wrap = %v", err)
	}
	var se *storage.Error
	if !errors.As(err, &se) || se.Op != "open" || se.Key != "a/b" {
		t.Errorf("errors.As = %+v", se)
	}
	if again := storage.Wrap("open", "a/b", err); again != err {
		t.Errorf("Wrap of its own envelope rewrapped it: %v", again)
	}
	// Another call's envelope is the cause: the outer one names this call.
	outer := storage.Wrap("put", "x", err)
	if !errors.As(outer, &se) || se.Op != "put" || se.Key != "x" || !errors.Is(outer, storage.ErrNotFound) {
		t.Errorf("Wrap of another call's envelope = %v (%+v)", outer, se)
	}
	if storage.Wrap("put", "x", nil) != nil {
		t.Error("Wrap(nil) != nil")
	}
}

func TestMapError(t *testing.T) {
	ctx := context.Background()
	cause := errors.New("s3: SlowDown: dial tcp 10.0.0.1:443: i/o timeout")
	cases := []struct {
		err    error
		status int
	}{
		{storage.Wrap("open", "k", storage.ErrNotFound), http.StatusNotFound},
		{storage.Wrap("put", "k", storage.ErrInvalidKey), http.StatusInternalServerError},
		{storage.Wrap("put", "k", context.DeadlineExceeded), http.StatusServiceUnavailable},
		{storage.Wrap("open", "k", context.Canceled), http.StatusServiceUnavailable},
		{storage.Wrap("put", "k", storage.ErrInvalidOptions), http.StatusUnprocessableEntity},
		{storage.Wrap("put", "k", storage.ErrSizeMismatch), http.StatusUnprocessableEntity},
		{storage.Wrap("put", "k", errors.Join(storage.ErrUnavailable, cause)), http.StatusServiceUnavailable},
		{storage.Wrap("url", "k", storage.ErrUnsupported), http.StatusInternalServerError},
		{cause, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		mapped := storage.MapError(ctx, tc.err, "file not found", "could not read the file")
		var env *contract.ErrorEnvelope
		if !errors.As(mapped, &env) {
			t.Fatalf("MapError(%v) = %T, want *contract.ErrorEnvelope", tc.err, mapped)
		}
		if env.GetStatus() != tc.status {
			t.Errorf("MapError(%v) status = %d, want %d", tc.err, env.GetStatus(), tc.status)
		}
		if strings.Contains(env.Body.Message, "10.0.0.1") {
			t.Errorf("MapError leaked the driver's error text: %q", env.Body.Message)
		}
	}
	if storage.MapError(ctx, nil, "", "") != nil {
		t.Error("MapError(nil) != nil")
	}
}

// stubStore answers Stat with a fixed error.
type stubStore struct {
	storage.Storage
	statErr error
}

func (s stubStore) Stat(context.Context, string) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{}, s.statErr
}

func TestExists(t *testing.T) {
	ctx := context.Background()
	if ok, err := storage.Exists(ctx, stubStore{}, "k"); !ok || err != nil {
		t.Errorf("present = %v, %v", ok, err)
	}
	if ok, err := storage.Exists(ctx, stubStore{statErr: storage.Wrap("stat", "k", storage.ErrNotFound)}, "k"); ok || err != nil {
		t.Errorf("missing = %v, %v; want false, nil", ok, err)
	}
	boom := storage.Wrap("stat", "k", storage.ErrUnavailable)
	if ok, err := storage.Exists(ctx, stubStore{statErr: boom}, "k"); ok || !errors.Is(err, storage.ErrUnavailable) {
		t.Errorf("failing = %v, %v; want false and the error", ok, err)
	}
}

// TestNoDriverDependencies: the contract package, which application code
// imports, depends on nothing but the standard library and Gombit's own
// contract package, so it can never pull an S3 SDK or a filesystem driver
// into an application.
func TestNoDriverDependencies(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range pkg.Imports {
		if imp == "github.com/gombit-dev/gombit/contract" || !strings.Contains(strings.Split(imp, "/")[0], ".") {
			continue
		}
		t.Errorf("package storage imports %s: the contract must not depend on a driver or third-party module", imp)
	}
}

// maxRunes is the most repetitions of r a one-letter metadata name ("n")
// can hold within MaxMetadataBytes, found with the contract's own measure.
func maxRunes(r string) int {
	n := 0
	for storage.ValidateMetadata(map[string]string{"n": strings.Repeat(r, n+1)}) == nil {
		n++
	}
	return n
}

func TestMetadataValueWireLen(t *testing.T) {
	for _, value := range []string{"", "plain ascii", "100% sure", "é", "tab\there", "résumé.pdf", strings.Repeat("é", 700)} {
		want := len(value)
		if strings.ContainsFunc(value, func(r rune) bool { return r < 0x20 || r > 0x7e }) {
			want = len(mime.BEncoding.Encode("UTF-8", value))
		}
		if got := storage.MetadataValueWireLen(value); got != want {
			t.Errorf("MetadataValueWireLen(%q) = %d, want %d", value, got, want)
		}
	}
	// The boundary is exactly MaxMetadataBytes as MinIO counts it.
	at := len("x-amz-meta-n") + storage.MetadataValueWireLen(strings.Repeat("é", maxRunes("é")))
	over := len("x-amz-meta-n") + storage.MetadataValueWireLen(strings.Repeat("é", maxRunes("é")+1))
	if at > storage.MaxMetadataBytes || over <= storage.MaxMetadataBytes {
		t.Fatalf("the accepted maximum measures %d and one more rune %d; want <= %d and > %d", at, over, storage.MaxMetadataBytes, storage.MaxMetadataBytes)
	}
}

// TestPaddedMetadataIsRefused: HTTP drops a header value's surrounding
// spaces, so such a value could not come back as given.
func TestPaddedMetadataIsRefused(t *testing.T) {
	for _, v := range []string{" padded", "padded ", " "} {
		if err := storage.ValidateMetadata(map[string]string{"n": v}); !errors.Is(err, storage.ErrInvalidOptions) {
			t.Errorf("ValidateMetadata(%q) = %v", v, err)
		}
	}
	for _, v := range []string{"two  spaces inside", "a b", ""} {
		if err := storage.ValidateMetadata(map[string]string{"n": v}); err != nil {
			t.Errorf("ValidateMetadata(%q) = %v", v, err)
		}
	}
}

// seekCloser is an io.ReadSeekCloser over a string.
type seekCloser struct{ *strings.Reader }

func (seekCloser) Close() error { return nil }

func TestContextReadCloser(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rc := storage.ContextReadCloser(ctx, seekCloser{strings.NewReader("0123456789")})
	s, ok := rc.(io.Seeker)
	if !ok {
		t.Fatal("a seekable reader lost Seek")
	}
	if _, err := s.Seek(5, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 2)
	if n, err := rc.Read(b); err != nil || string(b[:n]) != "56" {
		t.Fatalf("Read after Seek = %q, %v", b[:n], err)
	}
	cancel()
	if _, err := rc.Read(b); !errors.Is(err, context.Canceled) {
		t.Fatalf("Read after the context ended = %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	plain := storage.ContextReadCloser(context.Background(), io.NopCloser(strings.NewReader("x")))
	if _, ok := plain.(io.Seeker); ok {
		t.Fatal("a reader that cannot seek gained Seek")
	}
}

// blockingBody is a body whose Read blocks until it is closed, telling
// started when a Read has begun.
type blockingBody struct {
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func newBlockingBody() *blockingBody {
	return &blockingBody{started: make(chan struct{}), closed: make(chan struct{})}
}

func (b *blockingBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.closed
	return 0, io.ErrClosedPipe
}

func (b *blockingBody) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

// TestContextReadCloserInterruptsARead: a Read in progress (it has begun:
// the body said so) when the context ends returns the context's error,
// because the wrapper closes a body whose Close interrupts a Read.
func TestContextReadCloserInterruptsARead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	body := newBlockingBody()
	rc := storage.ContextReadCloser(ctx, body)
	done := make(chan error, 1)
	go func() {
		_, err := rc.Read(make([]byte, 8))
		done <- err
	}()
	<-body.started // the Read is in progress, past the context check
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the interrupted Read = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a Read in progress was not interrupted when the context ended")
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close after the interruption = %v", err)
	}
}

// stubbornBody's Read blocks until release, whatever Close does: a reader
// whose Close cannot interrupt a Read.
type stubbornBody struct {
	started, release chan struct{}
	once             sync.Once
}

func (b *stubbornBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return copy(p, "late"), nil
}

func (*stubbornBody) Close() error { return nil }

// TestContextReadCloserWithoutInterruption: over a reader whose Close does
// not end a Read, a Read blocked when the context ends returns when the
// reader lets it, and then with the context's error, not the late data.
func TestContextReadCloserWithoutInterruption(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	body := &stubbornBody{started: make(chan struct{}), release: make(chan struct{})}
	rc := storage.ContextReadCloser(ctx, body)
	done := make(chan error, 1)
	go func() {
		n, err := rc.Read(make([]byte, 8))
		if n != 0 {
			err = fmt.Errorf("returned %d late bytes (%v)", n, err)
		}
		done <- err
	}()
	<-body.started
	cancel()
	select {
	case err := <-done:
		t.Fatalf("the Read returned (%v) before the reader let it", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(body.release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("the late Read = %v, want context.Canceled", err)
	}
}
