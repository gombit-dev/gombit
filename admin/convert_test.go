package admin

import "testing"

func TestConstraintMessageCountsRunes(t *testing.T) {
	f := Field{MaxLength: 1}
	if got := constraintMessage(f, "é"); got != "" {
		t.Fatalf("é: %q, want empty", got)
	}
	if got := constraintMessage(f, "👍"); got != "" {
		t.Fatalf("thumbs up: %q, want empty", got)
	}
	if got := constraintMessage(f, "éé"); got != "is too long" {
		t.Fatalf("éé: %q", got)
	}
}

func TestFormatMessageMatchesHuma(t *testing.T) {
	if got := constraintMessage(Field{Format: "email"}, ""); got == "" {
		t.Fatal("blank email must fail")
	}
	if got := constraintMessage(Field{Format: "email"}, "ada@example.com"); got != "" {
		t.Fatalf("email: %q", got)
	}
	if got := constraintMessage(Field{Format: "email"}, "not-an-email"); got == "" {
		t.Fatal("bad email accepted")
	}
	if got := constraintMessage(Field{Format: "uri"}, "https://example.com"); got != "" {
		t.Fatalf("uri: %q", got)
	}
	if got := constraintMessage(Field{Format: "uri"}, "example.com"); got == "" {
		t.Fatal("relative uri accepted")
	}
	if got := constraintMessage(Field{Format: "ip"}, "127.0.0.1"); got != "" {
		t.Fatalf("ip: %q", got)
	}
	if got := constraintMessage(Field{Format: "ip"}, "nope"); got == "" {
		t.Fatal("bad ip accepted")
	}
	if got := constraintMessage(Field{Format: "uri-reference"}, ""); got != "" {
		t.Fatalf("uri-reference blank: %q", got)
	}
	if got := constraintMessage(Field{Pattern: `^[-a-zA-Z0-9_]+$`}, "ada_lovelace"); got != "" {
		t.Fatalf("slug: %q", got)
	}
	if got := constraintMessage(Field{Pattern: `^[-a-zA-Z0-9_]+$`}, "has space"); got == "" {
		t.Fatal("bad slug accepted")
	}
}

func TestCoercePathIDPreservesStringWhitespace(t *testing.T) {
	for _, tc := range []struct {
		id   string
		ft   FieldType
		want any
	}{
		{" sku", TypeString, " sku"},
		{"sku ", TypeString, "sku "},
		{" sku ", TypeText, " sku "},
		{"   ", TypeString, "   "},
		{" 42 ", TypeInteger, int64(42)},
		{" 550e8400-e29b-41d4-a716-446655440000 ", TypeUUID, "550e8400-e29b-41d4-a716-446655440000"},
		{" 550E8400-E29B-41D4-A716-446655440000 ", typeCanonicalUUID, "550e8400-e29b-41d4-a716-446655440000"},
	} {
		got, err := coercePathID(tc.id, tc.ft)
		if err != nil {
			t.Fatalf("coercePathID(%q, %q) returned unexpected error: %v", tc.id, tc.ft, err)
		}
		if got != tc.want {
			t.Errorf("coercePathID(%q, %q) = %#v, want %#v", tc.id, tc.ft, got, tc.want)
		}
	}

	for _, tc := range []struct {
		id string
		ft FieldType
	}{
		{"", TypeString},
		{"", TypeText},
		{"", TypeInteger},
		{"   ", TypeInteger},
		{"   ", TypeUUID},
		{"   ", typeCanonicalUUID},
	} {
		if _, err := coercePathID(tc.id, tc.ft); err == nil {
			t.Errorf("coercePathID(%q, %q) expected error for empty id, got nil", tc.id, tc.ft)
		}
	}
}

func TestCoerceFilterPreservesStringWhitespace(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		ft   FieldType
		want any
	}{
		{" sku", TypeString, " sku"},
		{"sku ", TypeString, "sku "},
		{" sku ", TypeText, " sku "},
		{" 42 ", TypeInteger, int64(42)},
		{"   ", TypeInteger, nil},
		{"", TypeString, nil},
		{" 550e8400-e29b-41d4-a716-446655440000 ", TypeUUID, "550e8400-e29b-41d4-a716-446655440000"},
		{" 550E8400-E29B-41D4-A716-446655440000 ", typeCanonicalUUID, "550e8400-e29b-41d4-a716-446655440000"},
		{"   ", TypeUUID, nil},
		{"   ", typeCanonicalUUID, nil},
	} {
		got, err := coerceFilter(tc.raw, tc.ft)
		if err != nil {
			t.Fatalf("coerceFilter(%q, %q) returned unexpected error: %v", tc.raw, tc.ft, err)
		}
		if got != tc.want {
			t.Errorf("coerceFilter(%q, %q) = %#v, want %#v", tc.raw, tc.ft, got, tc.want)
		}
	}
}
