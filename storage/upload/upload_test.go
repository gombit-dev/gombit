package upload_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/storage"
	"github.com/gombit-dev/gombit/storage/local"
	"github.com/gombit-dev/gombit/storage/memory"
	"github.com/gombit-dev/gombit/storage/upload"
)

// png is the start of a PNG file: enough for content sniffing.
var png = append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), bytes.Repeat([]byte{7}, 200)...)

var images = upload.Policy{MaxBytes: 1 << 20, Types: []string{"image/png", "image/jpeg"}, Prefix: "avatars/"}

// part is one part of a multipart request.
type part struct {
	field, filename, contentType string
	body                         []byte
}

// form returns a multipart/form-data request with parts.
func form(t *testing.T, parts ...part) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, p := range parts {
		h := textproto.MIMEHeader{}
		disposition := fmt.Sprintf(`form-data; name=%q`, p.field)
		if p.filename != "" || p.contentType != "" {
			disposition += fmt.Sprintf(`; filename=%q`, p.filename)
		}
		h.Set("Content-Disposition", disposition)
		if p.contentType != "" {
			h.Set("Content-Type", p.contentType)
		}
		pw, err := w.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = pw.Write(p.body)
	}
	_ = w.Close()
	r := httptest.NewRequest(http.MethodPost, "/upload", &buf)
	r.Header.Set("Content-Type", w.FormDataContentType())
	return r
}

var generatedKey = regexp.MustCompile(`^avatars/[0-9a-f]{32}$`)

func TestReceiveStoresTheFile(t *testing.T) {
	store := memory.New()
	r := form(t,
		part{field: "title", body: []byte("my avatar")},
		part{field: "file", filename: "../../etc/résumé.png", contentType: "text/html", body: png},
		part{field: "csrf", body: []byte("token")},
	)
	f, err := upload.Receive(store, r, images)
	if err != nil {
		t.Fatal(err)
	}
	if !generatedKey.MatchString(f.Key) {
		t.Fatalf("key = %q, want a generated key under the prefix", f.Key)
	}
	if f.ContentType != "image/png" || f.Filename() != "résumé.png" || f.Size != int64(len(png)) {
		t.Fatalf("file = %+v", f)
	}
	body, info, err := store.Open(context.Background(), f.Key)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(body)
	if !bytes.Equal(got, png) || info.ContentType != "image/png" || info.Metadata[upload.FilenameMetadata] != "résumé.png" {
		t.Fatalf("stored %d bytes of %q, metadata %v", len(got), info.ContentType, info.Metadata)
	}
	if keys := store.Keys(); len(keys) != 1 {
		t.Fatalf("stored %v, want one object", keys)
	}
}

// TestFilenamesCannotTraverse: whatever the client calls the file, the key
// is the server's, and the filename kept is one plain name.
func TestFilenamesCannotTraverse(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"photo.png", "photo.png"},
		{"../../etc/passwd", "passwd"},
		{`..\..\windows\system32\evil.png`, "evil.png"},
		{"C:\\Users\\me\\photo.png", "photo.png"},
		{"a/b/../c.png", "c.png"},
		{"..", ""},
		{".", ""},
		{"/", ""},
		{"dir/", ""},
		{"  spaced.png  ", "spaced.png"},
		{"new\nline\r.png", "newline.png"},
		{"nul\x00.png", "nul.png"},
		{"gnp.\u202eexe", "gnp.exe"}, // a right-to-left override
		{"=?UTF-8?B?Zm9v?=.png", "=_UTF-8?B?Zm9v?=.png"},
		{"=\x00?x", "=_x"},
		{"bad\xffutf8.png", "bad\ufffdutf8.png"},
		{strings.Repeat("é", 200), strings.Repeat("é", 127)},
	} {
		if got := upload.CleanFilename(tc.in); got != tc.want {
			t.Errorf("CleanFilename(%q) = %q, want %q", tc.in, got, tc.want)
		}
		f, err := upload.Save(context.Background(), memory.New(), bytes.NewReader(png), tc.in, images)
		if err != nil {
			t.Fatalf("%q: %v", tc.in, err)
		}
		if !generatedKey.MatchString(f.Key) || f.Filename() != tc.want {
			t.Errorf("%q stored as %q named %q", tc.in, f.Key, f.Filename())
		}
		if err := storage.ValidateMetadata(map[string]string{upload.FilenameMetadata: f.Filename()}); err != nil {
			t.Errorf("%q: the cleaned name is not valid metadata: %v", tc.in, err)
		}
	}
}

// TestReceiveIgnoresTheClientsPath: a multipart filename with a path is
// stored under a generated key, named by its last element.
func TestReceiveIgnoresTheClientsPath(t *testing.T) {
	for _, name := range []string{"../../etc/passwd", `..\..\evil.png`, "/abs/x.png", ".."} {
		f, err := upload.Receive(memory.New(), form(t, part{field: "file", filename: name, body: png}), images)
		if err != nil {
			t.Fatalf("%q: %v", name, err)
		}
		if !generatedKey.MatchString(f.Key) || strings.ContainsAny(f.Filename(), `/\`) || f.Filename() == ".." {
			t.Errorf("%q stored as %q named %q", name, f.Key, f.Filename())
		}
	}
}

// TestTypeIsDetectedNotTrusted: the policy judges the bytes, not the
// client's Content-Type or the filename's extension.
func TestTypeIsDetectedNotTrusted(t *testing.T) {
	store := memory.New()
	html := []byte("<!DOCTYPE html><script>alert(1)</script>")
	_, err := upload.Receive(store, form(t, part{field: "file", filename: "cat.png", contentType: "image/png", body: html}), images)
	if !errors.Is(err, upload.ErrType) {
		t.Fatalf("HTML named .png with Content-Type image/png = %v, want ErrType", err)
	}
	if keys := store.Keys(); len(keys) != 0 {
		t.Fatalf("a refused file was stored: %v", keys)
	}
	f, err := upload.Receive(store, form(t, part{field: "file", filename: "cat.html", contentType: "text/html", body: png}), images)
	if err != nil || f.ContentType != "image/png" {
		t.Fatalf("a PNG named .html = %+v, %v; want it stored as image/png", f, err)
	}
}

func TestTypePatterns(t *testing.T) {
	jpeg := append([]byte("\xff\xd8\xff"), bytes.Repeat([]byte{1}, 50)...)
	for _, tc := range []struct {
		types []string
		body  []byte
		ok    bool
	}{
		{[]string{"image/*"}, jpeg, true},
		{[]string{"image/*"}, []byte("plain text"), false},
		{[]string{"text/plain"}, []byte("plain text"), true}, // detected as text/plain; charset=utf-8
		{[]string{"*/*"}, []byte("<html>"), true},
		{[]string{"image/png"}, jpeg, false},
		{[]string{"image/*"}, []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script/></svg>`), false}, // never image/svg+xml
		{[]string{"image/png"}, nil, false},                                                             // empty is text/plain
	} {
		p := upload.Policy{MaxBytes: 1 << 10, Types: tc.types}
		_, err := upload.Save(context.Background(), memory.New(), bytes.NewReader(tc.body), "", p)
		if ok := err == nil; ok != tc.ok || (!ok && !errors.Is(err, upload.ErrType)) {
			t.Errorf("%v accepting %.12q = %v, want ok=%v", tc.types, tc.body, err, tc.ok)
		}
	}
}

func TestCustomDetector(t *testing.T) {
	p := upload.Policy{MaxBytes: 1 << 10, Types: []string{"application/json"}, Detect: func(head []byte) string {
		if bytes.HasPrefix(bytes.TrimSpace(head), []byte("{")) {
			return "application/json"
		}
		return http.DetectContentType(head)
	}}
	f, err := upload.Save(context.Background(), memory.New(), strings.NewReader(`{"a":1}`), "a.json", p)
	if err != nil || f.ContentType != "application/json" {
		t.Fatalf("Save = %+v, %v", f, err)
	}
	p.Detect = func([]byte) string { return "not a media type" }
	if _, err := upload.Save(context.Background(), memory.New(), strings.NewReader(`{}`), "", p); !errors.Is(err, upload.ErrType) {
		t.Fatalf("a detector returning garbage = %v, want ErrType", err)
	}
}

// endless is a body that never ends, counting what was read from it.
type endless struct{ read int64 }

func (e *endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	e.read += int64(len(p))
	return len(p), nil
}

// endlessForm is a multipart request whose file part never ends.
func endlessForm(src *endless) *http.Request {
	head := "--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"big.png\"\r\n\r\n" + string(png)
	r := httptest.NewRequest(http.MethodPost, "/upload", io.MultiReader(strings.NewReader(head), src))
	r.ContentLength = -1
	r.Header.Set("Content-Type", "multipart/form-data; boundary=b")
	return r
}

// TestOversizedFailsBeforeUnboundedBuffering: a file over the limit is
// refused unread when its length is declared, and at the byte past the
// limit when it is not; the rest of an endless body is never read.
func TestOversizedFailsBeforeUnboundedBuffering(t *testing.T) {
	p := upload.Policy{MaxBytes: 64 << 10, Types: []string{"*/*"}}
	store := memory.New()

	unread := &endless{}
	r := httptest.NewRequest(http.MethodPut, "/upload", unread)
	r.ContentLength = p.MaxBytes + 1
	if _, err := upload.ReceiveBody(store, r, p); !errors.Is(err, upload.ErrTooLarge) || unread.read != 0 {
		t.Fatalf("a declared length over the limit = %v after reading %d bytes, want ErrTooLarge unread", err, unread.read)
	}
	r = form(t, part{field: "file", filename: "a.png", body: png})
	r.ContentLength = p.MaxBytes + upload.MaxFormBytes + 1
	if _, err := upload.Receive(store, r, p); !errors.Is(err, upload.ErrTooLarge) {
		t.Fatalf("a multipart request declared over the limit = %v", err)
	}

	src := &endless{}
	r = httptest.NewRequest(http.MethodPut, "/upload", src)
	r.ContentLength = -1
	if _, err := upload.ReceiveBody(store, r, p); !errors.Is(err, upload.ErrTooLarge) {
		t.Fatalf("an endless body = %v, want ErrTooLarge", err)
	}
	if src.read > p.MaxBytes+64<<10 {
		t.Fatalf("read %d bytes of an endless body with a %d-byte limit", src.read, p.MaxBytes)
	}

	src = &endless{}
	if _, err := upload.Receive(store, endlessForm(src), p); !errors.Is(err, upload.ErrTooLarge) {
		t.Fatalf("an endless multipart file = %v, want ErrTooLarge", err)
	}
	if src.read > p.MaxBytes+64<<10 {
		t.Fatalf("read %d bytes of an endless multipart file with a %d-byte limit", src.read, p.MaxBytes)
	}

	if keys := store.Keys(); len(keys) != 0 {
		t.Fatalf("oversized uploads were stored: %v", keys)
	}
	if err := upload.MapError(context.Background(), upload.ErrTooLarge); status(err) != http.StatusRequestEntityTooLarge {
		t.Fatalf("ErrTooLarge maps to %d", status(err))
	}
}

func TestLimitIsExact(t *testing.T) {
	p := upload.Policy{MaxBytes: 10, Types: []string{"*/*"}} // smaller than what detection reads
	store := memory.New()
	if _, err := upload.Save(context.Background(), store, strings.NewReader("0123456789"), "", p); err != nil {
		t.Fatalf("a file of exactly MaxBytes = %v", err)
	}
	if _, err := upload.Save(context.Background(), store, strings.NewReader("0123456789A"), "", p); !errors.Is(err, upload.ErrTooLarge) {
		t.Fatalf("a file of MaxBytes+1 = %v, want ErrTooLarge", err)
	}
	r := httptest.NewRequest(http.MethodPut, "/upload", strings.NewReader("0123456789"))
	if _, err := upload.ReceiveBody(store, r, p); err != nil {
		t.Fatalf("a body of exactly MaxBytes = %v", err)
	}
}

// files lists the regular files under root.
// files lists the objects and temporary files under a local store's root:
// every regular file but the store's own bookkeeping, the owner file each
// store keeps locked in its work directory and the root's .durable marker.
func files(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		if owner, _ := filepath.Match(filepath.Join(root, "tmp", "w-*", "owner"), path); !owner && path != filepath.Join(root, ".durable") {
			out = append(out, path)
		}
		return nil
	})
	return out
}

// failing is a reader that returns n bytes of data, then err.
type failing struct {
	data []byte
	err  error
}

func (f *failing) Read(p []byte) (int, error) {
	if len(f.data) == 0 {
		return 0, f.err
	}
	n := copy(p, f.data)
	f.data = f.data[n:]
	return n, nil
}

// goneAfter is a client that sends data, then disconnects: its context
// ends and the body fails, as a server's does.
type goneAfter struct {
	data   []byte
	cancel context.CancelFunc
}

func (g *goneAfter) Read(p []byte) (int, error) {
	if len(g.data) == 0 {
		g.cancel()
		return 0, errors.New("connection reset by peer")
	}
	n := copy(p, g.data)
	g.data = g.data[n:]
	return n, nil
}

// TestFailuresLeaveNothingBehind: on the local driver, every way an upload
// fails leaves no file on disk: not the object, not a temporary file.
func TestFailuresLeaveNothingBehind(t *testing.T) {
	root := t.TempDir()
	store, err := local.New(root)
	if err != nil {
		t.Fatal(err)
	}
	p := upload.Policy{MaxBytes: 256 << 10, Types: []string{"image/png"}}
	big := append(append([]byte{}, png...), bytes.Repeat([]byte{1}, 300<<10)...)
	half := append(append([]byte{}, png...), bytes.Repeat([]byte{1}, 100<<10)...)
	canceled, cancel := context.WithCancel(context.Background())
	defer cancel()

	for name, tc := range map[string]struct {
		req  func() *http.Request
		want error
	}{
		"too large": {func() *http.Request { return form(t, part{field: "file", filename: "a.png", body: big}) }, upload.ErrTooLarge},
		"wrong type": {func() *http.Request {
			return form(t, part{field: "file", filename: "a.png", body: []byte("<html>")})
		}, upload.ErrType},
		"body breaks mid-file": {func() *http.Request {
			src := &failing{data: []byte("--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.png\"\r\n\r\n" + string(half)), err: errors.New("connection reset")}
			r := httptest.NewRequest(http.MethodPost, "/upload", src)
			r.Header.Set("Content-Type", "multipart/form-data; boundary=b")
			return r
		}, upload.ErrMalformed},
		"truncated form": {func() *http.Request {
			full := form(t, part{field: "file", filename: "a.png", body: half})
			b, _ := io.ReadAll(full.Body)
			r := httptest.NewRequest(http.MethodPost, "/upload", bytes.NewReader(b[:len(b)-200]))
			r.Header = full.Header
			return r
		}, upload.ErrMalformed},
		"second file after the first was stored": {func() *http.Request {
			return form(t, part{field: "file", filename: "a.png", body: png}, part{field: "file", filename: "b.png", body: png})
		}, upload.ErrMalformed},
		"broken trailing part after the file was stored": {func() *http.Request {
			full := form(t, part{field: "file", filename: "a.png", body: png}, part{field: "note", body: bytes.Repeat([]byte("n"), 1000)})
			b, _ := io.ReadAll(full.Body)
			r := httptest.NewRequest(http.MethodPost, "/upload", bytes.NewReader(b[:len(b)-100]))
			r.Header = full.Header
			return r
		}, upload.ErrMalformed},
		"trailing fields over the form limit": {func() *http.Request {
			return form(t, part{field: "file", filename: "a.png", body: png}, part{field: "note", body: bytes.Repeat([]byte("n"), int(upload.MaxFormBytes+p.MaxBytes))})
		}, upload.ErrTooLarge},
		"trailing fields over the form limit, chunked": {func() *http.Request {
			r := form(t, part{field: "file", filename: "a.png", body: png}, part{field: "note", body: bytes.Repeat([]byte("n"), int(upload.MaxFormBytes+p.MaxBytes))})
			r.ContentLength = -1
			return r
		}, upload.ErrTooLarge},
		"fields before the file use up the request's limit": {func() *http.Request {
			file := append(append([]byte{}, png...), bytes.Repeat([]byte{1}, int(p.MaxBytes)-len(png))...)
			r := form(t, part{field: "note", body: bytes.Repeat([]byte("n"), upload.MaxFormBytes-100)}, part{field: "file", filename: "a.png", body: file})
			r.ContentLength = -1
			return r
		}, upload.ErrTooLarge},
		"client went away after the file was stored": {func() *http.Request {
			ctx, cancel := context.WithCancel(context.Background())
			full := form(t, part{field: "file", filename: "a.png", body: png}, part{field: "note", body: bytes.Repeat([]byte("n"), 1000)})
			b, _ := io.ReadAll(full.Body)
			cut := bytes.Index(b, []byte(`name="note"`))
			r := httptest.NewRequest(http.MethodPost, "/upload", &goneAfter{data: b[:cut], cancel: cancel})
			r.Header = full.Header
			return r.WithContext(ctx)
		}, upload.ErrMalformed},
		"client went away": {func() *http.Request {
			return form(t, part{field: "file", filename: "a.png", body: half}).WithContext(canceled)
		}, context.Canceled},
	} {
		if name == "client went away" {
			cancel()
		}
		_, err := upload.Receive(store, tc.req(), p)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: Receive = %v, want %v", name, err, tc.want)
		}
		if tc.want == upload.ErrTooLarge && errors.Is(err, upload.ErrMalformed) {
			t.Errorf("%s: %v is both ErrTooLarge and ErrMalformed", name, err)
		}
		if left := files(t, root); len(left) != 0 {
			t.Errorf("%s: left %v on disk", name, left)
		}
	}
}

func TestReceiveBody(t *testing.T) {
	store := memory.New()
	r := httptest.NewRequest(http.MethodPut, "/upload", bytes.NewReader(png))
	r.Header.Set("Content-Type", "text/html")
	r.Header.Set("Content-Disposition", `attachment; filename="../me.png"`)
	f, err := upload.ReceiveBody(store, r, images)
	if err != nil {
		t.Fatal(err)
	}
	if !generatedKey.MatchString(f.Key) || f.ContentType != "image/png" || f.Filename() != "me.png" || f.Size != int64(len(png)) {
		t.Fatalf("file = %+v", f)
	}

	r = httptest.NewRequest(http.MethodPut, "/upload", bytes.NewReader(png))
	r.ContentLength = int64(len(png)) + 10 // the body is shorter than declared
	if _, err := upload.ReceiveBody(store, r, images); !errors.Is(err, storage.ErrSizeMismatch) {
		t.Fatalf("a short body = %v, want storage.ErrSizeMismatch", err)
	}
	if n := len(store.Keys()); n != 1 {
		t.Fatalf("%d objects stored, want 1", n)
	}
}

func TestNoFile(t *testing.T) {
	store := memory.New()
	for name, r := range map[string]*http.Request{
		"no parts":        form(t),
		"only fields":     form(t, part{field: "title", body: []byte("x")}),
		"other field":     form(t, part{field: "photo", filename: "a.png", body: png}),
		"empty input":     form(t, part{field: "file", filename: "", contentType: "application/octet-stream"}),
		"field, not file": form(t, part{field: "file", body: png}),
	} {
		if _, err := upload.Receive(store, r, images); !errors.Is(err, upload.ErrNoFile) {
			t.Errorf("%s: Receive = %v, want ErrNoFile", name, err)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/upload", bytes.NewReader(png))
	r.Header.Set("Content-Type", "image/png")
	if _, err := upload.Receive(store, r, images); !errors.Is(err, upload.ErrMalformed) {
		t.Errorf("a request that is not multipart = %v, want ErrMalformed", err)
	}
	p := images
	p.Field = "photo"
	if _, err := upload.Receive(store, form(t, part{field: "photo", filename: "a.png", body: png}), p); err != nil {
		t.Errorf("Policy.Field = %v", err)
	}
}

func TestPolicyIsValidated(t *testing.T) {
	for _, p := range []upload.Policy{
		{Types: []string{"*/*"}},
		{MaxBytes: 1},
		{MaxBytes: 1, Types: []string{"image"}},
		{MaxBytes: 1, Types: []string{"Image/PNG"}},
		{MaxBytes: 1, Types: []string{"*/png"}},
		{MaxBytes: 1, Types: []string{"text/plain; charset=utf-8"}},
		// Not media types, or wildcards other than "type/*" and "*/*": none
		// could ever match a detected type.
		{MaxBytes: 1, Types: []string{"image/png/garbage"}},
		{MaxBytes: 1, Types: []string{"image/p@ng"}},
		{MaxBytes: 1, Types: []string{"image/*suffix"}},
		{MaxBytes: 1, Types: []string{"image/**"}},
		{MaxBytes: 1, Types: []string{"ima*ge/png"}},
		{MaxBytes: 1, Types: []string{"image/png\n"}},
		{MaxBytes: 1, Types: []string{" image/png"}},
		{MaxBytes: 1, Types: []string{"image/"}},
		{MaxBytes: 1, Types: []string{"/png"}},
		{MaxBytes: 1, Types: []string{"image/png;"}},
		{MaxBytes: 1, Types: []string{"image/png", "text/(plain)"}},
		{MaxBytes: 1, Types: []string{"*/*"}, Prefix: "avatars"},
		{MaxBytes: 1, Types: []string{"*/*"}, Prefix: "../avatars/"},
		{MaxBytes: 1, Types: []string{"*/*"}, Metadata: map[string]string{upload.FilenameMetadata: "x"}},
		{MaxBytes: 1, Types: []string{"*/*"}, Metadata: map[string]string{"Bad Name": "x"}},
		{MaxBytes: math.MaxInt64, Types: []string{"*/*"}}, // the request's limit would overflow
	} {
		_, err := upload.Save(context.Background(), memory.New(), strings.NewReader("x"), "", p)
		if err == nil {
			t.Errorf("Save with %+v succeeded", p)
		} else if status(upload.MapError(context.Background(), err)) != http.StatusInternalServerError {
			t.Errorf("an invalid policy (%v) maps to %d, want 500: it is the server's fault", err, status(upload.MapError(context.Background(), err)))
		}
	}
}

// TestLongFilenameFitsTheMetadata: a filename that would push the policy's
// metadata over the limit is shortened, not a failed upload.
func TestLongFilenameFitsTheMetadata(t *testing.T) {
	p := upload.Policy{MaxBytes: 1 << 10, Types: []string{"*/*"}, Metadata: map[string]string{"note": strings.Repeat("n", 1900)}}
	f, err := upload.Save(context.Background(), memory.New(), strings.NewReader("x"), strings.Repeat("é", 120), p)
	if err != nil {
		t.Fatal(err)
	}
	if f.Filename() == "" || len(f.Filename()) >= 240 || f.Metadata[upload.FilenameMetadata] != f.Filename() {
		t.Fatalf("filename %d bytes, metadata %d bytes", len(f.Filename()), len(f.Metadata[upload.FilenameMetadata]))
	}
	if err := storage.ValidateMetadata(f.Metadata); err != nil {
		t.Fatal(err)
	}
}

func status(err error) int {
	var env *contract.ErrorEnvelope
	if !errors.As(err, &env) {
		return 0
	}
	return env.GetStatus()
}

func TestMapError(t *testing.T) {
	ctx := context.Background()
	for err, want := range map[error]int{
		upload.ErrTooLarge:                          http.StatusRequestEntityTooLarge,
		&http.MaxBytesError{Limit: 1}:               http.StatusRequestEntityTooLarge,
		fmt.Errorf("x: %w", upload.ErrType):         http.StatusUnprocessableEntity,
		upload.ErrNoFile:                            http.StatusUnprocessableEntity,
		upload.ErrMalformed:                         http.StatusUnprocessableEntity,
		storage.ErrSizeMismatch:                     http.StatusUnprocessableEntity,
		&storage.Error{Err: storage.ErrUnavailable}: http.StatusServiceUnavailable,
		errors.New("disk on fire"):                  http.StatusInternalServerError,
	} {
		if got := status(upload.MapError(ctx, err)); got != want {
			t.Errorf("MapError(%v) = %d, want %d", err, got, want)
		}
	}
	if upload.MapError(ctx, nil) != nil {
		t.Error("MapError(nil) != nil")
	}
}

// TestCleanFilenameMatchesTheMetadataRules: every character CleanFilename
// keeps is one metadata accepts, so a cleaned name is never dropped for a
// character the contract refuses.
func TestCleanFilenameMatchesTheMetadataRules(t *testing.T) {
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !utf8.ValidRune(r) {
			continue
		}
		name := upload.CleanFilename("a" + string(r) + "b")
		if err := storage.ValidateMetadata(map[string]string{upload.FilenameMetadata: name}); err != nil {
			t.Fatalf("CleanFilename kept %U, which metadata refuses: %v", r, err)
		}
	}
}

// TestClientGoneIsNotAServerError: a client that disconnects mid-file is
// the context's error (503), not an internal one.
func TestClientGoneIsNotAServerError(t *testing.T) {
	store, err := local.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := &goneAfter{data: append(append([]byte{}, png...), bytes.Repeat([]byte{1}, 100<<10)...), cancel: cancel}
	_, err = upload.Save(ctx, store, src, "a.png", images)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Save = %v, want context.Canceled", err)
	}
	if got := status(upload.MapError(ctx, err)); got != http.StatusServiceUnavailable {
		t.Fatalf("MapError = %d, want 503", got)
	}
}

// failingDelete is a store whose Delete fails.
type failingDelete struct {
	storage.Storage
	err error
}

func (f failingDelete) Delete(context.Context, string) error { return f.err }

func TestFailedCleanupIsReported(t *testing.T) {
	deleteErr := errors.New("delete failed")
	store := failingDelete{Storage: memory.New(), err: deleteErr}
	_, err := upload.Receive(store, form(t, part{field: "file", filename: "a.png", body: png}, part{field: "file", filename: "b.png", body: png}), images)
	if !errors.Is(err, upload.ErrMalformed) || !errors.Is(err, deleteErr) {
		t.Fatalf("Receive = %v, want ErrMalformed and the failed delete", err)
	}
	if got := status(upload.MapError(context.Background(), err)); got != http.StatusUnprocessableEntity {
		t.Fatalf("MapError = %d, want the upload's own error (422)", got)
	}
}

func TestReceiveMarksTheRequestRead(t *testing.T) {
	r := form(t, part{field: "file", filename: "a.png", body: png})
	if _, err := upload.Receive(memory.New(), r, images); err != nil {
		t.Fatal(err)
	}
	if err := r.ParseMultipartForm(1 << 20); err == nil || !strings.Contains(err.Error(), "MultipartReader") {
		t.Fatalf("ParseMultipartForm after Receive = %v, want Go's multipart-reader-used error", err)
	}
}

// TestBadDetectorIsTheServersFault: a detector returning a type the store
// cannot record is a 500, not the client's 422.
func TestBadDetectorIsTheServersFault(t *testing.T) {
	p := upload.Policy{MaxBytes: 1 << 10, Types: []string{"*/*"}, Detect: func([]byte) string {
		return "application/x-long; profile=" + strings.Repeat("p", 300)
	}}
	_, err := upload.Save(context.Background(), memory.New(), strings.NewReader("x"), "", p)
	if err == nil || errors.Is(err, upload.ErrType) {
		t.Fatalf("Save = %v", err)
	}
	if got := status(upload.MapError(context.Background(), err)); got != http.StatusInternalServerError {
		t.Fatalf("MapError = %d, want 500", got)
	}
}

// unknownOutcome stores the object, then reports that it does not know
// whether it did: a remote store whose answer was lost.
type unknownOutcome struct {
	*memory.Store
	deleteErr error
}

func (u unknownOutcome) Put(ctx context.Context, key string, r io.Reader, opts storage.PutOptions) (storage.ObjectInfo, error) {
	if _, err := u.Store.Put(ctx, key, r, opts); err != nil {
		return storage.ObjectInfo{}, err
	}
	return storage.ObjectInfo{}, storage.Wrap("put", key, errors.Join(storage.ErrUnknownOutcome, storage.ErrUnavailable))
}

func (u unknownOutcome) Delete(ctx context.Context, key string) error {
	if u.deleteErr != nil {
		return u.deleteErr
	}
	return u.Store.Delete(ctx, key)
}

// TestUnknownOutcomeIsCleanedUp: a Put whose outcome is unknown may have
// stored the file under its generated key; Save deletes the key, so
// nothing is left behind, and the failure no longer claims an unknown
// outcome. When that delete fails, the error names the key
// (*upload.CleanupError) and still reports the unknown outcome.
func TestUnknownOutcomeIsCleanedUp(t *testing.T) {
	ctx := context.Background()
	t.Run("deleted", func(t *testing.T) {
		store := unknownOutcome{Store: memory.New()}
		_, err := upload.Save(ctx, store, bytes.NewReader(png), "a.png", images)
		if err == nil || errors.Is(err, storage.ErrUnknownOutcome) || !errors.Is(err, storage.ErrUnavailable) {
			t.Fatalf("Save = %v; want the store's failure (ErrUnavailable), no longer of unknown outcome", err)
		}
		if keys := store.Keys(); len(keys) != 0 {
			t.Fatalf("the store holds %v after the failed Save, want nothing", keys)
		}
	})
	t.Run("delete fails", func(t *testing.T) {
		deleteErr := errors.New("delete failed")
		store := unknownOutcome{Store: memory.New(), deleteErr: deleteErr}
		_, err := upload.Save(ctx, store, bytes.NewReader(png), "a.png", images)
		var cleanup *upload.CleanupError
		if !errors.As(err, &cleanup) || !errors.Is(err, storage.ErrUnknownOutcome) || !errors.Is(cleanup.Err, deleteErr) {
			t.Fatalf("Save = %v; want ErrUnknownOutcome and an *upload.CleanupError carrying the delete's failure", err)
		}
		if keys := store.Keys(); len(keys) != 1 || keys[0] != cleanup.Key || !generatedKey.MatchString(cleanup.Key) {
			t.Fatalf("CleanupError.Key = %q, store holds %v; want the generated key of the stored file", cleanup.Key, keys)
		}
	})
}

// TestFormOverheadIsBounded: MaxFormBytes bounds everything in a multipart
// request but the file (part headers, boundaries, other fields), on its
// own, not just as part of one total: a tiny file does not lend its unused
// MaxBytes to the other fields.
func TestFormOverheadIsBounded(t *testing.T) {
	small := upload.Policy{MaxBytes: 256 << 10, Types: []string{"image/png"}, Prefix: "avatars/"}
	field := func(n int) part { return part{field: "note", body: bytes.Repeat([]byte("n"), n)} }
	file := part{field: "file", filename: "a.png", body: png}
	many := []part{file}
	for i := 0; i < 70; i++ {
		many = append(many, field(1<<10)) // 70 KiB of small fields
	}
	for name, tc := range map[string]struct {
		req  *http.Request
		want error
	}{
		"field after the file":    {form(t, file, field(upload.MaxFormBytes+1)), upload.ErrTooLarge},
		"field before the file":   {form(t, field(upload.MaxFormBytes+1), file), upload.ErrTooLarge},
		"many small fields":       {form(t, many...), upload.ErrTooLarge},
		"fields within the limit": {form(t, file, field(upload.MaxFormBytes-4<<10)), nil},
	} {
		store := memory.New()
		_, err := upload.Receive(store, tc.req, small)
		if !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
			t.Errorf("%s: Receive = %v, want %v", name, err, tc.want)
		}
		if keys := store.Keys(); tc.want != nil && len(keys) != 0 {
			t.Errorf("%s: the store holds %v after the refused request", name, keys)
		}
	}
}

// prefixOf returns a valid prefix exactly n bytes long (n >= 2).
func prefixOf(n int) string {
	var b strings.Builder
	for b.Len() < n-1 {
		seg := min(200, n-1-b.Len())
		if b.Len() > 0 {
			seg = min(200, n-2-b.Len())
			b.WriteByte('/')
		}
		b.WriteString(strings.Repeat("p", seg))
	}
	b.WriteByte('/')
	return b.String()
}

// TestPrefixLeavesRoomForTheKey: the policy checks the key it will
// generate (Prefix and a 32-character id), so a prefix the policy accepts
// gives keys every driver accepts.
func TestPrefixLeavesRoomForTheKey(t *testing.T) {
	ctx := context.Background()
	fits := prefixOf(storage.MaxKeyBytes - 32)
	tooLong := prefixOf(storage.MaxKeyBytes - 31)
	if len(fits) != storage.MaxKeyBytes-32 || len(tooLong) != storage.MaxKeyBytes-31 {
		t.Fatalf("prefixOf made %d and %d bytes", len(fits), len(tooLong))
	}
	p := images
	p.Prefix = fits
	f, err := upload.Save(ctx, memory.New(), bytes.NewReader(png), "", p)
	if err != nil || len(f.Key) != storage.MaxKeyBytes {
		t.Fatalf("Save with a %d-byte prefix = %q (%d bytes), %v; want a %d-byte key", len(fits), f.Key, len(f.Key), err, storage.MaxKeyBytes)
	}
	p.Prefix = tooLong
	store := &countingPuts{Store: memory.New()}
	if _, err := upload.Save(ctx, store, bytes.NewReader(png), "", p); err == nil || status(upload.MapError(ctx, err)) != http.StatusInternalServerError {
		t.Fatalf("Save with a %d-byte prefix = %v, want the policy refused (500: the server's mistake)", len(tooLong), err)
	}
	if store.puts != 0 {
		t.Fatalf("Save with a %d-byte prefix reached the store (%d Puts): the policy should have refused it", len(tooLong), store.puts)
	}
}

// countingPuts counts the Puts that reach a store.
type countingPuts struct {
	*memory.Store
	puts int
}

func (c *countingPuts) Put(ctx context.Context, key string, r io.Reader, opts storage.PutOptions) (storage.ObjectInfo, error) {
	c.puts++
	return c.Store.Put(ctx, key, r, opts)
}

// TestEpilogueIsBounded: bytes after the closing boundary (the MIME
// epilogue, which a multipart parser stops before) are part of the request
// and of the form's overhead: a chunked request cannot follow a valid form
// with an unbounded tail. A small epilogue is fine.
func TestEpilogueIsBounded(t *testing.T) {
	chunked := func(epilogue int) *http.Request {
		t.Helper()
		f := form(t, part{field: "file", filename: "a.png", body: png})
		b, err := io.ReadAll(f.Body)
		if err != nil {
			t.Fatal(err)
		}
		body := io.MultiReader(bytes.NewReader(b), bytes.NewReader(bytes.Repeat([]byte("e"), epilogue)))
		r := httptest.NewRequest(http.MethodPost, "/upload", struct{ io.Reader }{body}) // no length: chunked
		r.Header = f.Header
		if r.ContentLength != -1 {
			t.Fatalf("ContentLength = %d, want -1 (unknown)", r.ContentLength)
		}
		return r
	}
	for name, tc := range map[string]struct {
		epilogue int
		want     error
	}{
		"over the form limit":  {upload.MaxFormBytes + 1, upload.ErrTooLarge},
		"over the total limit": {int(images.MaxBytes) + upload.MaxFormBytes, upload.ErrTooLarge},
		"small":                {100, nil},
	} {
		store := memory.New()
		_, err := upload.Receive(store, chunked(tc.epilogue), images)
		if !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
			t.Errorf("%s: Receive = %v, want %v", name, err, tc.want)
		}
		if keys := store.Keys(); tc.want != nil && len(keys) != 0 {
			t.Errorf("%s: the store holds %v after the refused request", name, keys)
		}
	}
}

// truncatedAtTheEnd returns all of data together with err in one Read, then
// io.EOF: what a body cut short (fewer bytes than its Content-Length) can
// do. A buffered parser may keep err while it hands out the bytes.
type truncatedAtTheEnd struct {
	data []byte
	err  error
	done bool
}

func (r *truncatedAtTheEnd) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	n := copy(p, r.data)
	r.data = r.data[n:]
	if len(r.data) > 0 {
		r.done = false
		return n, nil
	}
	return n, r.err
}

// TestBodyErrorAfterTheClosingBoundaryFails: a body that fails (here with
// io.ErrUnexpectedEOF) in the same Read that delivered the closing
// boundary has failed, though the multipart entity parsed whole: Receive
// fails and deletes the file it stored.
func TestBodyErrorAfterTheClosingBoundaryFails(t *testing.T) {
	f := form(t, part{field: "file", filename: "a.png", body: png})
	b, err := io.ReadAll(f.Body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/upload", struct{ io.Reader }{&truncatedAtTheEnd{data: b, err: io.ErrUnexpectedEOF}})
	r.Header = f.Header
	store := memory.New()
	_, err = upload.Receive(store, r, images)
	if !errors.Is(err, upload.ErrMalformed) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Receive of a body that failed at its end = %v, want ErrMalformed carrying io.ErrUnexpectedEOF", err)
	}
	if keys := store.Keys(); len(keys) != 0 {
		t.Fatalf("the store holds %v after the failed request", keys)
	}
}
