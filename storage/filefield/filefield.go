// Package filefield is the runtime behind storage-backed model fields
// (types.File and types.Image): the code `gombit generate` writes for a
// resource with such a field calls it to grant uploads for the field,
// accept an uploaded file into a record, and describe it on reads.
//
// A field's value is an object key in App.Storage(). Its Policy (the
// model's `storage` tag) is the prefix the field owns, the largest file,
// and the accepted types. A record accepts a key only for an upload that
// passes the policy (upload.Confirm, which checks the bytes) and that no
// other record holds (the column's unique index is the ownership: one
// record per file), so a client cannot attach a file it did not upload or
// another record's file. Files never attached are removed by
// storage.Sweep with ReferencedBy.
package filefield

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/field"
	"github.com/gombit-dev/gombit/storage"
	"github.com/gombit-dev/gombit/storage/upload"
)

// DefaultMaxBytes is a field's largest file unless its tag says otherwise.
const DefaultMaxBytes = 10 << 20

// ImageTypes are the media types an image field accepts unless its tag
// says otherwise: the raster formats browsers display and
// http.DetectContentType recognizes (never SVG, which can carry script).
var ImageTypes = []string{"image/png", "image/jpeg", "image/gif", "image/webp"}

// LinkTTL is how long the signed URL of a private file in a read lives.
const LinkTTL = 15 * time.Minute

// MaxKeyBytes bounds a file column (a generated key is the prefix and 32
// hex digits): it keeps the column's unique index within MySQL's limit.
const MaxKeyBytes = 512

// ErrReferenced: the file is already another record's.
var ErrReferenced = errors.New("filefield: the file belongs to another record")

// FileInfo is a file as a read returns it.
type FileInfo struct {
	// Key identifies the file; send it back to keep the file on a write.
	Key string `json:"key" doc:"The file's key; send it back to keep the file."`
	// Filename is the name it was uploaded with.
	Filename string `json:"filename,omitempty" doc:"The name the file was uploaded with."`
	// Size is its length in bytes.
	Size int64 `json:"size" doc:"The file's length in bytes."`
	// ContentType is its media type.
	ContentType string `json:"content_type,omitempty" doc:"The file's media type."`
	// URL downloads it: permanent for a public file, signed and short-lived
	// (LinkTTL) otherwise; empty when the store has no URLs.
	URL string `json:"url,omitempty" doc:"Where to download the file; a signed URL expires."`
	// Missing is true when the record names a file the store no longer has.
	Missing bool `json:"missing,omitempty" doc:"The record names a file the store no longer has."`
}

// Policy parses a field's `storage` tag ("prefix=docs/file/;max_bytes=5242880;
// types=application/pdf,image/png") into its upload policy. kind is
// field.File or field.Image (which defaults to ImageTypes; a file field to
// any type); defaultPrefix is used when the tag names none. Unknown keys
// are errors, so a typo does not silently loosen a limit.
func Policy(tag string, kind field.Kind, defaultPrefix string) (upload.Policy, error) {
	p := upload.Policy{MaxBytes: DefaultMaxBytes, Prefix: defaultPrefix, Types: []string{"*/*"}}
	if kind == field.Image {
		p.Types = append([]string(nil), ImageTypes...)
	} else if kind != field.File {
		return upload.Policy{}, fmt.Errorf("filefield: kind %q is not a file kind", kind)
	}
	for _, part := range strings.Split(tag, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, value, ok := strings.Cut(part, "=")
		if !ok {
			return upload.Policy{}, fmt.Errorf("filefield: storage tag %q: want name=value", part)
		}
		switch strings.TrimSpace(name) {
		case "prefix":
			p.Prefix = strings.TrimSpace(value)
		case "max_bytes":
			n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil || n <= 0 {
				return upload.Policy{}, fmt.Errorf("filefield: storage tag max_bytes=%q: want a positive number of bytes", value)
			}
			p.MaxBytes = n
		case "types":
			p.Types = nil
			for _, t := range strings.Split(value, ",") {
				if t = strings.TrimSpace(t); t != "" {
					p.Types = append(p.Types, t)
				}
			}
		default:
			return upload.Policy{}, fmt.Errorf("filefield: storage tag has an unknown setting %q (prefix, max_bytes, types)", name)
		}
	}
	if p.Prefix == "" {
		return upload.Policy{}, errors.New("filefield: a file field needs a prefix of its own (storage:\"prefix=...\")")
	}
	if len(p.Prefix)+32 > MaxKeyBytes {
		return upload.Policy{}, fmt.Errorf("filefield: prefix %q is too long for a %d-byte key", p.Prefix, MaxKeyBytes)
	}
	if err := p.Validate(); err != nil {
		return upload.Policy{}, err
	}
	return p, nil
}

// UploadGrantRequest is the body of a request for an upload grant.
type UploadGrantRequest struct {
	Size        int64  `json:"size" minimum:"0" doc:"The file's exact length in bytes."`
	ContentType string `json:"content_type" doc:"The file's media type."`
	Filename    string `json:"filename,omitempty" doc:"The file's name, kept for display."`
}

// UploadGrant is a granted upload as the API returns it: the key to submit
// as the field's value, and the request that uploads the file.
type UploadGrant struct {
	Key    string                `json:"key" doc:"The file's key; submit it as the field's value once uploaded."`
	Upload storage.UploadRequest `json:"upload" doc:"Send the file's bytes with this request, before it expires."`
}

// Authorize grants the upload of one file for a field with policy p
// (upload.Authorize): the client sends the returned request, then submits
// the grant's key as the field's value.
func Authorize(ctx context.Context, store storage.Storage, p upload.Policy, r UploadGrantRequest) (UploadGrant, error) {
	g, err := upload.Authorize(ctx, store, p, r.Size, r.ContentType, r.Filename)
	if err != nil {
		return UploadGrant{}, err
	}
	return UploadGrant{Key: g.Key, Upload: g.Request}, nil
}

// Accept checks that key may become the value of column in a new record of
// model: nothing for no key; otherwise no record holds it yet
// (ErrReferenced), and it is an upload that passes p (upload.Confirm: under
// p's prefix, stored, within the size, of an accepted type by its bytes;
// a file that fails is deleted).
func Accept(ctx context.Context, db *gorm.DB, store storage.Storage, model any, column, key string, p upload.Policy) error {
	if key == "" {
		return nil
	}
	used, err := ReferencedBy(db, model, column)(ctx, key)
	if err != nil {
		return err
	}
	if used {
		return ErrReferenced
	}
	_, err = upload.Confirm(ctx, store, key, p)
	return err
}

// ReferencedBy returns whether a record of model holds key in column: the
// lookup storage.Sweep needs to remove the field's abandoned uploads.
func ReferencedBy(db *gorm.DB, model any, column string) func(ctx context.Context, key string) (bool, error) {
	return func(ctx context.Context, key string) (bool, error) {
		var n int64
		err := db.WithContext(ctx).Model(model).Where(clause.Eq{Column: clause.Column{Name: column}, Value: key}).Limit(1).Count(&n).Error
		return n > 0, err
	}
}

// Resolve describes the file with key for a read: nil for no key; Missing
// when the store no longer has it.
func Resolve(ctx context.Context, store storage.Storage, key string) (*FileInfo, error) {
	if key == "" {
		return nil, nil
	}
	info, err := store.Stat(ctx, key)
	if errors.Is(err, storage.ErrNotFound) {
		return &FileInfo{Key: key, Missing: true}, nil
	}
	if err != nil {
		return nil, err
	}
	d := &FileInfo{Key: key, Filename: info.Filename(), Size: info.Size, ContentType: info.ContentType}
	u, err := store.URL(ctx, key, storage.PublicURL())
	if errors.Is(err, storage.ErrNotPublic) {
		u, err = store.URL(ctx, key, storage.SignedURL(LinkTTL))
	}
	switch {
	case errors.Is(err, storage.ErrUnsupported):
	case err != nil:
		return nil, err
	default:
		d.URL = u
	}
	return d, nil
}

// MapError maps an error of this package (or of upload and storage) to a
// D10 error for a handler.
func MapError(ctx context.Context, err error) error {
	if errors.Is(err, ErrReferenced) {
		return contract.WithContext(ctx, contract.Conflict("The file is already attached to another record."))
	}
	if errors.Is(err, storage.ErrUnsupported) {
		return contract.WithContext(ctx, contract.Internal("File uploads are not configured on this server."))
	}
	return upload.MapError(ctx, err)
}

// KeyOf is the key of an optional file field's value ("" for nil).
func KeyOf[T ~string](v *T) string {
	if v == nil {
		return ""
	}
	return string(*v)
}
