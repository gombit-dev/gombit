package storage

import (
	"context"
	"fmt"
	"strings"
)

// StoredFilename is the object's filename (the FilenameMetadata metadata),
// for display and Content-Disposition; empty when it was stored without
// one. (upload.File, which embeds ObjectInfo, has it as its Filename
// field.)
func (o ObjectInfo) StoredFilename() string { return o.Metadata[FilenameMetadata] }

// Lister is a Storage that can enumerate its objects, for inventories,
// audits, and migrations between stores. (Cleaning up abandoned uploads
// does not list the store, only the staging namespace that storage/upload
// reserves: see claims.SweepStaging.) The local, memory, and S3 drivers
// implement it.
type Lister interface {
	// List calls fn with every object whose key starts with prefix ("" for
	// all), in an order the driver chooses, until fn returns an error,
	// which List then returns. Each ObjectInfo has the Key, Size, ETag, and
	// ModTime; ContentType and Metadata may be empty (an S3 listing does
	// not carry them: Stat the object for them). fn may delete the object
	// it is given. An object stored or deleted while List runs may or may
	// not be seen.
	List(ctx context.Context, prefix string, fn func(ObjectInfo) error) error
}

// List enumerates s's objects under prefix (Lister), or fails with
// ErrUnsupported when s cannot.
func List(ctx context.Context, s Storage, prefix string, fn func(ObjectInfo) error) error {
	l, ok := s.(Lister)
	if !ok {
		return Wrap("list", prefix, fmt.Errorf("%w: %T cannot list its objects", ErrUnsupported, s))
	}
	return l.List(ctx, prefix, fn)
}

// Publisher is a Storage that copies an object in two steps, so that a
// copy whose outcome is unknown can be settled for good: S3, where the copy
// is a multipart upload whose parts are copied server-side, published by
// CompleteMultipartUpload and fenced by AbortMultipartUpload.
// PreparePublish, Publish and Fence (the functions) use it, and fall back
// to an in-process copy for a BoundedWriter; for any other store they fail
// with ErrUnsupported.
type Publisher interface {
	// PreparePublish readies a copy of the object at src (its bytes,
	// ContentType and Metadata) to dst, and returns a token naming it (at
	// most MaxPublicationToken bytes, by construction). Nothing is published yet: record the
	// token durably before Publish, so that the copy can be fenced whatever
	// happens to the caller. A missing src is ErrNotFound. When it fails
	// but returns a token, something of the copy may remain (a request
	// whose answer was lost): record the token and Fence it.
	PreparePublish(ctx context.Context, src, dst string) (string, error)
	// Publish publishes the prepared copy at dst. A failure whose outcome
	// is unknown (the request sent, no definite answer) is
	// ErrUnknownOutcome: the copy may be published, now or later; Fence
	// settles it.
	Publish(ctx context.Context, token string) (ObjectInfo, error)
	// Fence makes sure the copy named by token can never be published
	// after it returns nil: it was published already, or never will be.
	// It also removes what it can of an unpublished copy, but proves no
	// more than that nothing will be published (on S3 a part still being
	// processed may be stored afterwards; it is never an object). It is
	// idempotent. An error means nothing is proven yet: try again.
	Fence(ctx context.Context, token string) error
}

// MaxPublicationToken is the longest token a Publisher returns: it fits a
// TEXT column on every supported database.
const MaxPublicationToken = 60000

// BoundedWriter is a Storage whose writes are bounded by their calls: a Put
// publishes, if at all, before it returns, and never once its context has
// ended (the driver checks it just before publishing). The local and
// memory drivers are. A store that writes through a remote service
// generally is not (a request it sent may complete later), and cannot
// declare it; to take part in storage/claims it must be a Publisher.
type BoundedWriter interface {
	Storage
	// BoundedWrites declares the guarantee; it does nothing.
	BoundedWrites()
}

// CheckOwnable reports whether storage/claims can own s's objects: s must
// be a Publisher (its copies can be fenced) or a BoundedWriter (its writes
// end with their calls). Any other store fails with ErrUnsupported: nothing
// proves that a write it sent will not complete after its claim is gone.
func CheckOwnable(s Storage) error {
	switch s.(type) {
	case Publisher, BoundedWriter:
		return nil
	}
	return fmt.Errorf("%w: %T can neither fence a publication (storage.Publisher) nor bound its writes (storage.BoundedWriter)", ErrUnsupported, s)
}

// fallbackToken is the token of a copy within the application process
// (a BoundedWriter): the source and destination keys.
const fallbackToken = "copy\n"

// PreparePublish readies a copy of src to dst in s (Publisher), and
// returns its token. For a BoundedWriter nothing happens yet: the token
// names the keys, and Publish copies in the process. Any other store fails
// with ErrUnsupported.
func PreparePublish(ctx context.Context, s Storage, src, dst string) (string, error) {
	if p, ok := s.(Publisher); ok {
		return p.PreparePublish(ctx, src, dst)
	}
	if err := CheckOwnable(s); err != nil {
		return "", Wrap("publish", dst, err)
	}
	if err := ValidateKey(src); err != nil {
		return "", Wrap("publish", src, err)
	}
	if err := ValidateKey(dst); err != nil {
		return "", Wrap("publish", dst, err)
	}
	return fallbackToken + src + "\n" + dst, nil
}

// Publish publishes the copy token names (PreparePublish). For a
// BoundedWriter, it copies within the process (Open, then Put with src's
// ContentType, Metadata and size, IfAbsent), under ctx: the copy publishes
// nothing once Publish has returned, or after ctx's deadline. Any other
// store fails with ErrUnsupported.
func Publish(ctx context.Context, s Storage, token string) (ObjectInfo, error) {
	if p, ok := s.(Publisher); ok {
		return p.Publish(ctx, token)
	}
	if err := CheckOwnable(s); err != nil {
		return ObjectInfo{}, Wrap("publish", "", err)
	}
	src, dst, ok := strings.Cut(strings.TrimPrefix(token, fallbackToken), "\n")
	if !ok || !strings.HasPrefix(token, fallbackToken) {
		return ObjectInfo{}, Wrap("publish", "", fmt.Errorf("%w: a malformed publication token", ErrInvalidOptions))
	}
	body, info, err := s.Open(ctx, src)
	if err != nil {
		return ObjectInfo{}, err
	}
	defer func() { _ = body.Close() }()
	return s.Put(ctx, dst, body, PutOptions{
		ContentType: info.ContentType,
		Metadata:    info.Metadata,
		Size:        KnownSize(info.Size),
		IfAbsent:    true,
	})
}

// Fence makes sure the copy token names can never be published after it
// returns nil (Publisher.Fence). For a BoundedWriter it does nothing: an
// in-process copy publishes nothing after its Publish call's context
// ended, so a caller that bounded that context (a lease) need only wait
// for the bound. Any other store fails with ErrUnsupported: nothing can be
// proven about it.
func Fence(ctx context.Context, s Storage, token string) error {
	if p, ok := s.(Publisher); ok {
		return p.Fence(ctx, token)
	}
	if err := CheckOwnable(s); err != nil {
		return Wrap("fence", "", err)
	}
	return nil
}
