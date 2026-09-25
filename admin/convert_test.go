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
	if got := constraintMessage(Field{Pattern: `^[-a-zA-Z0-9_]+$`}, "ada_lovelace"); got != "" {
		t.Fatalf("slug: %q", got)
	}
	if got := constraintMessage(Field{Pattern: `^[-a-zA-Z0-9_]+$`}, "has space"); got == "" {
		t.Fatal("bad slug accepted")
	}
}
