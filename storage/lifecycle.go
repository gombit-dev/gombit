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
