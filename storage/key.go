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

// MaxContentTypeBytes bounds PutOptions.ContentType, parameters included.
const MaxContentTypeBytes = 256

// MaxMetadataBytes bounds a PutOptions.Metadata, measured as S3 measures
// user metadata on the wire: the names and values together, with every
// byte of a value outside printable ASCII counted as three (its
// percent-encoded length), so a driver that has to encode values still
// fits S3's 2 KB limit.
const MaxMetadataBytes = 2048

// ValidateKey reports whether key is a valid object key, as ErrInvalidKey
// (wrapped with the reason) when it is not.
//
// A key is a '/'-separated path of one or more segments, the same on every
// driver, so a key that works in development works in production:
//
//   - 1 to MaxKeyBytes bytes of valid UTF-8;
//   - no leading or trailing '/', and no empty segment ("a//b");
//   - no "." or ".." segment, so a key can never climb out of a prefix or,
//     on the local driver, out of the storage root;
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
	if opts.Size < 0 {
		return fmt.Errorf("%w: negative size %d", ErrInvalidOptions, opts.Size)
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
		for _, r := range value {
			if unsafeRune(r) {
				return fmt.Errorf("%w: metadata %q: value contains the control character %U", ErrInvalidOptions, name, r)
			}
		}
		total += len(name) + encodedLen(value)
	}
	if total > MaxMetadataBytes {
		return fmt.Errorf("%w: metadata is %d bytes, more than %d", ErrInvalidOptions, total, MaxMetadataBytes)
	}
	return nil
}

// unsafeRune reports control characters (C0, DEL, C1), Unicode line and
// paragraph separators, and bidirectional controls.
func unsafeRune(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) || unicode.Is(unicode.Bidi_Control, r)
}

// encodedLen is s's length with every byte outside printable ASCII
// counted as three, its percent-encoded length.
func encodedLen(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c > 0x7e {
			n += 3
		} else {
			n++
		}
	}
	return n
}

// ValidateURLOptions reports whether opts is valid, as ErrInvalidOptions
// when it is not (a negative Expires).
func ValidateURLOptions(opts URLOptions) error {
	if opts.Expires < 0 {
		return fmt.Errorf("%w: negative expiry %s", ErrInvalidOptions, opts.Expires)
	}
	return nil
}
