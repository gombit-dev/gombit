package admin

import (
	"testing"

	"github.com/gombit-dev/gombit/types"
)

func TestEnumLabelsReachAdminMeta(t *testing.T) {
	type row struct {
		ID     int
		Status string          `json:"status" validate:"enum=draft,published;label=Draft,Published"`
		Opens  types.TimeOfDay `json:"opens" format:"time"`
		Length types.Duration  `json:"length" format:"duration"`
	}
	sch, err := parseSchema(row{})
	if err != nil {
		t.Fatal(err)
	}
	fields := []Field{
		{Name: "status", Type: TypeString},
		{Name: "opens", Type: inferFieldType(matchSchemaField(sch, Field{Name: "opens"}))},
		{Name: "length", Type: inferFieldType(matchSchemaField(sch, Field{Name: "length"}))},
	}
	if err := fillConstraints(fields, sch); err != nil {
		t.Fatal(err)
	}
	if len(fields[0].Choices) != 2 || fields[0].Choices[0].Value != "draft" || fields[0].Choices[0].Label != "Draft" || fields[0].Choices[1].Label != "Published" {
		t.Fatalf("choices = %+v", fields[0].Choices)
	}
	if fields[1].Type != TypeTime || fields[1].Format != "time" {
		t.Fatalf("opens = %+v", fields[1])
	}
	if fields[2].Type != TypeDuration || fields[2].Format != "duration" {
		t.Fatalf("length = %+v", fields[2])
	}
	meta := modelMetaFrom(Options{Fields: fields, Slug: "shift", Singular: "Shift", Plural: "Shifts"}, "id")
	if meta.Fields[0].Choices[1].Label != "Published" {
		t.Fatalf("meta choices = %+v", meta.Fields[0].Choices)
	}
	if msg := constraintMessage(fields[0], "nope"); msg != "must be one of Draft, Published" {
		t.Fatalf("constraint = %q", msg)
	}
	if msg := constraintMessage(fields[0], "draft"); msg != "" {
		t.Fatalf("stored value rejected: %q", msg)
	}
	clock, err := coerceValue("09:05", TypeTime)
	if err != nil || clock != "09:05:00" {
		t.Fatalf("clock = %#v, err %v", clock, err)
	}
	span, err := coerceValue("45m", TypeDuration)
	if err != nil || span != "45m0s" {
		t.Fatalf("span = %#v, err %v", span, err)
	}
}
