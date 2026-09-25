package field

import "testing"

func TestConstraintsRoundTrip(t *testing.T) {
	t.Parallel()
	in := Constraints{Min: "0", Max: "150", MaxLength: 40, Pattern: "^[a-z]+$", Default: "draft", Enum: []string{"draft", "published"}}
	got, err := ParseConstraints(FormatConstraints(in))
	if err != nil {
		t.Fatal(err)
	}
	if got.Min != in.Min || got.Max != in.Max || got.MaxLength != in.MaxLength || got.Pattern != in.Pattern || got.Default != in.Default || len(got.Enum) != 2 || got.Enum[0] != "draft" || got.Enum[1] != "published" {
		t.Fatalf("round trip = %+v, want %+v", got, in)
	}
}

func TestParseConstraintsRejectsUnknownKey(t *testing.T) {
	t.Parallel()
	if _, err := ParseConstraints("nope=1"); err == nil {
		t.Fatal("unknown key was accepted")
	}
}
