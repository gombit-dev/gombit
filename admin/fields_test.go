package admin

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/field"
	"github.com/gombit-dev/gombit/types"
)

type setterRow struct {
	Note    *string
	Name    string
	Payload json.RawMessage
	When    time.Time
	WhenPtr *time.Time
}

func TestConvertToNilMatchesDest(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		dest reflect.Type
		want func(reflect.Value) bool
	}{
		{
			name: "string",
			dest: reflect.TypeOf(""),
			want: func(v reflect.Value) bool { return v.Kind() == reflect.String && v.String() == "" },
		},
		{
			name: "pointer string",
			dest: reflect.TypeOf((*string)(nil)),
			want: func(v reflect.Value) bool { return v.Kind() == reflect.Pointer && v.IsNil() },
		},
		{
			name: "time.Time",
			dest: reflect.TypeOf(time.Time{}),
			want: func(v reflect.Value) bool {
				got, ok := v.Interface().(time.Time)
				return ok && got.IsZero()
			},
		},
		{
			name: "pointer time.Time",
			dest: reflect.TypeOf((*time.Time)(nil)),
			want: func(v reflect.Value) bool { return v.Kind() == reflect.Pointer && v.IsNil() },
		},
		{
			name: "json.RawMessage",
			dest: reflect.TypeOf(json.RawMessage{}),
			want: func(v reflect.Value) bool {
				got, ok := v.Interface().(json.RawMessage)
				return ok && got == nil
			},
		},
		{
			name: "[]byte",
			dest: reflect.TypeOf([]byte(nil)),
			want: func(v reflect.Value) bool {
				got, ok := v.Interface().([]byte)
				return ok && got == nil
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := convertTo(nil, tc.dest)
			if err != nil {
				t.Fatalf("convertTo(nil, %s) error = %v", tc.dest, err)
			}
			if !tc.want(got) {
				t.Fatalf("convertTo(nil, %s) = %#v, want dest zero/nil", tc.dest, got.Interface())
			}
		})
	}
}

func TestMakeSetterClearsPresentNil(t *testing.T) {
	t.Parallel()
	note := "hello"
	when := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	inst := &setterRow{
		Note:    &note,
		Name:    "keep",
		Payload: json.RawMessage(`{"a":1}`),
		When:    when,
		WhenPtr: &when,
	}
	rt := reflect.TypeOf(setterRow{})

	clear := func(name string, ft FieldType, raw any) {
		t.Helper()
		sf, ok := rt.FieldByName(name)
		if !ok {
			t.Fatalf("field %s missing", name)
		}
		set := makeSetter(sf.Index, ft, sf.Type)
		if err := set(inst, raw); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}

	clear("Note", TypeText, nil)
	clear("Payload", TypeJSON, nil)
	clear("When", TypeDateTime, nil)
	clear("WhenPtr", TypeDateTime, nil)

	if inst.Note != nil {
		t.Fatalf("Note = %#v, want nil pointer", inst.Note)
	}
	if inst.Name != "keep" {
		t.Fatalf("Name = %q, want %q (omitted key must stay unchanged)", inst.Name, "keep")
	}
	if inst.Payload != nil {
		t.Fatalf("Payload = %#v, want nil", inst.Payload)
	}
	if !inst.When.IsZero() {
		t.Fatalf("When = %s, want zero time", inst.When)
	}
	if inst.WhenPtr != nil {
		t.Fatalf("WhenPtr = %#v, want nil pointer", inst.WhenPtr)
	}

	clear("Name", TypeString, "")
	if inst.Name != "" {
		t.Fatalf("Name after empty string = %q, want empty", inst.Name)
	}
	clear("Name", TypeString, nil)
	if inst.Name != "" {
		t.Fatalf("Name after null = %q, want empty", inst.Name)
	}
}

func TestConvertToJSONAndUUID(t *testing.T) {
	t.Parallel()

	t.Run("map into json.RawMessage", func(t *testing.T) {
		got, err := convertTo(map[string]any{"a": float64(1)}, reflect.TypeOf(json.RawMessage{}))
		if err != nil {
			t.Fatalf("convertTo: %v", err)
		}
		raw, ok := got.Interface().(json.RawMessage)
		if !ok {
			t.Fatalf("type %T, want json.RawMessage", got.Interface())
		}
		if string(raw) != `{"a":1}` {
			t.Fatalf("RawMessage = %s, want {\"a\":1}", raw)
		}
	})

	t.Run("map into []byte", func(t *testing.T) {
		got, err := convertTo(map[string]any{"a": float64(1)}, reflect.TypeOf([]byte(nil)))
		if err != nil {
			t.Fatalf("convertTo: %v", err)
		}
		b, ok := got.Interface().([]byte)
		if !ok {
			t.Fatalf("type %T, want []byte", got.Interface())
		}
		if string(b) != `{"a":1}` {
			t.Fatalf("[]byte = %s, want {\"a\":1}", b)
		}
	})

	t.Run("map into nested struct", func(t *testing.T) {
		type nested struct {
			A int `json:"a"`
		}
		got, err := convertTo(map[string]any{"a": float64(7)}, reflect.TypeOf(nested{}))
		if err != nil {
			t.Fatalf("convertTo: %v", err)
		}
		n, ok := got.Interface().(nested)
		if !ok {
			t.Fatalf("type %T, want nested", got.Interface())
		}
		if n.A != 7 {
			t.Fatalf("nested.A = %d, want 7", n.A)
		}
	})

	t.Run("string into uuid.UUID", func(t *testing.T) {
		const s = "550e8400-e29b-41d4-a716-446655440000"
		got, err := convertTo(s, reflect.TypeOf(uuid.UUID{}))
		if err != nil {
			t.Fatalf("convertTo: %v", err)
		}
		id, ok := got.Interface().(uuid.UUID)
		if !ok {
			t.Fatalf("type %T, want uuid.UUID", got.Interface())
		}
		if id.String() != s {
			t.Fatalf("uuid = %s, want %s", id, s)
		}
	})
}

func TestMakeSetterTypesJSONRejectsNonDocuments(t *testing.T) {
	t.Parallel()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		ID   uint
		Note types.JSON `gorm:"type:text"`
	}
	if err := db.AutoMigrate(&row{}); err != nil {
		t.Fatal(err)
	}
	sf, ok := reflect.TypeOf(row{}).FieldByName("Note")
	if !ok {
		t.Fatal("Note missing")
	}
	set := makeSetter(sf.Index, TypeJSON, sf.Type)
	inst := &row{}
	for _, raw := range []any{"hello", float64(42), `{"a":1}`, `[1]`, "null", true} {
		if err := set(inst, raw); err == nil {
			t.Fatalf("setter stored %#v", raw)
		}
	}
	type optional struct {
		Note types.NullJSON
	}
	optField, ok := reflect.TypeOf(optional{}).FieldByName("Note")
	if !ok {
		t.Fatal("Note missing")
	}
	opt := &optional{Note: types.NullJSON(`{"keep":1}`)}
	if err := makeSetter(optField.Index, TypeJSON, optField.Type)(opt, "null"); err == nil {
		t.Fatal("setter cleared NullJSON from the string null")
	}
	if string(opt.Note) != `{"keep":1}` {
		t.Fatalf("note = %s, want the previous document", opt.Note)
	}
	if err := set(inst, map[string]any{"a": float64(1)}); err != nil {
		t.Fatalf("set object: %v", err)
	}
	if err := db.Create(inst).Error; err != nil {
		t.Fatal(err)
	}
	var got row
	if err := db.First(&got, inst.ID).Error; err != nil {
		t.Fatal(err)
	}
	if string(got.Note) != `{"a":1}` {
		t.Fatalf("note = %s", got.Note)
	}
	list := &row{}
	if err := set(list, []any{float64(1), "x"}); err != nil {
		t.Fatalf("set array: %v", err)
	}
	if err := db.Create(list).Error; err != nil {
		t.Fatal(err)
	}
	var gotList row
	if err := db.First(&gotList, list.ID).Error; err != nil {
		t.Fatal(err)
	}
	if string(gotList.Note) != `[1,"x"]` {
		t.Fatalf("note = %s", gotList.Note)
	}
}

func TestMakeSetterJSONObjectAndUUID(t *testing.T) {
	t.Parallel()
	type row struct {
		Payload json.RawMessage
		Token   uuid.UUID
	}
	inst := &row{}
	rt := reflect.TypeOf(row{})

	payloadField, ok := rt.FieldByName("Payload")
	if !ok {
		t.Fatal("Payload missing")
	}
	if err := makeSetter(payloadField.Index, TypeJSON, payloadField.Type)(inst, map[string]any{"a": float64(1)}); err != nil {
		t.Fatalf("set Payload: %v", err)
	}
	if string(inst.Payload) != `{"a":1}` {
		t.Fatalf("Payload = %s, want {\"a\":1}", inst.Payload)
	}

	tokenField, ok := rt.FieldByName("Token")
	if !ok {
		t.Fatal("Token missing")
	}
	const s = "550e8400-e29b-41d4-a716-446655440000"
	if err := makeSetter(tokenField.Index, TypeUUID, tokenField.Type)(inst, s); err != nil {
		t.Fatalf("set Token: %v", err)
	}
	if inst.Token.String() != s {
		t.Fatalf("Token = %s, want %s", inst.Token, s)
	}
}

// TestDecimalFieldInferenceAndCoercion covers #222: decimal columns (both the
// bare shopspring type and the framework types.Decimal wrapper) infer as
// TypeDecimal, and admin write coercion keeps the exact textual value instead of
// routing money through float64.
func TestDecimalFieldInferenceAndCoercion(t *testing.T) {
	type priced struct {
		ID    uint            `json:"id"`
		Price decimal.Decimal `json:"price" gorm:"type:decimal(19,4)"`
		Fee   types.Decimal   `json:"fee" gorm:"type:decimal(19,4)"`
	}
	fields, err := FieldsFrom(priced{})
	if err != nil {
		t.Fatalf("FieldsFrom: %v", err)
	}
	byName := map[string]Field{}
	for _, f := range fields {
		byName[f.Name] = f
	}
	if byName["price"].Type != TypeDecimal {
		t.Fatalf("price type = %q, want decimal", byName["price"].Type)
	}
	if byName["fee"].Type != TypeDecimal {
		t.Fatalf("fee type = %q, want decimal", byName["fee"].Type)
	}

	// A high-precision value must survive coercion exactly (no float rounding).
	got, err := coerceValue("19.9999", TypeDecimal)
	if err != nil {
		t.Fatalf("coerceValue: %v", err)
	}
	if got != "19.9999" {
		t.Fatalf("coerceValue = %#v, want exact string 19.9999", got)
	}
	if _, err := coerceValue("not-a-number", TypeDecimal); err == nil {
		t.Fatal("coerceValue(non-numeric) error = nil, want error")
	}
	// A JSON body decodes numbers to float64, which cannot represent a decimal
	// exactly; decimals must arrive as strings. A float64 must be rejected, not
	// silently rounded.
	if _, err := coerceValue(float64(19.99), TypeDecimal); err == nil {
		t.Fatal("coerceValue(float64) error = nil, want rejection (decimals must be strings)")
	}
	// A json.Number keeps its exact literal.
	num, err := coerceValue(json.Number("19.9999"), TypeDecimal)
	if err != nil || num != "19.9999" {
		t.Fatalf("coerceValue(json.Number) = %#v, %v; want 19.9999", num, err)
	}
}

func TestDetectVersionFieldSkipsPointer(t *testing.T) {
	type intVersion struct {
		ID      uint `gorm:"primaryKey"`
		Version int
	}
	type ptrVersion struct {
		ID      uint `gorm:"primaryKey"`
		Version *int
	}
	type strVersion struct {
		ID      uint `gorm:"primaryKey"`
		Version string
	}

	schInt, err := parseSchema(intVersion{})
	if err != nil {
		t.Fatalf("parseSchema int: %v", err)
	}
	if detectVersionField(schInt) == nil {
		t.Fatal("int version column should enable optimistic locking")
	}

	schPtr, err := parseSchema(ptrVersion{})
	if err != nil {
		t.Fatalf("parseSchema ptr: %v", err)
	}
	if detectVersionField(schPtr) != nil {
		t.Fatal("pointer version column must be ignored (get/set no-op on pointers)")
	}

	schStr, err := parseSchema(strVersion{})
	if err != nil {
		t.Fatalf("parseSchema str: %v", err)
	}
	if detectVersionField(schStr) != nil {
		t.Fatal("non-integer version column must be ignored")
	}
}

func TestAdminWireSetMatchesVocabulary(t *testing.T) {
	t.Parallel()
	wires := field.AdminWires()
	if len(wires) == 0 {
		t.Fatal("vocabulary has no admin wires")
	}
	seen := map[string]struct{}{}
	for _, w := range wires {
		if !validFieldType(FieldType(w)) {
			t.Fatalf("admin rejects vocabulary wire %q", w)
		}
		seen[w] = struct{}{}
	}
	for _, declared := range []FieldType{
		TypeString, TypeText, TypeInteger, TypeFloat, TypeDecimal, TypeBoolean,
		TypeDateTime, TypeDate, TypeUUID, TypeJSON, TypeRelation,
	} {
		if _, ok := seen[string(declared)]; !ok {
			t.Fatalf("admin type %q is not in the vocabulary admin wires", declared)
		}
		delete(seen, string(declared))
	}
	if len(seen) != 0 {
		t.Fatalf("vocabulary admin wires with no admin constant: %v", seen)
	}
}

func TestFieldsFromFollowsResourcePolicy(t *testing.T) {
	type book struct {
		gorm.Model
		Title    string `gorm:"not null"`
		Secret   string `gombit:"-"`
		TenantID uint   `gorm:"not null" gombit:"read,server"`
		Password string `gombit:"write"`
		Status   string `gorm:"size:32" gombit:"read,write,filterable,sortable,searchable"`
	}
	fields, err := FieldsFrom(&book{})
	if err != nil {
		t.Fatalf("FieldsFrom: %v", err)
	}
	byName := map[string]Field{}
	for _, f := range fields {
		byName[f.Name] = f
		if f.Name == "secret" {
			t.Fatal("hidden column was derived")
		}
	}
	title := byName["title"]
	if title.ReadOnly || !title.Required || title.WriteOnly {
		t.Fatalf("title = %+v", title)
	}
	tenant := byName["tenant_id"]
	if !tenant.ReadOnly || !tenant.ServerRequired || tenant.Required {
		t.Fatalf("tenant_id = %+v", tenant)
	}
	password := byName["password"]
	if password.ReadOnly || !password.WriteOnly {
		t.Fatalf("password = %+v", password)
	}
	if _, ok := byName["deleted_at"]; ok {
		t.Fatal("soft-delete column was derived")
	}

	m := &registered{fields: []resolvedField{{Field: tenant}}}
	err = applyWrite(context.Background(), m, &book{}, nil, true)
	var env *contract.ErrorEnvelope
	if !errors.As(err, &env) || len(env.Body.Fields["tenant_id"]) == 0 || !strings.Contains(env.Body.Fields["tenant_id"][0], "set by the server") {
		t.Fatalf("create error = %#v", err)
	}

	row := (&registered{fields: []resolvedField{
		{Field: title, get: func(any) any { return "Ada" }},
		{Field: password, get: func(any) any { return "secret" }},
	}}).toRow(&book{})
	if _, ok := row["password"]; ok {
		t.Fatalf("write-only value was returned: %#v", row)
	}
	if row["title"] != "Ada" {
		t.Fatalf("row = %#v", row)
	}
}

func TestFieldsFromRejectsUnsatisfiablePolicy(t *testing.T) {
	type bad struct {
		gorm.Model
		Name string `gorm:"not null" gombit:"read"`
	}
	if _, err := FieldsFrom(&bad{}); err == nil {
		t.Fatal("read-only NOT NULL column was accepted")
	}
}
