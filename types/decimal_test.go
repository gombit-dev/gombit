package types_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/gombit-dev/gombit/types"
)

func TestDecimalJSONRoundTripsAsString(t *testing.T) {
	d, err := types.NewDecimalFromString("19.99")
	if err != nil {
		t.Fatalf("NewDecimalFromString: %v", err)
	}
	b, err := json.Marshal(struct {
		Price types.Decimal `json:"price"`
	}{Price: d})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(b), `{"price":"19.99"}`; got != want {
		t.Fatalf("json = %s, want %s (a quoted string, no float rounding)", got, want)
	}

	var out struct {
		Price types.Decimal `json:"price"`
	}
	if err := json.Unmarshal([]byte(`{"price":"19.99"}`), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !out.Price.Equal(d.Decimal) {
		t.Fatalf("round-trip = %s, want 19.99", out.Price.String())
	}
}

func TestDecimalHumaSchemaIsString(t *testing.T) {
	reg := huma.NewMapRegistry("#/components/schemas/", huma.DefaultSchemaNamer)
	s := reg.Schema(reflect.TypeOf(types.Decimal{}), false, "Decimal")
	if s.Type != huma.TypeString {
		t.Fatalf("schema type = %q, want string (so OpenAPI/TS see a string)", s.Type)
	}
	if s.Format != "decimal" {
		t.Fatalf("schema format = %q, want decimal", s.Format)
	}
}

func TestDecimalGORMRoundTrip(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	type product struct {
		ID    uint
		Price types.Decimal `gorm:"type:decimal(19,4);not null"`
	}
	if err := db.AutoMigrate(&product{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	want, _ := types.NewDecimalFromString("1234.5600")
	row := product{Price: want}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	var got product
	if err := db.First(&got, row.ID).Error; err != nil {
		t.Fatalf("first: %v", err)
	}
	if !got.Price.Equal(want.Decimal) {
		t.Fatalf("stored price = %s, want %s", got.Price.String(), want.String())
	}
}

// TestDecimalRefusesUnboundedSizeQuickly: a decimal's exponent is unbounded,
// and formatting "1e1000000000" materialises a billion digits. Every parse
// entry point refuses a value beyond types.MaxDecimalDigits within a fixed budget,
// without formatting it (#440 review).
func TestDecimalRefusesUnboundedSizeQuickly(t *testing.T) {
	within := func(label string, f func() error) {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- f() }()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s: accepted, want a refusal", label)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: still running after 2s; the value is being formatted", label)
		}
	}
	for _, hostile := range []string{"1e1000000000", "-7.5e-999999999", "1e1001", strings.Repeat("9", types.MaxDecimalDigits+1)} {
		within("UnmarshalJSON "+hostile[:min(len(hostile), 20)], func() error {
			var d types.Decimal
			return json.Unmarshal([]byte(`"`+hostile+`"`), &d)
		})
		within("UnmarshalText "+hostile[:min(len(hostile), 20)], func() error {
			var d types.Decimal
			return d.UnmarshalText([]byte(hostile))
		})
		within("NewDecimalFromString "+hostile[:min(len(hostile), 20)], func() error {
			_, err := types.NewDecimalFromString(hostile)
			return err
		})
	}
	// Large but bounded values still parse.
	for _, ok := range []string{"1e999", strings.Repeat("9", types.MaxDecimalDigits), "0.5", "-12.25"} {
		var d types.Decimal
		if err := json.Unmarshal([]byte(`"`+ok+`"`), &d); err != nil {
			t.Errorf("UnmarshalJSON %.20s: %v, want accepted", ok, err)
		}
	}
}
