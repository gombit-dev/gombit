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
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/internal/urlbase"
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
	// The rule config.Validate applies to GOMBIT_STORAGE_LOCAL_URL too.
	path, problem := urlbase.Base(cfg.Base)
	switch {
	case problem != "":
		return nil, fmt.Errorf("presign: Base %q: %s", cfg.Base, problem)
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
	return &Signer{base: cfg.Base, path: path, secret: append([]byte(nil), cfg.Secret...), public: cfg.PublicPrefix, scope: cfg.Scope, now: now}, nil
}

// Path is the URL path Handler serves under (the path of Base).
func (s *Signer) Path() string { return s.path }

// URL returns key's URL, as Storage.URL specifies: a public one for a
// public key (ErrNotPublic for a private one), or a signed one that
// expires after storage.RoundExpiry(opts.Expires), at a whole Unix second
// no earlier than that. key and opts must
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
	e := strconv.FormatInt(s.expiry(opts.Expires), 10)
	return u + "?expires=" + e + "&signature=" + s.sign(key, e), nil
}

// sign returns the signature of key expiring at expires (decimal Unix
// seconds), for reading it.
func (s *Signer) sign(key, expires string) string {
	return s.mac("gombit-storage-url-v2", key, expires)
}

// mac returns the signature of a message of kind about key: the kind
// keeps a read URL's signature from authorizing an upload and back.
func (s *Signer) mac(kind, key string, parts ...string) string {
	mac := hmac.New(sha256.New, s.secret)
	// Each part is length-prefixed, so the message splits one way only.
	for _, part := range append([]string{kind, s.scope, s.path, key}, parts...) {
		_, _ = io.WriteString(mac, strconv.Itoa(len(part))+":"+part)
	}
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// expiry returns the Unix second a URL living ttl from now expires at:
// ttl rounded up to a whole second (storage.RoundExpiry, as on every
// driver), then the end rounded up to a whole second, so it is never
// shorter than asked.
func (s *Signer) expiry(ttl time.Duration) int64 {
	end := s.now().Add(storage.RoundExpiry(ttl))
	expires := end.Unix()
	if end.After(time.Unix(expires, 0)) {
		expires++
	}
	return expires
}

// UploadURL returns a signed direct upload (storage.DirectUploader): a PUT
// to key's URL whose query signs the length, the content type, and the
// metadata, and the Content-Type header the client must send. Handler
// stores the body only when all of them match.
func (s *Signer) UploadURL(key string, opts storage.UploadURLOptions) (storage.UploadRequest, error) {
	if err := storage.ValidateKey(key); err != nil {
		return storage.UploadRequest{}, err
	}
	if err := storage.ValidateUploadURLOptions(opts); err != nil {
		return storage.UploadRequest{}, err
	}
	contentType := opts.ContentType
	if contentType == "" {
		contentType = storage.DefaultContentType
	}
	meta, err := encodeMetadata(opts.Metadata)
	if err != nil {
		return storage.UploadRequest{}, err
	}
	expires := s.expiry(opts.Expires)
	e, size := strconv.FormatInt(expires, 10), strconv.FormatInt(opts.Size, 10)
	q := url.Values{"expires": {e}, "size": {size}, "type": {contentType}}
	if meta != "" {
		q.Set("meta", meta)
	}
	q.Set("signature", s.mac("gombit-storage-upload-v1", key, e, size, contentType, meta))
	return storage.UploadRequest{
		Method:  http.MethodPut,
		URL:     s.base + "/" + storage.EscapeKey(key) + "?" + q.Encode(),
		Header:  map[string]string{"Content-Type": contentType},
		Expires: time.Unix(expires, 0),
	}, nil
}

// encodeMetadata is md as an upload URL carries it: base64url JSON
// (encoding/json sorts the names), empty for none.
func encodeMetadata(md map[string]string) (string, error) {
	if len(md) == 0 {
		return "", nil
	}
	b, err := json.Marshal(md)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// grant is a verified upload: what the request may store.
type grant struct {
	size        int64
	contentType string
	metadata    map[string]string
}

// verifyUpload checks an upload request for key against its signed query
// (ErrSignature, ErrExpired).
func (s *Signer) verifyUpload(key string, q url.Values) (grant, error) {
	for _, name := range []string{"expires", "size", "type", "signature"} {
		if len(q[name]) != 1 {
			return grant{}, ErrSignature
		}
	}
	if len(q["meta"]) > 1 {
		return grant{}, ErrSignature
	}
	e, size, contentType, meta := q.Get("expires"), q.Get("size"), q.Get("type"), q.Get("meta")
	if !hmac.Equal([]byte(q.Get("signature")), []byte(s.mac("gombit-storage-upload-v1", key, e, size, contentType, meta))) {
		return grant{}, ErrSignature
	}
	expires, err := strconv.ParseInt(e, 10, 64)
	if err != nil {
		return grant{}, ErrSignature
	}
	if !s.now().Before(time.Unix(expires, 0)) {
		return grant{}, ErrExpired
	}
	n, err := strconv.ParseInt(size, 10, 64)
	if err != nil || n < 0 {
		return grant{}, ErrSignature
	}
	g := grant{size: n, contentType: contentType}
	if meta != "" {
		b, err := base64.RawURLEncoding.DecodeString(meta)
		if err != nil || json.Unmarshal(b, &g.metadata) != nil {
			return grant{}, ErrSignature
		}
	}
	return g, nil
}

// Verify reports whether a request for key with query q may read it: a
// request carrying a signature (or an expiry) only if it is an unexpired
// signature the Signer made for key (ErrExpired, ErrSignature), whether
// key is public or not; an unsigned request only for a public key.
func (s *Signer) Verify(key string, q url.Values) error {
	// A public key needs no signature, but a URL that carries one is a
	// signed URL, and holds to what it says: it expires, and a forged one
	// fails. (The same key without the query still reads the object: it
	// is public.)
	if _, signed := q["signature"]; !signed {
		if _, signed = q["expires"]; !signed && storage.IsPublic(s.public, key) {
			return nil
		}
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
//
// PUT stores a direct upload (Signer.UploadURL): only with an unexpired
// upload signature, a Content-Type equal to the signed one, and a body of
// exactly the signed length (403 otherwise, as S3 refuses a request that
// differs from its presigned one). It answers 200 with the new ETag.
func Handler(store storage.Storage, s *Signer) http.Handler {
	locks := new(keyLocks)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodPut {
			w.Header().Set("Allow", "GET, HEAD, PUT")
			writeError(w, r, contract.MethodNotAllowed(""))
			return
		}
		key, ok := strings.CutPrefix(r.URL.Path, s.path+"/")
		if !ok || storage.ValidateKey(key) != nil {
			writeError(w, r, contract.NotFound("file not found"))
			return
		}
		q := r.URL.Query()
		if r.Method == http.MethodPut {
			serveUpload(w, r, store, s, locks, key, q)
			return
		}
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

// keyLocks serializes uploads per key (striped), so two uses of one grant
// in this process cannot both find the key empty.
type keyLocks [64]sync.Mutex

func (l *keyLocks) lock(key string) func() {
	h := fnv.New32a()
	_, _ = io.WriteString(h, key)
	m := &l[h.Sum32()%uint32(len(l))]
	m.Lock()
	return m.Unlock
}

// serveUpload stores the body of a PUT to key's upload URL, once: a key
// that already holds an object is a D10 409 conflict (S3 answers the
// grant's "If-None-Match: *" with 412), so a checked upload cannot be replaced through its
// grant. (Processes sharing a local root each serialize their own
// uploads; two using one grant at the same instant could both store.)
func serveUpload(w http.ResponseWriter, r *http.Request, store storage.Storage, s *Signer, locks *keyLocks, key string, q url.Values) {
	g, err := s.verifyUpload(key, q)
	if err != nil {
		writeError(w, r, contract.Authorization("The upload link is invalid or has expired."))
		return
	}
	if r.Header.Get("Content-Type") != g.contentType || r.ContentLength != g.size {
		writeError(w, r, contract.Authorization("The upload does not match its link: send the signed Content-Type and exactly the signed length."))
		return
	}
	body := io.Reader(http.NoBody)
	if r.Body != nil {
		body = r.Body
	}
	defer locks.lock(key)()
	switch exists, err := storage.Exists(r.Context(), store, key); {
	case err != nil:
		writeError(w, r, storage.MapError(r.Context(), err, "file not found", "could not store the file"))
		return
	case exists:
		writeError(w, r, contract.New(contract.CategoryConflict, "This upload link has been used."))
		return
	}
	info, err := store.Put(r.Context(), key, body, storage.PutOptions{
		ContentType: g.contentType,
		Size:        storage.KnownSize(g.size),
		Metadata:    g.metadata,
	})
	if err != nil {
		writeError(w, r, storage.MapError(r.Context(), err, "file not found", "could not store the file"))
		return
	}
	if info.ETag != "" {
		w.Header().Set("ETag", strconv.Quote(info.ETag))
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
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

// StoreURL is Storage.URL for a driver whose URLs come from s (nil: none):
// it validates key and opts, honors ctx, and wraps the result as the
// contract asks. The local and memory drivers' URL is this.
func StoreURL(ctx context.Context, s *Signer, key string, opts storage.URLOptions) (string, error) {
	if err := storage.ValidateKey(key); err != nil {
		return "", storage.Wrap("url", key, err)
	}
	if err := storage.ValidateURLOptions(opts); err != nil {
		return "", storage.Wrap("url", key, err)
	}
	if err := ctx.Err(); err != nil {
		return "", storage.Wrap("url", key, err)
	}
	if s == nil {
		return "", storage.Wrap("url", key, storage.ErrUnsupported)
	}
	u, err := s.URL(key, opts)
	return u, storage.Wrap("url", key, err)
}

// StoreUploadURL is storage.DirectUploader's UploadURL for a driver whose
// URLs come from s (nil: none), as StoreURL is for URL.
func StoreUploadURL(ctx context.Context, s *Signer, key string, opts storage.UploadURLOptions) (storage.UploadRequest, error) {
	if err := storage.ValidateKey(key); err != nil {
		return storage.UploadRequest{}, storage.Wrap("upload url", key, err)
	}
	if err := storage.ValidateUploadURLOptions(opts); err != nil {
		return storage.UploadRequest{}, storage.Wrap("upload url", key, err)
	}
	if err := ctx.Err(); err != nil {
		return storage.UploadRequest{}, storage.Wrap("upload url", key, err)
	}
	if s == nil {
		return storage.UploadRequest{}, storage.Wrap("upload url", key, storage.ErrUnsupported)
	}
	req, err := s.UploadURL(key, opts)
	return req, storage.Wrap("upload url", key, err)
}
