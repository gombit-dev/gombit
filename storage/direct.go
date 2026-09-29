package storage

import (
	"context"
	"fmt"
	"time"
)

// DirectUploader is a Storage a client can upload to directly, without
// the object's bytes passing through the application: the application
// asks for an upload URL (after deciding the client may upload), hands it
// to the client, and the client sends the bytes to the backend. The local,
// memory (with a presign.Signer), and S3 drivers implement it; UploadURL
// answers ErrUnsupported for a store that does not.
type DirectUploader interface {
	// UploadURL returns the request that stores exactly one object: under
	// key, of exactly opts.Size bytes, with opts.ContentType and
	// opts.Metadata, until opts.Expires has passed. The backend refuses a
	// request that differs in any of these (a different length, type,
	// metadata, or key), so the grant is scoped to that one object. It
	// stores only where nothing is stored yet (S3 answers 412, the app's
	// storage route a D10 409 conflict, otherwise): once used,
	// the object cannot be replaced through the grant, so bytes checked
	// after the upload stay the bytes stored. It authorizes nobody: decide
	// who may upload first, and check what arrived (upload.Confirm) before
	// using it.
	UploadURL(ctx context.Context, key string, opts UploadURLOptions) (UploadRequest, error)
}

// UploadURLOptions describes the object a direct upload will store.
type UploadURLOptions struct {
	// Expires is how long the upload URL works: positive, and at most
	// MaxURLExpiry. Keep it short (minutes): the URL is a bearer grant.
	Expires time.Duration
	// Size is the exact length the client will upload, in bytes (zero for
	// an empty object). A request of any other length is refused.
	Size int64
	// ContentType is the object's media type, which the client must send
	// as its Content-Type (DefaultContentType when empty). It is what the
	// client declares, not what the bytes are: check them afterwards.
	ContentType string
	// Metadata is stored with the object (the rules of
	// PutOptions.Metadata).
	Metadata map[string]string
}

// UploadRequest is a direct upload for a client to make: send Method to
// URL with exactly the headers in Header and the object's bytes as the
// body, before Expires. The client's HTTP library sets Content-Length from
// the body (a browser's fetch does; it cannot be set by hand).
type UploadRequest struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Header  map[string]string `json:"headers"`
	Expires time.Time         `json:"expires"`
}

// ValidateUploadURLOptions reports whether opts is valid, as
// ErrInvalidOptions when it is not: a lifetime as for a signed URL, a
// non-negative size, and a content type and metadata as for Put.
func ValidateUploadURLOptions(opts UploadURLOptions) error {
	if err := ValidateURLOptions(SignedURL(opts.Expires)); err != nil {
		return err
	}
	if opts.Size < 0 {
		return fmt.Errorf("%w: negative size %d", ErrInvalidOptions, opts.Size)
	}
	return ValidatePutOptions(PutOptions{ContentType: opts.ContentType, Metadata: opts.Metadata})
}

// UploadURL asks s for a direct upload URL (DirectUploader), or fails with
// ErrUnsupported when s cannot make one.
func UploadURL(ctx context.Context, s Storage, key string, opts UploadURLOptions) (UploadRequest, error) {
	d, ok := s.(DirectUploader)
	if !ok {
		if err := ValidateKey(key); err != nil {
			return UploadRequest{}, Wrap("upload url", key, err)
		}
		if err := ValidateUploadURLOptions(opts); err != nil {
			return UploadRequest{}, Wrap("upload url", key, err)
		}
		return UploadRequest{}, Wrap("upload url", key, fmt.Errorf("%w: %T has no direct uploads", ErrUnsupported, s))
	}
	return d.UploadURL(ctx, key, opts)
}
