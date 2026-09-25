package field

import "testing"

func TestConstraintsRoundTrip(t *testing.T) {
	t.Parallel()
	in := Constraints{Min: "0", Max: "150", MaxLength: 40, Pattern: "^[a-z]+$", Default: "draft"}
	got, err := ParseConstraints(FormatConstraints(in))
	if err != nil {
		t.Fatal(err)
	}
	if got != in {
		t.Fatalf("round trip = %+v, want %+v", got, in)
	}
}

func TestParseConstraintsRejectsUnknownKey(t *testing.T) {
	t.Parallel()
	if _, err := ParseConstraints("nope=1"); err == nil {
		t.Fatal("unknown key was accepted")
	}
}
