package storage

import (
	"fmt"
	"strings"
	"time"
)

// FilenameMetadata is the metadata name of an object's filename, for
// display and Content-Disposition (storage/upload stores the client's
// filename under it; presign.Handler serves it).
const FilenameMetadata = "filename"

// MaxURLExpiry is the longest a signed URL may live: S3's limit for a
// presigned URL (SigV4), applied on every driver so a lifetime that works
// in development works in production.
const MaxURLExpiry = 7 * 24 * time.Hour

// Visibility. An object is public when its key is under the store's
// public prefix ("public/" unless configured otherwise; empty makes no
// object public), and private otherwise. A public object has a permanent
// URL anyone can fetch (URL with PublicURL); a private one is reached
// through the application, or through a signed URL that expires (URL with
// SignedURL). Visibility follows the key, not a flag stored with the
// object, because that is what a bucket policy or a CDN can serve: to make
// an object public, store it under the public prefix.

// IsPublic reports whether key is public under publicPrefix: publicPrefix
// is not empty and key starts with it.
func IsPublic(publicPrefix, key string) bool {
	return publicPrefix != "" && strings.HasPrefix(key, publicPrefix)
}

// ValidatePublicPrefix reports whether prefix can be a store's public
// prefix: empty (no object is public), or a key path ending with '/'.
func ValidatePublicPrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	if !strings.HasSuffix(prefix, "/") {
		return fmt.Errorf("%w: public prefix %q must end with '/'", ErrInvalidOptions, prefix)
	}
	if err := ValidateKey(prefix + "k"); err != nil {
		return fmt.Errorf("%w: public prefix %q: %v", ErrInvalidOptions, prefix, err)
	}
	return nil
}

// EscapeKey returns key as a URL path: each segment percent-encoded except
// for unreserved characters (letters, digits, '-', '.', '_', '~'), the '/'
// between segments kept. Every byte that could mean something else in a
// URL, or that a server might decode differently ('+', ';', '%'), is
// encoded.
func EscapeKey(key string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(key))
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9',
			c == '-', c == '.', c == '_', c == '~', c == '/':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&15])
		}
	}
	return b.String()
}
