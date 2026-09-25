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
