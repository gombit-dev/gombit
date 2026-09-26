package field

import (
	"strings"
	"testing"
)

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
	if strings.Contains(FormatConstraints(in), "label=") {
		t.Fatal("labels that match the stored values should stay off the tag")
	}
	labeled := Constraints{Enum: []string{"draft", "published"}, Label: []string{"Draft", "Published"}}
	text := FormatConstraints(labeled)
	if !strings.Contains(text, "enum=draft,published") || !strings.Contains(text, "label=Draft,Published") {
		t.Fatalf("labeled tag = %s", text)
	}
	back, err := ParseConstraints(text)
	if err != nil || len(back.Label) != 2 || back.Label[0] != "Draft" || back.Label[1] != "Published" {
		t.Fatalf("labeled parse = %+v, err %v", back, err)
	}
	if _, err := ParseConstraints("enum=draft;label=Draft,Published"); err == nil {
		t.Fatal("mismatched label count was accepted")
	}
}

func TestParseConstraintsRejectsUnknownKey(t *testing.T) {
	t.Parallel()
	if _, err := ParseConstraints("nope=1"); err == nil {
		t.Fatal("unknown key was accepted")
	}
}
