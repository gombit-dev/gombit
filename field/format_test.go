package field

import "testing"

func TestFormatRejectsEmptyTheWayHumaDoes(t *testing.T) {
	t.Parallel()
	reject := []string{
		"date-time", "date-time-http", "date", "duration", "time",
		"email", "idn-email", "hostname", "idn-hostname",
		"ipv4", "ipv6", "ip", "uri", "iri", "uuid", "relative-json-pointer",
	}
	for _, format := range reject {
		if !FormatRejects(format, "") {
			t.Fatalf("format %q must reject empty", format)
		}
	}
	accept := []string{"", "uri-reference", "iri-reference", "uri-template", "json-pointer", "regex", "not-a-format"}
	for _, format := range accept {
		if FormatRejects(format, "") {
			t.Fatalf("format %q must accept empty", format)
		}
	}
	if FormatRejects("hostname", "example.com") {
		t.Fatal("hostname example.com rejected")
	}
	if !FormatRejects("ipv4", "2001:db8::1") {
		t.Fatal("ipv6 accepted as ipv4")
	}
	if FormatRejects("uri", "https://example.com") {
		t.Fatal("absolute uri rejected")
	}
	if !FormatRejects("uri", "example.com") {
		t.Fatal("relative uri accepted")
	}
}
