package upload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"net/http"
	"strings"

	"github.com/gombit-dev/gombit/storage"
)

// Grant is a direct upload granted by Authorize: the client sends Request
// (the file's bytes as its body), and the application keeps Key to
// Confirm afterwards.
type Grant struct {
	// Key is where the file will be: Policy.Prefix and a random id.
	Key string `json:"key"`
	// Request is the upload for the client to make.
	Request storage.UploadRequest `json:"upload"`
}

// Authorize grants a direct upload of one file the client declares: size
// bytes (at most p.MaxBytes, else ErrTooLarge) of contentType (one p.Types
// accepts, else ErrType), with the client's filename kept as metadata. The
// key is generated, and the store refuses any other length or type; the
// declared type is only a claim, which Confirm checks against the bytes.
// Decide that the caller may upload before calling it, and remember the
// Key (with who asked) to Confirm the same one: a grant is not a record
// that the upload happened.
//
// It fails with storage.ErrUnsupported when the store has no direct
// uploads (storage.DirectUploader); Receive is the fallback.
func Authorize(ctx context.Context, store storage.Storage, p Policy, size int64, contentType, filename string) (Grant, error) {
	if err := p.validate(); err != nil {
		return Grant{}, err
	}
	switch {
	case size < 0:
		return Grant{}, fmt.Errorf("%w: a negative size", ErrMalformed)
	case size > p.MaxBytes:
		return Grant{}, fmt.Errorf("%w: %d bytes, more than %d", ErrTooLarge, size, p.MaxBytes)
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || storage.ValidatePutOptions(storage.PutOptions{ContentType: contentType}) != nil {
		return Grant{}, fmt.Errorf("%w: declared %q", ErrType, contentType)
	}
	if !p.accepts(mediaType) {
		return Grant{}, fmt.Errorf("%w: declared %q", ErrType, contentType)
	}
	key, err := newKey(p.Prefix)
	if err != nil {
		return Grant{}, err
	}
	md := maps.Clone(p.Metadata)
	if filename = CleanFilename(filename); filename != "" {
		if md == nil {
			md = map[string]string{}
		}
		fitMetadata(md, filename)
	}
	ttl := p.GrantExpiry
	if ttl == 0 {
		ttl = DefaultGrantExpiry
	}
	req, err := storage.UploadURL(ctx, store, key, storage.UploadURLOptions{
		Expires:     ttl,
		Size:        size,
		ContentType: contentType,
		Metadata:    md,
	})
	if err != nil {
		return Grant{}, err
	}
	return Grant{Key: key, Request: req}, nil
}

// Confirm checks the direct upload at key (a Grant's Key) against p once
// the client says it is done: it must be stored (ErrNoFile otherwise), at
// most p.MaxBytes long (ErrTooLarge), and of a type p accepts as detected
// from its bytes, the same media type it was declared as (ErrType: a
// declared image/png whose bytes are HTML is refused, since the declared
// type is what the store serves it as). A file that fails is deleted.
//
// key must be one the application granted (Policy.Prefix is checked, as
// a guard): Confirm checks the file, not who may claim it.
func Confirm(ctx context.Context, store storage.Storage, key string, p Policy) (File, error) {
	if err := p.validate(); err != nil {
		return File{}, err
	}
	if !strings.HasPrefix(key, p.Prefix) || storage.ValidateKey(key) != nil {
		return File{}, fmt.Errorf("%w: key %q is not under the policy's prefix %q", ErrMalformed, key, p.Prefix)
	}
	body, info, err := store.Open(ctx, key)
	if errors.Is(err, storage.ErrNotFound) {
		return File{}, fmt.Errorf("%w: nothing was uploaded to %q", ErrNoFile, key)
	}
	if err != nil {
		return File{}, err
	}
	head := make([]byte, SniffBytes)
	n, rerr := io.ReadFull(body, head)
	_ = body.Close()
	if rerr != nil && !errors.Is(rerr, io.EOF) && !errors.Is(rerr, io.ErrUnexpectedEOF) {
		return File{}, rerr
	}
	reject := func(err error) (File, error) {
		if derr := discard(ctx, store, key); derr != nil {
			err = errors.Join(err, &CleanupError{Key: key, Err: derr})
		}
		return File{}, err
	}
	if info.Size > p.MaxBytes {
		return reject(fmt.Errorf("%w: %d bytes, more than %d", ErrTooLarge, info.Size, p.MaxBytes))
	}
	detect := p.Detect
	if detect == nil {
		detect = http.DetectContentType
	}
	detected := detect(head[:n])
	detectedType, _, err := mime.ParseMediaType(detected)
	if err != nil || !p.accepts(detectedType) {
		return reject(fmt.Errorf("%w: detected %q", ErrType, detected))
	}
	declaredType, _, err := mime.ParseMediaType(info.ContentType)
	if err != nil || declaredType != detectedType {
		return reject(fmt.Errorf("%w: declared %q, but the bytes are %q", ErrType, info.ContentType, detected))
	}
	return File{ObjectInfo: info, Filename: info.Metadata[FilenameMetadata]}, nil
}
