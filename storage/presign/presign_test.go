package presign_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/storage"
	"github.com/gombit-dev/gombit/storage/memory"
	"github.com/gombit-dev/gombit/storage/presign"
	"github.com/gombit-dev/gombit/storage/storagetest"
)

var secret = []byte(strings.Repeat("s", presign.MinSecretBytes))

// clock is a settable clock.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newSigner(t *testing.T, c *clock) *presign.Signer {
	t.Helper()
	s, err := presign.New(presign.Config{Base: "/_storage", Secret: secret, PublicPrefix: "public/", Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// setup returns a memory store with URLs, a server for them, and the clock.
func setup(t *testing.T) (*memory.Store, *httptest.Server, *clock) {
	t.Helper()
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	s := newSigner(t, c)
	store := memory.New(memory.WithURLs(s))
	mux := http.NewServeMux()
	mux.Handle(s.Path()+"/", presign.Handler(store, s))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return store, srv, c
}

func get(t *testing.T, srv *httptest.Server, u string) (*http.Response, string) {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL + u)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func put(t *testing.T, s storage.Storage, key, body string, md map[string]string) {
	t.Helper()
	if _, err := s.Put(context.Background(), key, strings.NewReader(body), storage.PutOptions{ContentType: "text/html", Metadata: md}); err != nil {
		t.Fatal(err)
	}
}

func TestSignedURLServesThePrivateObjectUntilItExpires(t *testing.T) {
	store, srv, c := setup(t)
	key := "private/ré sumé+1;.html"
	put(t, store, key, "<script>alert(1)</script>", map[string]string{"filename": "résumé.html"})
	u, err := store.URL(context.Background(), key, storage.SignedURL(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(u, "/_storage/private/") || strings.ContainsAny(strings.SplitN(u, "?", 2)[0], " +;") {
		t.Fatalf("URL = %q, want the escaped key under /_storage", u)
	}
	resp, body := get(t, srv, u)
	if resp.StatusCode != http.StatusOK || body != "<script>alert(1)</script>" {
		t.Fatalf("GET = %d %q", resp.StatusCode, body)
	}
	h := resp.Header
	if h.Get("Content-Type") != "text/html" || h.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(h.Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("headers %v: an uploaded HTML file must be sandboxed, not run in the app's origin", h)
	}
	if h.Get("Cache-Control") != "private, max-age=600" || !strings.Contains(h.Get("Content-Disposition"), "filename") {
		t.Fatalf("headers %v", h)
	}

	c.t = c.t.Add(10*time.Minute - time.Second)
	if resp, _ := get(t, srv, u); resp.StatusCode != http.StatusOK {
		t.Fatalf("a second before it expires = %d", resp.StatusCode)
	}
	c.t = c.t.Add(time.Second)
	resp, body = get(t, srv, u)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, `"authorization"`) {
		t.Fatalf("at the expiry = %d %s, want a D10 403", resp.StatusCode, body)
	}
}

func TestSignedURLsCannotBeAltered(t *testing.T) {
	store, srv, _ := setup(t)
	put(t, store, "private/a.txt", "a", nil)
	put(t, store, "private/b.txt", "b", nil)
	u, err := store.URL(context.Background(), "private/a.txt", storage.SignedURL(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(u)
	q := parsed.Query()
	later := func() string {
		q := parsed.Query()
		q.Set("expires", "9999999999")
		return parsed.Path + "?" + q.Encode()
	}
	for name, alt := range map[string]string{
		"no query":           parsed.Path,
		"another key":        "/_storage/private/b.txt?" + parsed.RawQuery,
		"a later expiry":     later(),
		"no signature":       parsed.Path + "?expires=" + q.Get("expires"),
		"a wrong signature":  parsed.Path + "?expires=" + q.Get("expires") + "&signature=" + strings.Repeat("A", 43),
		"a padded expiry":    parsed.Path + "?expires=0" + q.Get("expires") + "&signature=" + q.Get("signature"),
		"a repeated expiry":  parsed.Path + "?expires=" + q.Get("expires") + "&expires=1&signature=" + q.Get("signature"),
		"the key's case":     "/_storage/private/A.txt?" + parsed.RawQuery,
		"a trailing segment": "/_storage/private/a.txt/x?" + parsed.RawQuery,
	} {
		if resp, _ := get(t, srv, alt); resp.StatusCode == http.StatusOK {
			t.Errorf("%s (%s) served the object", name, alt)
		}
	}
	if resp, body := get(t, srv, u); resp.StatusCode != http.StatusOK || body != "a" {
		t.Fatalf("the unaltered URL = %d %q", resp.StatusCode, body)
	}
}

func TestPublicObjects(t *testing.T) {
	store, srv, _ := setup(t)
	put(t, store, "public/logo.png", "png", nil)
	u, err := store.URL(context.Background(), "public/logo.png", storage.PublicURL())
	if err != nil || u != "/_storage/public/logo.png" {
		t.Fatalf("URL = %q, %v", u, err)
	}
	resp, body := get(t, srv, u)
	if resp.StatusCode != http.StatusOK || body != "png" || resp.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("GET public = %d %q %v", resp.StatusCode, body, resp.Header)
	}
	if _, err := store.URL(context.Background(), "private/x", storage.PublicURL()); !errors.Is(err, storage.ErrNotPublic) {
		t.Fatalf("a public URL for a private key = %v, want ErrNotPublic", err)
	}
	// "public" without the slash is not under the prefix.
	if _, err := store.URL(context.Background(), "publicity.txt", storage.PublicURL()); !errors.Is(err, storage.ErrNotPublic) {
		t.Fatalf("publicity.txt = %v, want ErrNotPublic", err)
	}
	if u, err := store.URL(context.Background(), "public/logo.png", storage.SignedURL(time.Minute)); err != nil || !strings.Contains(u, "signature=") {
		t.Fatalf("a signed URL for a public key = %q, %v", u, err)
	}
}

func TestHandlerErrors(t *testing.T) {
	store, srv, _ := setup(t)
	u, _ := store.URL(context.Background(), "private/missing.txt", storage.SignedURL(time.Minute))
	resp, body := get(t, srv, u)
	var env struct {
		Error struct{ Code string } `json:"error"`
	}
	if resp.StatusCode != http.StatusNotFound || json.Unmarshal([]byte(body), &env) != nil || env.Error.Code != "not_found" {
		t.Fatalf("a signed URL for a missing object = %d %s", resp.StatusCode, body)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+u, nil)
	r, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Body.Close()
	if r.StatusCode != http.StatusMethodNotAllowed || r.Header.Get("Allow") != "GET, HEAD, PUT" {
		t.Fatalf("POST = %d (Allow %q)", r.StatusCode, r.Header.Get("Allow"))
	}
	put(t, store, "public/head.txt", "12345", nil)
	req, _ = http.NewRequest(http.MethodHead, srv.URL+"/_storage/public/head.txt", nil)
	r, err = srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if r.StatusCode != http.StatusOK || len(b) != 0 || r.Header.Get("Content-Length") != "5" {
		t.Fatalf("HEAD = %d, %d body bytes, length %q", r.StatusCode, len(b), r.Header.Get("Content-Length"))
	}
}

func TestExpiryRoundsUp(t *testing.T) {
	c := &clock{t: time.Unix(100, 900_000_000)}
	s := newSigner(t, c)
	u, err := s.URL("k", storage.SignedURL(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(u)
	if got := parsed.Query().Get("expires"); got != "101" {
		t.Fatalf("expires = %s, want 101 (never shorter than asked)", got)
	}
	if err := s.Verify("k", parsed.Query()); err != nil {
		t.Fatalf("Verify = %v", err)
	}
	c.t = time.Unix(101, 0)
	if err := s.Verify("k", parsed.Query()); !errors.Is(err, presign.ErrExpired) {
		t.Fatalf("Verify at the expiry = %v, want ErrExpired", err)
	}
}

func TestSignersWithDifferentScopesDisagree(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	mk := func(scope, base string) *presign.Signer {
		s, err := presign.New(presign.Config{Base: base, Secret: secret, Scope: scope, Now: c.now})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	u, _ := mk("app\x00staging", "/_storage").URL("private/k", storage.SignedURL(time.Hour))
	parsed, _ := url.Parse(u)
	for name, other := range map[string]*presign.Signer{
		"another scope": mk("app\x00production", "/_storage"),
		"another path":  mk("app\x00staging", "/files"),
	} {
		if err := other.Verify("private/k", parsed.Query()); !errors.Is(err, presign.ErrSignature) {
			t.Errorf("%s accepted the URL: %v", name, err)
		}
	}
	if err := mk("app\x00staging", "/_storage").Verify("private/k", parsed.Query()); err != nil {
		t.Fatalf("the same scope = %v", err)
	}
}

func TestSignersWithDifferentSecretsDisagree(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	a := newSigner(t, c)
	b, err := presign.New(presign.Config{Base: "/_storage", Secret: []byte(strings.Repeat("t", 32)), Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := a.URL("private/k", storage.SignedURL(time.Hour))
	parsed, _ := url.Parse(u)
	if err := b.Verify("private/k", parsed.Query()); !errors.Is(err, presign.ErrSignature) {
		t.Fatalf("another secret's URL = %v, want ErrSignature", err)
	}
}

func TestNewValidates(t *testing.T) {
	for _, cfg := range []presign.Config{
		{Base: "/_storage", Secret: []byte("short")},
		{Base: "", Secret: secret},
		{Base: "/", Secret: secret},
		{Base: "/_storage/", Secret: secret},
		{Base: "_storage", Secret: secret},
		{Base: "/_storage?x=1", Secret: secret},
		{Base: "ftp://host/_storage", Secret: secret},
		{Base: "https://host", Secret: secret},
		{Base: "https://user:pw@host/_storage", Secret: secret}, // #nosec G101 -- a refused URL, not a credential.
		{Base: "//host/_storage", Secret: secret},
		{Base: "/_storage", Secret: secret, PublicPrefix: "public"},
		{Base: "/_storage", Secret: secret, PublicPrefix: "../public/"},
	} {
		if _, err := presign.New(cfg); err == nil {
			t.Errorf("New(%+v) succeeded", cfg)
		}
	}
	s, err := presign.New(presign.Config{Base: "https://app.example.com/files", Secret: secret, PublicPrefix: "public/"})
	if err != nil || s.Path() != "/files" {
		t.Fatalf("an absolute Base = %v (path %q)", err, s.Path())
	}
	if u, _ := s.URL("public/a b", storage.PublicURL()); u != "https://app.example.com/files/public/a%20b" {
		t.Fatalf("URL = %q", u)
	}
	if _, err := s.URL("k", storage.SignedURL(storage.MaxURLExpiry+time.Second)); !errors.Is(err, storage.ErrInvalidOptions) {
		t.Fatalf("a lifetime over MaxURLExpiry = %v", err)
	}
}

// TestConformance: a store with URLs still passes the whole suite.
func TestConformance(t *testing.T) {
	storagetest.Run(t, func(t *testing.T) storage.Storage {
		return memory.New(memory.WithURLs(newSigner(t, &clock{t: time.Now()})))
	})
}

// send makes req (a direct upload) with body, the grant's headers, and
// edit applied to the request last.
func send(t *testing.T, srv *httptest.Server, req storage.UploadRequest, body string, edit func(*http.Request)) int {
	t.Helper()
	r, err := http.NewRequest(req.Method, srv.URL+req.URL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range req.Header {
		r.Header.Set(k, v)
	}
	if edit != nil {
		edit(r)
	}
	resp, err := srv.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestDirectUploads(t *testing.T) {
	store, srv, c := setup(t)
	ctx := context.Background()
	opts := storage.UploadURLOptions{Expires: 10 * time.Minute, Size: 5, ContentType: "text/plain", Metadata: map[string]string{"filename": "résumé.txt"}}
	req, err := store.UploadURL(ctx, "uploads/a", opts)
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != http.MethodPut || req.Header["Content-Type"] != "text/plain" || !req.Expires.Equal(c.t.Add(10*time.Minute)) {
		t.Fatalf("UploadURL = %+v", req)
	}
	parsed, _ := url.Parse(req.URL)
	q := parsed.Query()
	with := func(name, value string) storage.UploadRequest {
		q := parsed.Query()
		q.Set(name, value)
		alt := req
		alt.URL = parsed.Path + "?" + q.Encode()
		return alt
	}
	other := req
	other.URL = "/_storage/uploads/b?" + parsed.RawQuery
	read, _ := store.URL(ctx, "uploads/a", storage.SignedURL(time.Minute))
	readAsUpload := req
	readAsUpload.URL = read
	for name, tc := range map[string]struct {
		req  storage.UploadRequest
		body string
		edit func(*http.Request)
	}{
		"a longer body":        {req, "hello world", nil},
		"a shorter body":       {req, "hi", nil},
		"another type":         {req, "hello", func(r *http.Request) { r.Header.Set("Content-Type", "text/html") }},
		"no type":              {req, "hello", func(r *http.Request) { r.Header.Del("Content-Type") }},
		"chunked":              {req, "hello", func(r *http.Request) { r.ContentLength = -1 }},
		"another key":          {other, "hello", nil},
		"a larger signed size": {with("size", "50"), "hello", nil},
		"another signed type":  {with("type", "text/html"), "hello", func(r *http.Request) { r.Header.Set("Content-Type", "text/html") }},
		"other metadata":       {with("meta", "e30"), "hello", nil},
		"a later expiry":       {with("expires", "9999999999"), "hello", nil},
		"no signature":         {with("signature", ""), "hello", nil},
		"a read URL":           {readAsUpload, "hello", nil},
		"a repeated size":      {storage.UploadRequest{Method: req.Method, URL: req.URL + "&size=5", Header: req.Header}, "hello", nil},
		"a repeated meta":      {storage.UploadRequest{Method: req.Method, URL: req.URL + "&meta=e30", Header: req.Header}, "hello", nil},
	} {
		if code := send(t, srv, tc.req, tc.body, tc.edit); code != http.StatusForbidden {
			t.Errorf("%s: PUT = %d, want 403", name, code)
		}
	}
	if keys := store.Keys(); len(keys) != 0 {
		t.Fatalf("refused uploads stored %v", keys)
	}
	if q.Get("signature") == "" {
		t.Fatal("no signature")
	}

	if code := send(t, srv, req, "hello", nil); code != http.StatusOK {
		t.Fatalf("the granted PUT = %d", code)
	}
	body, info, err := store.Open(ctx, "uploads/a")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(body)
	_ = body.Close()
	if string(b) != "hello" || info.ContentType != "text/plain" || info.Metadata["filename"] != "résumé.txt" {
		t.Fatalf("stored %q %+v", b, info)
	}
	// The grant is spent: it cannot replace what it stored.
	if code := send(t, srv, req, "HELLO", nil); code != http.StatusConflict {
		t.Fatalf("replaying the grant = %d, want 409", code)
	}
	if body, _, _ := store.Open(ctx, "uploads/a"); body != nil {
		b, _ := io.ReadAll(body)
		_ = body.Close()
		if string(b) != "hello" {
			t.Fatalf("the replay replaced the object with %q", b)
		}
	}
	// The upload URL does not read the object.
	if resp, _ := get(t, srv, req.URL); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("GET with the upload URL = %d, want 403", resp.StatusCode)
	}
	c.t = c.t.Add(10 * time.Minute)
	if code := send(t, srv, req, "hello", nil); code != http.StatusForbidden {
		t.Fatalf("PUT at the expiry = %d, want 403", code)
	}
}

func TestDirectUploadOfAnEmptyObject(t *testing.T) {
	store, srv, _ := setup(t)
	req, err := store.UploadURL(context.Background(), "uploads/empty", storage.UploadURLOptions{Expires: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if req.Header["Content-Type"] != storage.DefaultContentType {
		t.Fatalf("header = %v", req.Header)
	}
	if code := send(t, srv, req, "", nil); code != http.StatusOK {
		t.Fatalf("PUT empty = %d", code)
	}
	if info, err := store.Stat(context.Background(), "uploads/empty"); err != nil || info.Size != 0 {
		t.Fatalf("Stat = %+v, %v", info, err)
	}
}
