package storage

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxKeyBytes is the longest key, in bytes (S3's limit).
const MaxKeyBytes = 1024

// MaxSegmentBytes is the longest segment of a key (the part between '/'s),
// in bytes: the file-name limit of the filesystems under S3-compatible
// services such as MinIO, which refuse a longer one.
const MaxSegmentBytes = 255

// MaxContentTypeBytes bounds PutOptions.ContentType, parameters included.
const MaxContentTypeBytes = 256

// MaxMetadataBytes bounds a PutOptions.Metadata, measured as the metadata
// headers of an S3 request: each name, plus each value as it is sent. A
// printable-ASCII value is sent as is; any other value (and one containing
// "=?", which would read as an encoded word) as one RFC 2047 base64 word,
// S3's documented encoding for non-ASCII metadata, which is 12 bytes plus
// the base64 of its UTF-8. That is the strictest measure among S3-compatible
// services: AWS counts the decoded UTF-8, MinIO the headers as sent.
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
		for _, r := range value {
			if unsafeRune(r) {
				return fmt.Errorf("%w: metadata %q: value contains the control character %U", ErrInvalidOptions, name, r)
			}
		}
		total += len(name) + MetadataValueWireLen(value)
	}
	if total > MaxMetadataBytes {
		return fmt.Errorf("%w: metadata is %d bytes, more than %d", ErrInvalidOptions, total, MaxMetadataBytes)
	}
	return nil
}

// MetadataValueWireLen is how many bytes value takes in an S3 metadata
// header: its length when it is printable ASCII without "=?", otherwise the
// length of "=?UTF-8?B?" + base64(value) + "?=". An S3 driver that encodes
// values must encode exactly this way, so what ValidateMetadata accepts
// fits every S3-compatible service.
func MetadataValueWireLen(value string) int {
	if strings.Contains(value, "=?") {
		return 12 + base64.StdEncoding.EncodedLen(len(value))
	}
	for i := 0; i < len(value); i++ {
		if c := value[i]; c < 0x20 || c > 0x7e {
			return 12 + base64.StdEncoding.EncodedLen(len(value))
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
// when it is not: a signed URL needs a positive Expires, and a public one
// none.
func ValidateURLOptions(opts URLOptions) error {
	switch {
	case opts.Signed && opts.Expires <= 0:
		return fmt.Errorf("%w: a signed URL needs a positive lifetime, not %s", ErrInvalidOptions, opts.Expires)
	case !opts.Signed && opts.Expires != 0:
		return fmt.Errorf("%w: a public URL has no lifetime; use SignedURL", ErrInvalidOptions)
	}
	return nil
}

// ExpectSize returns r limited to exactly size bytes, for a driver
// enforcing PutOptions.Size: a source that ends early, or has a byte past
// size, fails with ErrSizeMismatch, and a size of zero means an empty
// source. The read that reaches size looks one byte further, so the
// mismatch comes with it: read the result to EOF (io.Copy, io.ReadAll).
// io.ReadFull and io.CopyN stop at a byte count and can drop an error
// returned with the last bytes. A negative size fails every read with
// ErrInvalidOptions.
func ExpectSize(r io.Reader, size int64) io.Reader {
	return &exactReader{r: r, left: size, size: size}
}

type exactReader struct {
	r          io.Reader
	left, size int64
	done       error // the result once size is reached: io.EOF, or why not
}

func (e *exactReader) Read(p []byte) (int, error) {
	if e.done != nil {
		return 0, e.done
	}
	if e.size < 0 {
		e.done = fmt.Errorf("%w: negative size %d", ErrInvalidOptions, e.size)
		return 0, e.done
	}
	n := 0
	if e.left > 0 {
		if int64(len(p)) > e.left {
			p = p[:e.left]
		}
		var err error
		n, err = e.r.Read(p)
		e.left -= int64(n)
		switch {
		case e.left > 0 && errors.Is(err, io.EOF):
			e.done = fmt.Errorf("%w: %d bytes, declared %d", ErrSizeMismatch, e.size-e.left, e.size)
			return n, e.done
		case e.left > 0 || (err != nil && !errors.Is(err, io.EOF)):
			return n, err
		case errors.Is(err, io.EOF):
			// Exactly size bytes and the source says it is done.
			e.done = io.EOF
			return n, io.EOF
		}
	}
	// size bytes have been read and the source has not said it is done:
	// look one byte further, so any excess is reported now.
	e.done = e.probe()
	return n, e.done
}

// probe reads past the declared size: io.EOF when the source is done,
// ErrSizeMismatch when it has another byte.
func (e *exactReader) probe() error {
	var one [1]byte
	for tries := 0; tries < 100; tries++ {
		n, err := e.r.Read(one[:])
		switch {
		case n > 0:
			return fmt.Errorf("%w: more than %d bytes", ErrSizeMismatch, e.size)
		case errors.Is(err, io.EOF):
			return io.EOF
		case err != nil:
			return err
		}
	}
	return io.ErrNoProgress
}
