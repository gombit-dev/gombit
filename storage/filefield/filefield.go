// Package filefield is the runtime behind storage-backed model fields
// (types.File and types.Image): the code `gombit generate` writes for a
// resource with such a field calls it to grant uploads for the field,
// accept an uploaded file into a record, and describe it on reads.
//
// A field's value is an object key in App.Storage(). Its Policy (the
// model's `storage` tag) is the prefix the field owns, the largest file,
// and the accepted types. Files are owned through storage/claims: an
// upload grant claims its key (pending), and a record takes a key only for
// an upload that passes the policy (Accept: upload.Confirm, which checks
// the bytes) and whose claim its transaction holds (claims.CreateWith), so
// a client cannot attach a file that was not uploaded for the field, or
// another record's file. The column's unique index backs that up. Uploads
// never attached stay pending until claims.Sweep removes them.
package filefield

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/field"
	"github.com/gombit-dev/gombit/storage"
	"github.com/gombit-dev/gombit/storage/claims"
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
// hex digits): it keeps the column's unique index within MySQL's limit,
// and is the longest key a claim holds (claims.MaxKeyLen).
const MaxKeyBytes = claims.MaxKeyLen

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

// Claims is the claims the generated handlers own their files through,
// over db and store, with failures to delete a released file logged to log.
func Claims(db *gorm.DB, store storage.Storage, log *zap.Logger) *claims.Claims {
	return claims.New(db, store, claims.WithWarn(func(msg string, err error) {
		log.Warn(msg, zap.Error(err))
	}))
}

// Authorize grants the upload of one file for a field with policy p
// (upload.Authorize), its key claimed in cl: the client sends the returned
// request, then submits the grant's key as the field's value.
func Authorize(ctx context.Context, store storage.Storage, cl upload.Claimer, p upload.Policy, r UploadGrantRequest) (UploadGrant, error) {
	p.Claims = cl
	g, err := upload.Authorize(ctx, store, p, r.Size, r.ContentType, r.Filename)
	if err != nil {
		return UploadGrant{}, err
	}
	return UploadGrant{Key: g.Key, Upload: g.Request}, nil
}

// Accept checks that key may become the value of a field with policy p in
// a record: nothing for no key; otherwise an upload that passes p
// (upload.Confirm under cl: under p's prefix, stored, within the size, of
// an accepted type by its bytes). A file that fails is deleted, but only
// while its claim is pending: never another record's file. Whether this
// record may take the file is decided when its transaction holds the key
// (claims.CreateWith fails with claims.ErrNotPending for a file another
// record holds, or an upload that expired).
func Accept(ctx context.Context, store storage.Storage, cl upload.Claimer, key string, p upload.Policy) error {
	if key == "" {
		return nil
	}
	p.Claims = cl
	_, err := upload.Confirm(ctx, store, key, p)
	return err
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
	d := &FileInfo{Key: key, Filename: info.StoredFilename(), Size: info.Size, ContentType: info.ContentType}
	u, err := store.URL(ctx, key, storage.PublicURL())
	if errors.Is(err, storage.ErrNotPublic) || errors.Is(err, storage.ErrUnsupported) {
		// Private, or public on a store without public URLs: a signed URL
		// still reaches it.
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

// MapError maps an error of this package (or of upload, storage and
// claims) to a D10 error for a handler.
func MapError(ctx context.Context, err error) error {
	if errors.Is(err, claims.ErrNotPending) {
		return contract.WithContext(ctx, contract.Conflict("The file is already attached to a record, or its upload has expired."))
	}
	if errors.Is(err, storage.ErrUnsupported) {
		return contract.WithContext(ctx, contract.Internal("File uploads are not configured on this server."))
	}
	return upload.MapError(ctx, err)
}

// MapCreateError maps the error of creating a record with files
// (claims.CreateWith): a file it cannot take is MapError's conflict;
// anything else is the database's (database.MapPersistError, with
// conflict and op as there).
func MapCreateError(ctx context.Context, err error, conflict, op string) error {
	if errors.Is(err, claims.ErrNotPending) {
		return MapError(ctx, err)
	}
	return database.MapPersistError(ctx, err, conflict, op)
}

// KeyOf is the key of an optional file field's value ("" for nil).
func KeyOf[T ~string](v *T) string {
	if v == nil {
		return ""
	}
	return string(*v)
}

// ResolveConcurrency bounds how many rows' files a list resolves at once.
const ResolveConcurrency = 8

// ForEach runs fn for i in [0, n), at most ResolveConcurrency at a time,
// waits for all of them, and returns the first error (after which the rest
// see ctx canceled and are not started). The generated list uses it to
// resolve its rows' files in parallel rather than one storage request
// after another.
func ForEach(ctx context.Context, n int, fn func(ctx context.Context, i int) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg    sync.WaitGroup
		once  sync.Once
		first error
	)
	sem := make(chan struct{}, ResolveConcurrency)
	for i := 0; i < n && ctx.Err() == nil; i++ {
		sem <- struct{}{}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := fn(ctx, i); err != nil {
				once.Do(func() { first = err; cancel() })
			}
		}(i)
	}
	wg.Wait()
	if first == nil {
		first = ctx.Err() // the caller's context ended before every fn ran
	}
	return first
}
