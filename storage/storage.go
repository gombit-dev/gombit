package storage

import (
	"context"
	"errors"
	"io"
	"time"
)

// Storage is the driver-neutral object storage contract. Application code
// depends on it, never on a driver's API (a filesystem path, an S3 SDK
// call), so switching drivers changes configuration, not code.
//
// Every method validates its key with ValidateKey and fails with
// ErrInvalidKey before touching the backend. Every method honors ctx: one
// that has already ended fails the call with ctx's error.
//
// Every valid key is its own object on every driver: keys that differ only
// in case, a key that is a prefix of another ("a" and "a/b"), segments up
// to 255 bytes, and names a filesystem reserves ("CON", "c:") all store and
// read back as themselves. A driver on a filesystem must map keys to paths
// in a way that keeps this (it cannot just join the key to a directory).
//
// Implementations must pass the storagetest conformance suite.
type Storage interface {
	// Put stores the bytes read from r under key, replacing any object
	// already there. It streams: r is read to EOF in pieces, never required
	// to be seekable or of known length.
	//
	// Put is atomic. When it fails (r returns an error, ctx ends, the
	// length differs from a declared opts.Size), key keeps the object it
	// had before, or stays absent; no reader ever sees a partial object.
	// Concurrent Puts to one key leave one of them whole, and a reader
	// during a Put reads the previous object whole.
	Put(ctx context.Context, key string, r io.Reader, opts PutOptions) (ObjectInfo, error)

	// Open returns the object's bytes as a stream, and its ObjectInfo. The
	// caller must Close the reader. A missing object is ErrNotFound.
	Open(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error)

	// Stat returns the object's ObjectInfo without its bytes. A missing
	// object is ErrNotFound.
	Stat(ctx context.Context, key string) (ObjectInfo, error)

	// Delete removes the object. Deleting a missing object is not an error,
	// so a retried Delete is safe.
	Delete(ctx context.Context, key string) error

	// URL returns a URL a client can fetch the object from: a permanent
	// public URL for a public object (ErrNotPublic for a private one; see
	// IsPublic), or a signed one that works for any object until it
	// expires (see URLOptions; at most MaxURLExpiry). A driver that cannot
	// produce one returns ErrUnsupported. URL does not check that the
	// object exists, and it does not authorize anyone: decide who may have
	// the URL before asking for it.
	URL(ctx context.Context, key string, opts URLOptions) (string, error)
}

// DefaultContentType is the content type of an object stored without one.
const DefaultContentType = "application/octet-stream"

// PutOptions describes an object being stored.
type PutOptions struct {
	// ContentType is the object's media type ("image/png"), parameters
	// allowed ("text/plain; charset=utf-8"). Empty stores
	// DefaultContentType. It must be a type/subtype media type.
	ContentType string

	// Size, when set, is the exact length r will produce (zero for an empty
	// object). Put then fails with ErrSizeMismatch, storing nothing, if r
	// ends early or runs long. Nil means unknown: Put reads r to EOF. Build
	// it with KnownSize, or from an HTTP request's Content-Length with
	// SizeFromContentLength (-1, unknown, is nil).
	Size *int64

	// Metadata is small user metadata stored with the object and returned
	// in its ObjectInfo exactly as given. Names are lower-case ASCII
	// letters, digits, and '-'; values are UTF-8 without control
	// characters; see ValidateMetadata for the size limit. A driver whose
	// backend carries only ASCII (S3 headers) encodes values reversibly.
	Metadata map[string]string
}

// ObjectInfo describes a stored object.
type ObjectInfo struct {
	// Key is the object's key.
	Key string
	// Size is the object's length in bytes.
	Size int64
	// ContentType is the object's media type (DefaultContentType when it
	// was stored without one).
	ContentType string
	// ETag identifies this version of the object's bytes, when the driver
	// has one; it changes when the object is replaced with different
	// bytes. Empty when unsupported. Compare ETags, don't compute them: the
	// local and memory drivers' is the hex SHA-256 of the bytes, S3's the
	// hex MD5 for an object stored in one request and "<md5>-<parts>" for a
	// multipart upload (a checksum of the parts, not of the bytes).
	ETag string
	// ModTime is when the object was last stored (for an object written
	// once, as uploads under generated keys are, when it was created).
	// Stat, Open, and List always report it; the ObjectInfo Put returns
	// may leave it zero when the backend does not say (S3's PutObject),
	// rather than cost a second request.
	ModTime time.Time
	// Metadata is the user metadata the object was stored with (nil when
	// none).
	Metadata map[string]string
}

// URLOptions chooses the kind of URL URL returns. Build it with PublicURL
// or SignedURL.
type URLOptions struct {
	// Signed asks for a URL that grants access only until Expires has
	// passed. Unsigned asks for a permanent public URL.
	Signed bool
	// Expires is how long a signed URL is valid: it must be positive for a
	// signed URL, and zero for a public one (ErrInvalidOptions otherwise),
	// so a zero or negative lifetime can never widen into a permanent URL.
	Expires time.Duration
}

// PublicURL asks for a permanent URL to an object readable without
// credentials.
func PublicURL() URLOptions { return URLOptions{} }

// SignedURL asks for a URL that grants access to the object until ttl has
// passed. ttl must be positive: URL fails with ErrInvalidOptions otherwise,
// rather than returning anything longer-lived.
func SignedURL(ttl time.Duration) URLOptions { return URLOptions{Signed: true, Expires: ttl} }

// KnownSize is a PutOptions.Size of exactly n bytes.
func KnownSize(n int64) *int64 { return &n }

// SizeFromContentLength is the PutOptions.Size for an HTTP request body of
// Content-Length n: nil when unknown (-1), the exact length otherwise
// (zero included, so a request claiming an empty body cannot store one).
func SizeFromContentLength(n int64) *int64 {
	if n < 0 {
		return nil
	}
	return KnownSize(n)
}

// Exists reports whether key holds an object: Stat, with ErrNotFound as
// false.
func Exists(ctx context.Context, s Storage, key string) (bool, error) {
	_, err := s.Stat(ctx, key)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}
