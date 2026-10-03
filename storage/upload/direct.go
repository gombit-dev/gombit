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
	"time"

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
// Under Policy.Claims the upload is staged: the key is claimed (Stage), and
// the request uploads to StagingKey(key), never to the key itself, which
// only Confirm writes, by copying the staged object once it passes. A PUT
// the application cannot end (S3 checks a presigned URL's expiry when the
// request starts) can then only ever publish a staging object.
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
	ttl := p.GrantExpiry
	if ttl == 0 {
		ttl = DefaultGrantExpiry
	}
	// Under Claims the client writes only the staging key. The PUT may
	// start until the grant expires, and the app's storage route aborts it
	// storage.SignedUploadTimeout later: the lease of the staging key's
	// writes through that route.
	target := key
	if p.Claims != nil {
		if err := storage.CheckOwnable(store); err != nil {
			return Grant{}, fmt.Errorf("upload: claims: %w", err)
		}
		if err := p.Claims.Stage(ctx, key, p.Scope, time.Now().Add(ttl+storage.SignedUploadTimeout)); err != nil {
			return Grant{}, fmt.Errorf("upload: claim %q: %w", key, err)
		}
		target = StagingKey(key)
	}
	md := maps.Clone(p.Metadata)
	if filename = CleanFilename(filename); filename != "" {
		if md == nil {
			md = map[string]string{}
		}
		fitMetadata(md, filename)
	}
	req, err := storage.UploadURL(ctx, store, target, storage.UploadURLOptions{
		Expires:     ttl,
		Size:        size,
		ContentType: contentType,
		Metadata:    md,
	})
	if err != nil {
		unclaim(ctx, p, key)
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
// Under Policy.Claims, the upload was staged (Authorize): Confirm promotes
// the claim (Claimer.Promote), checks the staged object, and publishes a
// copy of it at key (promote: prepared, recorded on the claim, then
// published, under the promotion's lease, Policy.UploadTimeout), then
// deletes the staged object. A refused staged object is deleted and the
// claim goes back to pending, so the grant can upload again. A copy whose
// outcome is unknown leaves the claim promoting with the copy recorded
// (it may exist): a retried Confirm then checks key as it is, and the
// claims protocol fences the copy before it ever forgets the key. A key that is not a staged upload awaiting
// confirmation (already promoted or held: a retried confirmation; or
// stored by Save) is checked as it is, and a refusal deletes nothing.
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
	if p.Claims == nil {
		return check(ctx, store, key, p, func(ctx context.Context) error { return discard(ctx, store, p, key) })
	}
	deadline := time.Now().Add(uploadTimeout(p))
	promoted, err := p.Claims.Promote(ctx, key, p.Scope, deadline)
	if err != nil {
		return File{}, fmt.Errorf("upload: promote %q: %w", key, err)
	}
	if !promoted {
		// Not a staged upload of this scope awaiting confirmation: a
		// retried confirmation, or a key stored by Save. It must still
		// have been claimed for this scope, not another field's.
		belongs, err := p.Claims.Belongs(ctx, key, p.Scope)
		if err != nil {
			return File{}, fmt.Errorf("upload: %q: %w", key, err)
		}
		if !belongs {
			return File{}, fmt.Errorf("%w: %q was not uploaded for %q", ErrMalformed, key, p.Scope)
		}
		return check(ctx, store, key, p, nil)
	}
	staged := StagingKey(key)
	if _, err := check(ctx, store, staged, p, func(ctx context.Context) error {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		return store.Delete(dctx, staged)
	}); err != nil {
		return File{}, unpromote(ctx, p, key, err) // nothing was copied to key
	}
	info, err := promote(ctx, store, p, key, deadline)
	if err != nil {
		return File{}, err
	}
	return File{ObjectInfo: info, Filename: info.StoredFilename()}, nil
}

// promote publishes the staged object of key to key, whose claim is
// promoting: the copy is prepared (storage.PreparePublish), its token
// recorded on the claim (Claimer.Publishing) before anything can publish,
// then published under deadline, and the staged object deleted. A copy
// whose outcome is unknown is never assumed not to have happened: the
// claim stays promoting with the token, and the claims protocol fences the
// copy (storage.Fence) before it ever forgets the key.
func promote(ctx context.Context, store storage.Storage, p Policy, key string, deadline time.Time) (storage.ObjectInfo, error) {
	staged := StagingKey(key)
	cctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	token, err := storage.PreparePublish(cctx, store, staged, key)
	if err != nil {
		err = fmt.Errorf("upload: promote %q: %w", key, err)
		if token == "" {
			return storage.ObjectInfo{}, unpromote(ctx, p, key, err) // nothing remains
		}
		// Something of the copy may remain (a part stored after its upload
		// was aborted): keep it recorded on the claim, which stays
		// promoting until the protocol has fenced it.
		if perr := p.Claims.Publishing(ctx, key, token); perr != nil {
			fence(ctx, store, token)
			err = errors.Join(err, perr)
		}
		return storage.ObjectInfo{}, err
	}
	if err := p.Claims.Publishing(ctx, key, token); err != nil {
		// The claim moved on (a sweep abandoned it), or could not be
		// updated: publish nothing. Only this call holds the token, so an
		// unrecorded copy is never published; fencing it frees it now.
		fence(ctx, store, token)
		return storage.ObjectInfo{}, fmt.Errorf("upload: promote %q: %w", key, err)
	}
	info, err := storage.Publish(cctx, store, token)
	if err != nil {
		err = fmt.Errorf("upload: promote %q: %w", key, err)
		if errors.Is(err, storage.ErrUnknownOutcome) || errors.Is(err, storage.ErrExists) {
			// The copy may be published (or something is at key): the
			// claim stays promoting, with the token to fence.
			return storage.ObjectInfo{}, err
		}
		// A definite failure: once fenced, nothing can be published.
		if fence(ctx, store, token) {
			return storage.ObjectInfo{}, unpromote(ctx, p, key, err)
		}
		return storage.ObjectInfo{}, err
	}
	// The staged object has served its purpose (if this fails,
	// claims.SweepStaging deletes it).
	dctx, dcancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer dcancel()
	_ = store.Delete(dctx, staged)
	return info, nil
}

// fence fences the copy token names, on a context detached from ctx, and
// reports whether it succeeded.
func fence(ctx context.Context, store storage.Storage, token string) bool {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	return storage.Fence(fctx, store, token) == nil
}

// unpromote puts key's claim back to pending (nothing was published to
// key), on a context detached from ctx, and returns err with any failure
// to.
func unpromote(ctx context.Context, p Policy, key string, err error) error {
	uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if uerr := p.Claims.Unpromote(uctx, key); uerr != nil {
		err = errors.Join(err, uerr)
	}
	return err
}

// check checks the object at objKey against p as Confirm does. A refused
// object is deleted with discard (nothing is deleted when it is nil).
func check(ctx context.Context, store storage.Storage, objKey string, p Policy, discard func(context.Context) error) (File, error) {
	body, info, err := store.Open(ctx, objKey)
	if errors.Is(err, storage.ErrNotFound) {
		return File{}, fmt.Errorf("%w: nothing was uploaded to %q", ErrNoFile, objKey)
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
		if discard == nil {
			return File{}, err
		}
		if derr := discard(ctx); derr != nil {
			err = errors.Join(err, &CleanupError{Key: objKey, Err: derr})
		}
		return File{}, err
	}
	if info.Size > p.MaxBytes {
		return reject(fmt.Errorf("%w: %d bytes, more than %d", ErrTooLarge, info.Size, p.MaxBytes))
	}
	// What the store kept beyond ObjectInfo (S3's unsigned standard headers,
	// such as Content-Encoding): an upload that set any of it is not the
	// object its grant described.
	if v, ok := store.(storage.UploadVerifier); ok {
		switch err := v.VerifyUpload(ctx, objKey); {
		case errors.Is(err, storage.ErrInvalidOptions):
			return reject(fmt.Errorf("%w: %w", ErrMalformed, err))
		case err != nil:
			return File{}, err // the check itself failed: nothing is refused yet
		}
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
	return File{ObjectInfo: info, Filename: info.StoredFilename()}, nil
}
