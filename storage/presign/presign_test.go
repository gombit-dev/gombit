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

	"github.com/gombit-dev/gombit/config"
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
	if r.StatusCode != http.StatusMethodNotAllowed || r.Header.Get("Allow") != "GET, HEAD" {
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

func TestExpiryCountsFromTheSigningSecond(t *testing.T) {
	// As S3 counts X-Amz-Expires from X-Amz-Date (whole seconds): a URL
	// signed at 100.9 for a second expires at 101, for two at 102.
	c := &clock{t: time.Unix(100, 900_000_000)}
	s := newSigner(t, c)
	for ttl, want := range map[time.Duration]string{time.Second: "101", 2 * time.Second: "102"} {
		u, err := s.URL("k", storage.SignedURL(ttl))
		if err != nil {
			t.Fatal(err)
		}
		parsed, _ := url.Parse(u)
		if got := parsed.Query().Get("expires"); got != want {
			t.Fatalf("SignedURL(%s) at 100.9: expires = %s, want %s", ttl, got, want)
		}
	}
	u, _ := s.URL("k", storage.SignedURL(time.Second))
	parsed, _ := url.Parse(u)
	c.t = time.Unix(100, 999_000_000)
	if err := s.Verify("k", parsed.Query()); err != nil {
		t.Fatalf("Verify at 100.999 = %v, want valid", err)
	}
	c.t = time.Unix(101, 0)
	if err := s.Verify("k", parsed.Query()); !errors.Is(err, presign.ErrExpired) {
		t.Fatalf("Verify at 101, the expiry = %v, want ErrExpired", err)
	}
	if _, err := s.URL("k", storage.SignedURL(time.Millisecond)); !errors.Is(err, storage.ErrInvalidOptions) {
		t.Fatalf("SignedURL(1ms) = %v, want ErrInvalidOptions: lifetimes are whole seconds", err)
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

// TestSignedURLForAPublicKeyExpires: a signed URL holds to what it says
// even when its key is public: past its expiry it is refused, and a forged
// signature is refused. The same key without the query still serves the
// object, because the key itself is public.
func TestSignedURLForAPublicKeyExpires(t *testing.T) {
	store, srv, c := setup(t)
	put(t, store, "public/logo.png", "png", nil)
	signed, err := store.URL(context.Background(), "public/logo.png", storage.SignedURL(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if resp, body := get(t, srv, signed); resp.StatusCode != http.StatusOK || body != "png" {
		t.Fatalf("the signed URL before it expires = %d %q", resp.StatusCode, body)
	}
	u, _ := url.Parse(signed)
	q := u.Query()
	q.Set("signature", strings.Repeat("A", len(q.Get("signature"))))
	forged := u.Path + "?" + q.Encode()
	if resp, _ := get(t, srv, forged); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a forged signature on a public key = %d, want 403", resp.StatusCode)
	}
	onlyExpires := u.Path + "?expires=" + u.Query().Get("expires")
	if resp, _ := get(t, srv, onlyExpires); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("an expiry without a signature on a public key = %d, want 403", resp.StatusCode)
	}
	c.t = c.t.Add(2 * time.Minute)
	if resp, _ := get(t, srv, signed); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("the signed URL of a public key after it expired = %d, want 403", resp.StatusCode)
	}
	if resp, body := get(t, srv, u.Path); resp.StatusCode != http.StatusOK || body != "png" {
		t.Fatalf("the public key without the query = %d %q, want 200: the key is public", resp.StatusCode, body)
	}
}

// TestBaseRuleIsShared: config validation and presign.New accept exactly
// the same Base (GOMBIT_STORAGE_LOCAL_URL), in both directions: one rule.
func TestBaseRuleIsShared(t *testing.T) {
	for base, valid := range map[string]bool{
		"/_storage":                           true,
		"/files/v1":                           true,
		"https://files.example.com/_storage":  true,
		"http://127.0.0.1:8080/s":             true,
		"/_stor%61ge":                         false, // percent-escaped: not a plain path
		"/a%2Fb":                              false,
		"/_storage/":                          false,
		"/":                                   false,
		"_storage":                            false,
		"//host/path":                         false,
		"https://files.example.com":           false, // no path to serve under
		"https://files.example.com/":          false,
		"ftp://files.example.com/s":           false,
		"https://user:pw@files.example.com/s": false,
		"/_storage?x=1":                       false,
		"/_storage?":                          false,
		"/_storage#top":                       false,
	} {
		_, newErr := presign.New(presign.Config{Base: base, Secret: secret})
		cfg := config.Default()
		cfg.Storage.Driver = config.StorageDriverMemory
		cfg.Storage.Local.URL = base
		cfgErr := config.ValidateStorage(cfg.Storage)
		if (newErr == nil) != valid || (cfgErr == nil) != valid {
			t.Errorf("Base %q: presign.New = %v, config = %v; want valid = %v", base, newErr, cfgErr, valid)
		}
	}
}
