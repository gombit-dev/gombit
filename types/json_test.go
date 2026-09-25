package types

import (
	"reflect"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestJSONSchemaRejectsNullUnlessOptional(t *testing.T) {
	reg := huma.NewMapRegistry("#/components/schemas/", huma.DefaultSchemaNamer)
	type wire struct {
		Meta JSON     `json:"meta"`
		Note NullJSON `json:"note"`
	}
	s := reg.Schema(reflect.TypeOf(wire{}), false, "Wire")
	res := huma.ValidateResult{}
	huma.Validate(reg, s, &huma.PathBuffer{}, huma.ModeWriteToServer, map[string]any{
		"meta": map[string]any{"a": 1},
		"note": nil,
	}, &res)
	if len(res.Errors) != 0 {
		t.Fatalf("object plus null optional: %v", res.Errors)
	}
	res = huma.ValidateResult{}
	huma.Validate(reg, s, &huma.PathBuffer{}, huma.ModeWriteToServer, map[string]any{
		"meta": nil,
		"note": []any{1, "x"},
	}, &res)
	if len(res.Errors) == 0 {
		t.Fatal("required JSON accepted null")
	}
	res = huma.ValidateResult{}
	huma.Validate(reg, s, &huma.PathBuffer{}, huma.ModeWriteToServer, map[string]any{
		"meta": "nope",
		"note": []any{},
	}, &res)
	if len(res.Errors) == 0 {
		t.Fatal("required JSON accepted a string")
	}
}

func TestNullJSONRoundTrip(t *testing.T) {
	t.Parallel()
	var n NullJSON
	if err := n.UnmarshalJSON([]byte("null")); err != nil {
		t.Fatal(err)
	}
	if n != nil {
		t.Fatalf("null = %#v", n)
	}
	b, err := n.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "null" {
		t.Fatalf("marshal nil = %s", b)
	}
	var required JSON
	if err := required.UnmarshalJSON([]byte("null")); err == nil {
		t.Fatal("required JSON accepted null")
	}
	if err := required.UnmarshalJSON([]byte(`"x"`)); err == nil {
		t.Fatal("JSON accepted a string")
	}
	if err := n.UnmarshalJSON([]byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	b, err = n.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"a":1}` {
		t.Fatalf("marshal = %s", b)
	}
}

func TestNullJSONGORMRoundTrip(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		ID   uint
		Note NullJSON `gorm:"type:text"`
	}
	if err := db.AutoMigrate(&row{}); err != nil {
		t.Fatal(err)
	}
	stored := row{Note: NullJSON(`{"a":1}`)}
	if err := db.Create(&stored).Error; err != nil {
		t.Fatal(err)
	}
	var got row
	if err := db.First(&got, stored.ID).Error; err != nil {
		t.Fatal(err)
	}
	if string(got.Note) != `{"a":1}` {
		t.Fatalf("note = %s", got.Note)
	}
	blank := row{}
	if err := db.Create(&blank).Error; err != nil {
		t.Fatal(err)
	}
	var empty row
	if err := db.First(&empty, blank.ID).Error; err != nil {
		t.Fatal(err)
	}
	if empty.Note != nil {
		t.Fatalf("null note read back as %s", empty.Note)
	}
}
