package storage

import (
	"fmt"
	"mime"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxKeyBytes is the longest key, in bytes (S3's limit).
const MaxKeyBytes = 1024

// MaxSegmentBytes is the longest segment of a key (the part between '/'s),
// in bytes. AWS S3 itself allows longer, but MinIO, an S3-compatible
// service that stores objects as files, refuses a longer segment
// (XMinioInvalidObjectName), so a portable key stays within it.
const MaxSegmentBytes = 255

// MaxContentTypeBytes bounds PutOptions.ContentType, parameters included.
const MaxContentTypeBytes = 256

// MaxMetadataBytes bounds a PutOptions.Metadata, measured as the metadata
// headers of an S3 request: each header name ("x-amz-meta-" and the name)
// plus each value as it is sent, a value that is not printable ASCII being
// RFC 2047 encoded (mime.BEncoding, words of at most 75 characters), S3's
// documented encoding for non-ASCII metadata. That is the strictest measure
// among S3-compatible services (MinIO counts exactly this; AWS the decoded
// UTF-8).
const MaxMetadataBytes = 2048

// ValidateKey reports whether key is a valid object key, as ErrInvalidKey
// (wrapped with the reason) when it is not.
//
// A key is a '/'-separated path of one or more segments, the same on every
// driver, so a key that works in development works in production:
//
//   - 1 to MaxKeyBytes bytes of valid UTF-8;
//   - no leading or trailing '/', and no empty segment ("a//b");
//   - no segment that is "." or "..", or that ends with '.' or a space:
//     Windows strips a trailing dot or space from a path component, so
//     "..." or ".. " would otherwise become ".." on a filesystem there,
//     and "a." would collide with "a";
//   - no segment longer than MaxSegmentBytes;
//   - no backslash (a path separator on Windows), no control character
//     (NUL, newline, DEL, the C1 controls), and no Unicode line separator
//     or bidirectional control (which can split a log line or disguise a
//     name in a listing: "invoice\u202efdp.exe").
//
// A key is an address, not a filename a client chose: build keys on the
// server and keep a client's filename as metadata.
func ValidateKey(key string) error {
	reason := keyProblem(key)
	if reason == "" {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrInvalidKey, reason)
}

func keyProblem(key string) string {
	switch {
	case key == "":
		return "empty"
	case len(key) > MaxKeyBytes:
		return fmt.Sprintf("longer than %d bytes", MaxKeyBytes)
	case !utf8.ValidString(key):
		return "not valid UTF-8"
	case strings.HasPrefix(key, "/"):
		return "starts with '/'"
	case strings.HasSuffix(key, "/"):
		return "ends with '/'"
	}
	for _, r := range key {
		switch {
		case r == '\\':
			return "contains a backslash"
		case unsafeRune(r):
			return fmt.Sprintf("contains the control character %U", r)
		}
	}
	for _, seg := range strings.Split(key, "/") {
		switch seg {
		case "":
			return "contains an empty segment"
		case ".", "..":
			return fmt.Sprintf("contains a %q segment", seg)
		}
		if last := seg[len(seg)-1]; last == '.' || last == ' ' {
			return fmt.Sprintf("has a segment ending with %q (%q)", last, seg)
		}
		if len(seg) > MaxSegmentBytes {
			return fmt.Sprintf("has a %d-byte segment, longer than %d", len(seg), MaxSegmentBytes)
		}
	}
	return ""
}

// ValidatePutOptions reports whether opts is valid, as ErrInvalidOptions
// (wrapped with the reason) when it is not: the content type must parse as
// a type/subtype media type of at most MaxContentTypeBytes, Size must not be negative, and Metadata must pass
// ValidateMetadata.
func ValidatePutOptions(opts PutOptions) error {
	if len(opts.ContentType) > MaxContentTypeBytes {
		return fmt.Errorf("%w: content type is %d bytes, more than %d", ErrInvalidOptions, len(opts.ContentType), MaxContentTypeBytes)
	}
	// The raw string is what a driver stores and later sends as a header:
	// check it, not only what mime.ParseMediaType makes of it (the parser
	// skips CR, LF, tab, and Unicode line separators as whitespace).
	for _, r := range opts.ContentType {
		if unsafeRune(r) {
			return fmt.Errorf("%w: content type %q contains the control character %U", ErrInvalidOptions, opts.ContentType, r)
		}
	}
	if opts.ContentType != "" {
		mediaType, _, err := mime.ParseMediaType(opts.ContentType)
		if err != nil {
			return fmt.Errorf("%w: content type %q: %v", ErrInvalidOptions, opts.ContentType, err)
		}
		// ParseMediaType accepts a bare token ("text"); a stored object
		// needs a type and a subtype, as HTTP sends it.
		if typ, sub, ok := strings.Cut(mediaType, "/"); !ok || typ == "" || sub == "" || strings.Contains(sub, "/") {
			return fmt.Errorf("%w: content type %q: want type/subtype", ErrInvalidOptions, opts.ContentType)
		}
	}
	if opts.Size != nil && *opts.Size < 0 {
		return fmt.Errorf("%w: negative size %d", ErrInvalidOptions, *opts.Size)
	}
	return ValidateMetadata(opts.Metadata)
}

// ValidateMetadata reports whether md is valid user metadata, as
// ErrInvalidOptions when it is not: names of lower-case ASCII letters,
// digits, and '-' (portable as HTTP header suffixes, which are
// case-insensitive); values of UTF-8 without control characters; names and
// values together at most MaxMetadataBytes.
func ValidateMetadata(md map[string]string) error {
	total := 0
	for name, value := range md {
		if name == "" {
			return fmt.Errorf("%w: empty metadata name", ErrInvalidOptions)
		}
		for i := 0; i < len(name); i++ {
			c := name[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return fmt.Errorf("%w: metadata name %q: only a-z, 0-9, and '-'", ErrInvalidOptions, name)
			}
		}
		if !utf8.ValidString(value) {
			return fmt.Errorf("%w: metadata %q: value is not valid UTF-8", ErrInvalidOptions, name)
		}
		// HTTP trims a header value's surrounding whitespace (Go's
		// transport does; SigV4 signs the trimmed value), so an S3 driver
		// could not give such a value back as given.
		if strings.TrimLeft(value, " ") != value || strings.TrimRight(value, " ") != value {
			return fmt.Errorf("%w: metadata %q: value has leading or trailing spaces, which HTTP headers drop", ErrInvalidOptions, name)
		}
		for _, r := range value {
			if unsafeRune(r) {
				return fmt.Errorf("%w: metadata %q: value contains the control character %U", ErrInvalidOptions, name, r)
			}
		}
		// "=?" starts an RFC 2047 encoded word: a value holding one could
		// not be told apart from an encoded value on the way back.
		if strings.Contains(value, "=?") {
			return fmt.Errorf("%w: metadata %q: value contains \"=?\", which starts an RFC 2047 encoded word", ErrInvalidOptions, name)
		}
		total += len(metadataHeaderPrefix) + len(name) + MetadataValueWireLen(value)
	}
	if total > MaxMetadataBytes {
		return fmt.Errorf("%w: metadata is %d bytes, more than %d", ErrInvalidOptions, total, MaxMetadataBytes)
	}
	return nil
}

// metadataHeaderPrefix starts every S3 user-metadata header name.
const metadataHeaderPrefix = "x-amz-meta-"

// MetadataValueWireLen is how many bytes value takes in an S3 metadata
// header: its length when it is printable ASCII, otherwise the length of
// its RFC 2047 encoding (mime.BEncoding.Encode("UTF-8", value)), which an
// S3 driver must send.
func MetadataValueWireLen(value string) int {
	for i := 0; i < len(value); i++ {
		if c := value[i]; c < 0x20 || c > 0x7e {
			return len(mime.BEncoding.Encode("UTF-8", value))
		}
	}
	return len(value)
}

// unsafeRune reports control characters (C0, DEL, C1), Unicode line and
// paragraph separators, and bidirectional controls.
func unsafeRune(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) || unicode.Is(unicode.Bidi_Control, r)
}

// ValidateURLOptions reports whether opts is valid, as ErrInvalidOptions
// when it is not: a signed URL needs a positive Expires of at most
// MaxURLExpiry, and a public one none.
func ValidateURLOptions(opts URLOptions) error {
	switch {
	case opts.Signed && opts.Expires <= 0:
		return fmt.Errorf("%w: a signed URL needs a positive lifetime, not %s", ErrInvalidOptions, opts.Expires)
	case !opts.Signed && opts.Expires != 0:
		return fmt.Errorf("%w: a public URL has no lifetime; use SignedURL", ErrInvalidOptions)
	case opts.Expires > MaxURLExpiry:
		return fmt.Errorf("%w: a signed URL lives at most %s, not %s", ErrInvalidOptions, MaxURLExpiry, opts.Expires)
	}
	return nil
}
