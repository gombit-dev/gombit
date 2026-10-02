package storage_test

import (
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/storage"
)

func TestEscapeKey(t *testing.T) {
	for in, want := range map[string]string{
		"avatars/1.png":        "avatars/1.png",
		"a b/c+d;e%f?g#h&i=j":  "a%20b/c%2Bd%3Be%25f%3Fg%23h%26i%3Dj",
		"ré/~x_y-z.":           "r%C3%A9/~x_y-z.",
		"..hidden/name..ext":   "..hidden/name..ext",
		"colon:at@dollar$!'()": "colon%3Aat%40dollar%24%21%27%28%29",
	} {
		got := storage.EscapeKey(in)
		if got != want {
			t.Errorf("EscapeKey(%q) = %q, want %q", in, got, want)
		}
		// It is a path that decodes back to the key.
		if u, err := url.Parse("https://h/" + got); err != nil || u.Path != "/"+in {
			t.Errorf("EscapeKey(%q) = %q decodes to %q, %v", in, got, u.Path, err)
		}
	}
}

func TestVisibility(t *testing.T) {
	for _, tc := range []struct {
		prefix, key string
		public      bool
	}{
		{"public/", "public/a.png", true},
		{"public/", "public/nested/a.png", true},
		{"public/", "publicity.png", false},
		{"public/", "private/public/a.png", false},
		{"public/", "Public/a.png", false},
		{"", "public/a.png", false},
		{"media/public/", "media/public/a", true},
	} {
		if got := storage.IsPublic(tc.prefix, tc.key); got != tc.public {
			t.Errorf("IsPublic(%q, %q) = %v", tc.prefix, tc.key, got)
		}
	}
	for _, ok := range []string{"", "public/", "media/public/"} {
		if err := storage.ValidatePublicPrefix(ok); err != nil {
			t.Errorf("ValidatePublicPrefix(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"public", "/public/", "../public/", "a//", "pub./"} {
		if err := storage.ValidatePublicPrefix(bad); !errors.Is(err, storage.ErrInvalidOptions) {
			t.Errorf("ValidatePublicPrefix(%q) = %v", bad, err)
		}
	}
}

func TestURLLifetimeIsBounded(t *testing.T) {
	if err := storage.ValidateURLOptions(storage.SignedURL(storage.MaxURLExpiry)); err != nil {
		t.Fatalf("MaxURLExpiry = %v", err)
	}
	if err := storage.ValidateURLOptions(storage.SignedURL(storage.MaxURLExpiry + time.Nanosecond)); !errors.Is(err, storage.ErrInvalidOptions) {
		t.Fatalf("over MaxURLExpiry = %v", err)
	}
}

// TestSignedURLLifetimeIsWholeSeconds: a signed URL's lifetime is a whole
// number of seconds, from one second to MaxURLExpiry, as S3 counts it; a
// finer one is refused rather than counted differently by each driver.
func TestSignedURLLifetimeIsWholeSeconds(t *testing.T) {
	for ttl, valid := range map[time.Duration]bool{
		time.Second:                             true,
		time.Minute:                             true,
		storage.MaxURLExpiry:                    true,
		time.Nanosecond:                         false,
		time.Millisecond:                        false,
		1500 * time.Millisecond:                 false,
		storage.MaxURLExpiry - time.Millisecond: false,
		storage.MaxURLExpiry + time.Second:      false,
	} {
		err := storage.ValidateURLOptions(storage.SignedURL(ttl))
		if (err == nil) != valid || (err != nil && !errors.Is(err, storage.ErrInvalidOptions)) {
			t.Errorf("SignedURL(%s): %v, want valid = %v", ttl, err, valid)
		}
	}
}
