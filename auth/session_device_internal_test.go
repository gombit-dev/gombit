package auth

import (
	"net/netip"
	"strings"
	"sync"
	"testing"

	"gorm.io/gorm/schema"

	"github.com/gombit-dev/gombit/config"
)

// TestSessionDeviceSizesAreTheColumns: what storableText cuts the user
// agent and the location to is the size of their refresh_tokens columns,
// and the client_address column holds the longest address
// sessionClientAddress writes. A tag widened or narrowed without the
// constant fails here, before a long value fails a login.
func TestSessionDeviceSizesAreTheColumns(t *testing.T) {
	s, err := schema.Parse(&RefreshToken{}, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatal(err)
	}
	size := func(field string) int {
		t.Helper()
		f := s.LookUpField(field)
		if f == nil {
			t.Fatalf("RefreshToken has no field %s", field)
		}
		return f.Size
	}
	if got := size("UserAgent"); got != sessionUserAgentSize {
		t.Errorf("user_agent column is %d bytes, storableText cuts to %d", got, sessionUserAgentSize)
	}
	if got := size("Location"); got != sessionLocationSize {
		t.Errorf("location column is %d bytes, storableText cuts to %d", got, sessionLocationSize)
	}
	longest := netip.MustParseAddr("ffff:ffff:ffff:ffff:ffff:ffff:ffff:fffe")
	for _, mode := range []config.SessionClientAddress{config.SessionClientAddressFull, config.SessionClientAddressCoarse} {
		if got, column := sessionClientAddress(longest, mode), size("ClientAddress"); len(got) > column {
			t.Errorf("%s address %q is %d bytes, the client_address column %d", mode, got, len(got), column)
		}
	}
}

// TestStorableText: a label keeps its text and loses what does not belong
// in one line of it: control characters of every range, line and paragraph
// separators, bidirectional formatting characters; invalid UTF-8 becomes
// U+FFFD before the cut, so the cut still fits.
func TestStorableText(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Firefox/133.0 (X11; Linux)", "Firefox/133.0 (X11; Linux)"},
		{"a\x00b\tc\x1bd\x7fe", "abcde"},                             // C0 and DEL
		{"a\u0085b\u009bc", "abc"},                                   // C1: NEL, CSI
		{"a\u2028b\u2029c", "abc"},                                   // line and paragraph separators
		{"a\u202eb\u2066c\u200fd", "abcd"},                           // bidi override, isolate, mark
		{"  caf\u00e9  ", "caf\u00e9"},                               // trimmed, letters kept
		{"a\xffb", "a\ufffdb"},                                       // invalid UTF-8 replaced
		{"\U0001F469\u200d\U0001F4BB", "\U0001F469\u200d\U0001F4BB"}, // a joiner is no control
	}
	for _, tt := range tests {
		if got := storableText(tt.in, 255); got != tt.want {
			t.Errorf("storableText(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	if got := storableText(strings.Repeat("\xff", 100), 255); len(got) > 255 || !strings.HasPrefix(got, "\ufffd") {
		t.Errorf("100 invalid bytes became %d bytes, want U+FFFDs cut to at most 255", len(got))
	}
}
