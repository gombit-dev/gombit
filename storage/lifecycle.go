package storage

import (
	"context"
	"fmt"
)

// StoredFilename is the object's filename (the FilenameMetadata metadata),
// for display and Content-Disposition; empty when it was stored without
// one. (upload.File, which embeds ObjectInfo, has it as its Filename
// field.)
func (o ObjectInfo) StoredFilename() string { return o.Metadata[FilenameMetadata] }

// Lister is a Storage that can enumerate its objects, for inventories,
// audits, and migrations between stores. (Cleaning up abandoned uploads
// does not list: see storage/claims.) The local, memory, and S3 drivers
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

// Copier is a Storage that copies an object within itself without the
// bytes passing through the application: S3's CopyObject. Copy (the
// function) uses it, and falls back to Open and Put for any other store.
type Copier interface {
	// Copy stores a copy of the object at src under dst: its bytes,
	// ContentType and Metadata. A missing src is ErrNotFound. The copy is
	// conditional where the backend supports it: an object already at dst
	// is then ErrExists and is left as it was (S3's If-None-Match, which
	// AWS honors and some S3-compatible services ignore, replacing dst).
	// Do not rely on it for exclusivity. A failure whose outcome the
	// driver cannot know (the request sent, no definite answer) is
	// ErrUnknownOutcome, as for Put: the copy may exist.
	Copy(ctx context.Context, src, dst string) (ObjectInfo, error)
}

// Copy stores a copy of the object at src under dst, with s's Copier, or
// else by reading src and putting it under dst (Open, then Put with src's
// ContentType, Metadata and size, IfAbsent).
func Copy(ctx context.Context, s Storage, src, dst string) (ObjectInfo, error) {
	if c, ok := s.(Copier); ok {
		return c.Copy(ctx, src, dst)
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
