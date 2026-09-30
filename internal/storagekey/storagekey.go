// Package storagekey holds the object key rules of package storage, for
// the packages that must apply them without depending on storage itself
// (config, which validates a key prefix before any driver exists).
// storage.ValidateKey and storage.ValidatePrefix are the public forms.
package storagekey

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxKeyBytes is the longest key, in bytes (S3's limit).
const MaxKeyBytes = 1024

// MaxSegmentBytes is the longest segment of a key (the part between '/'s),
// in bytes.
const MaxSegmentBytes = 255

// Problem says why key breaks the key rules, or "" when it does not. The
// rules are documented on storage.ValidateKey.
func Problem(key string) string {
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
		case UnsafeRune(r):
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

// PrefixProblem says why prefix cannot start every key of a store, or ""
// when it can. A prefix is empty, or a valid key followed by '/', short
// enough to leave room for a key after it: prefix + key is itself a key,
// held to MaxKeyBytes.
func PrefixProblem(prefix string) string {
	if prefix == "" {
		return ""
	}
	if !strings.HasSuffix(prefix, "/") {
		return "does not end with '/'"
	}
	if p := Problem(strings.TrimSuffix(prefix, "/")); p != "" {
		return p
	}
	if len(prefix) >= MaxKeyBytes {
		return fmt.Sprintf("is %d bytes, leaving no room for a key within %d", len(prefix), MaxKeyBytes)
	}
	return ""
}

// UnsafeRune reports control characters (C0, DEL, C1), Unicode line and
// paragraph separators, and bidirectional controls.
func UnsafeRune(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) || unicode.Is(unicode.Bidi_Control, r)
}
