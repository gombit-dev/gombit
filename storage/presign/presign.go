// Package presign gives a store with no URL scheme of its own (the local
// and memory drivers) the URLs of the storage contract, served by the
// application: a Signer makes the URLs, and Handler serves them.
//
// A public object's URL is Base + "/" + the key. A private object's signed
// URL adds "expires" (Unix seconds) and "signature" (HMAC-SHA256 over the
// key and the expiry, with Secret); it stops working at the expiry, and a
// URL with any part changed does not work at all. Handler serves an object
// only through a URL the Signer would have made, so a private object is
// never readable without one.
//
// framework.New mounts this for the local and memory drivers, at
// GOMBIT_STORAGE_LOCAL_URL ("/_storage" by default).
package presign

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/storage"
)

// MinSecretBytes is the shortest Secret a Signer accepts.
const MinSecretBytes = 32

var (
	// ErrExpired: a signed URL whose expiry has passed.
	ErrExpired = errors.New("presign: the URL has expired")
	// ErrSignature: a URL without a valid signature for its key and
	// expiry (missing, malformed, or for other values).
	ErrSignature = errors.New("presign: the URL's signature is invalid")
)

// Config configures a Signer.
type Config struct {
	// Base is where Handler serves: an absolute URL
	// ("https://app.example.com/_storage") or a root-relative path
	// ("/_storage"), without a query, a fragment, or a trailing '/'.
	Base string
	// Secret is the HMAC-SHA256 key, at least MinSecretBytes long. Anyone
	// holding it can sign URLs for every object.
	Secret []byte
	// PublicPrefix makes the keys under it public (storage.IsPublic): their
	// URLs need no signature. Empty makes no object public.
	PublicPrefix string
	// Scope is signed into every URL, so a URL works only on Signers with
	// the same Scope and Secret: set it to what tells apps sharing a secret
	// apart (framework.New uses the app's name and environment, so a
	// staging URL does not open production's object of the same key).
	Scope string
	// Now is the clock (time.Now when nil), for tests.
	Now func() time.Time
}

// Signer makes and checks the URLs of one store.
type Signer struct {
	base   string
	path   string // the path of base, where Handler is mounted
	secret []byte
	public string
	scope  string
	now    func() time.Time
}

// New returns a Signer for cfg.
func New(cfg Config) (*Signer, error) {
	u, err := url.Parse(cfg.Base)
	switch {
	case err != nil:
		return nil, fmt.Errorf("presign: Base %q: %w", cfg.Base, err)
	case u.RawQuery != "" || u.Fragment != "" || strings.HasSuffix(cfg.Base, "/") || u.User != nil:
		return nil, fmt.Errorf("presign: Base %q: want a URL or path without a query, fragment, credentials, or trailing '/'", cfg.Base)
	case u.IsAbs() && (u.Scheme != "http" && u.Scheme != "https" || u.Host == ""):
		return nil, fmt.Errorf("presign: Base %q: an absolute Base must be http(s)://host/path", cfg.Base)
	case !u.IsAbs() && (u.Host != "" || !strings.HasPrefix(cfg.Base, "/")):
		return nil, fmt.Errorf("presign: Base %q: a relative Base must be a path starting with '/'", cfg.Base)
	case u.Path == "" || u.EscapedPath() != u.Path:
		return nil, fmt.Errorf("presign: Base %q: needs a plain path to serve under, such as /_storage", cfg.Base)
	case len(cfg.Secret) < MinSecretBytes:
		return nil, fmt.Errorf("presign: Secret is %d bytes; at least %d are needed", len(cfg.Secret), MinSecretBytes)
	}
	if err := storage.ValidatePublicPrefix(cfg.PublicPrefix); err != nil {
		return nil, fmt.Errorf("presign: %v", err)
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Signer{base: cfg.Base, path: u.Path, secret: append([]byte(nil), cfg.Secret...), public: cfg.PublicPrefix, scope: cfg.Scope, now: now}, nil
}

// Path is the URL path Handler serves under (the path of Base).
func (s *Signer) Path() string { return s.path }

// URL returns key's URL, as Storage.URL specifies: a public one for a
// public key (ErrNotPublic for a private one), or a signed one that
// expires after opts.Expires, rounded up to the second. key and opts must
// be valid (a driver checks them first).
func (s *Signer) URL(key string, opts storage.URLOptions) (string, error) {
	if err := storage.ValidateKey(key); err != nil {
		return "", err
	}
	if err := storage.ValidateURLOptions(opts); err != nil {
		return "", err
	}
	u := s.base + "/" + storage.EscapeKey(key)
	if !opts.Signed {
		if !storage.IsPublic(s.public, key) {
			return "", storage.ErrNotPublic
		}
		return u, nil
	}
	now := s.now()
	expires := now.Add(opts.Expires).Unix()
	if now.Add(opts.Expires).After(time.Unix(expires, 0)) {
		expires++ // round up: never shorter than asked
	}
	e := strconv.FormatInt(expires, 10)
	return u + "?expires=" + e + "&signature=" + s.sign(key, e), nil
}

// sign returns the signature of key expiring at expires (decimal Unix
// seconds).
func (s *Signer) sign(key, expires string) string {
	mac := hmac.New(sha256.New, s.secret)
	// Each part is length-prefixed, so the message splits one way only.
	for _, part := range []string{"gombit-storage-url-v2", s.scope, s.path, key, expires} {
		_, _ = io.WriteString(mac, strconv.Itoa(len(part))+":"+part)
	}
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Verify reports whether a request for key with query q may read it: yes
// for a public key; for a private one, only with an unexpired signature
// the Signer made (ErrExpired, ErrSignature).
func (s *Signer) Verify(key string, q url.Values) error {
	if storage.IsPublic(s.public, key) {
		return nil
	}
	e, sig := q.Get("expires"), q.Get("signature")
	if len(q["expires"]) != 1 || len(q["signature"]) != 1 {
		return ErrSignature
	}
	expires, err := strconv.ParseInt(e, 10, 64)
	if err != nil || expires <= 0 {
		return ErrSignature
	}
	if !hmac.Equal([]byte(sig), []byte(s.sign(key, e))) {
		return ErrSignature
	}
	if !s.now().Before(time.Unix(expires, 0)) {
		return ErrExpired
	}
	return nil
}

// Handler serves the objects of store at the URLs s makes: GET and HEAD
// on s.Path() + "/" + key. A private object needs a valid signed URL
// (403 otherwise), a missing one is 404, and errors are D10 envelopes.
//
// Objects are served as the store has them, inline (with byte ranges and
// conditional requests when the store's reader can seek, as the local and
// memory drivers' can), with
// "X-Content-Type-Options: nosniff" and a sandboxing
// Content-Security-Policy, so an uploaded HTML or SVG file served from the
// application's origin cannot run script there. A signed URL's response
// may be cached privately until the URL expires; a public one's, not at
// all (the object can be replaced under the same URL).
func Handler(store storage.Storage, s *Signer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, r, contract.MethodNotAllowed(""))
			return
		}
		key, ok := strings.CutPrefix(r.URL.Path, s.path+"/")
		if !ok || storage.ValidateKey(key) != nil {
			writeError(w, r, contract.NotFound("file not found"))
			return
		}
		q := r.URL.Query()
		if err := s.Verify(key, q); err != nil {
			writeError(w, r, contract.Authorization("The link is invalid or has expired."))
			return
		}
		body, info, err := store.Open(r.Context(), key)
		if err != nil {
			writeError(w, r, storage.MapError(r.Context(), err, "file not found", "could not read the file"))
			return
		}
		defer func() { _ = body.Close() }()
		h := w.Header()
		h.Set("Content-Type", info.ContentType)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
		if info.ETag != "" {
			h.Set("ETag", strconv.Quote(info.ETag))
		}
		if name := info.Metadata[storage.FilenameMetadata]; name != "" {
			if d := mime.FormatMediaType("inline", map[string]string{"filename": name}); d != "" {
				h.Set("Content-Disposition", d)
			}
		}
		if storage.IsPublic(s.public, key) {
			h.Set("Cache-Control", "no-cache")
		} else {
			expires, _ := strconv.ParseInt(q.Get("expires"), 10, 64)
			left := math.Max(0, time.Unix(expires, 0).Sub(s.now()).Seconds())
			h.Set("Cache-Control", "private, max-age="+strconv.Itoa(int(left)))
		}
		if rs, ok := body.(io.ReadSeeker); ok {
			// Ranges (media seeking, resumed downloads), conditional
			// requests on the ETag and ModTime, and HEAD, as S3 serves them.
			http.ServeContent(w, r, "", info.ModTime, rs)
			return
		}
		h.Set("Content-Length", strconv.FormatInt(info.Size, 10))
		if !info.ModTime.IsZero() {
			h.Set("Last-Modified", info.ModTime.UTC().Format(http.TimeFormat))
		}
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = io.Copy(w, body)
		}
	})
}

// writeError writes err, a D10 envelope, as the response.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	var env *contract.ErrorEnvelope
	if !errors.As(err, &env) {
		env = contract.Internal("unexpected error")
	}
	env = contract.WithContext(r.Context(), env)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(env.GetStatus())
	if r.Method != http.MethodHead {
		_ = json.NewEncoder(w).Encode(env)
	}
}
