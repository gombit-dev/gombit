package types_test

import (
	"encoding/json"
	"math"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/shopspring/decimal"
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
	// A zero coefficient is no cheaper: formatting rescales to the exponent.
	for _, hostile := range []string{"1e1000000000", "-7.5e-999999999", "1e1001", strings.Repeat("9", types.MaxDecimalDigits+1),
		"0e1000000000", "-0e999999999", "0e-1000000000"} {
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

// TestDecimalUnmarshalNullKeepsTheValue: a JSON null leaves the decimal as it
// was, the encoding/json convention shopspring's decoder follows, so decoding a
// partial body onto a loaded row does not zero it.
func TestDecimalUnmarshalNullKeepsTheValue(t *testing.T) {
	var row struct{ P types.Decimal }
	row.P = types.MustDecimal("12.5")
	if err := json.Unmarshal([]byte(`{"P":null}`), &row); err != nil {
		t.Fatal(err)
	}
	if row.P.String() != "12.5" {
		t.Fatalf("P after null = %s, want 12.5 kept", row.P)
	}
}

// TestDecimalScanIsBounded: a value stored before the bound existed (SQLite
// kept the text "1e1000000000" as written) fails to load instead of hanging the
// first formatting of it.
func TestDecimalScanIsBounded(t *testing.T) {
	var d types.Decimal
	if err := d.Scan("1e1000000000"); err == nil {
		t.Error("Scan(1e1000000000) accepted, want a refusal")
	}
	if err := d.Scan("0e-1000000000"); err == nil {
		t.Error("Scan(0e-1000000000) accepted, want a refusal")
	}
	if err := d.Scan("12.5"); err != nil || d.String() != "12.5" {
		t.Errorf("Scan(12.5) = %s, %v, want 12.5", d, err)
	}
}

// TestDecimalShapeMatchesString: DecimalShapeOf is the one size rule, so its
// digit count must be exactly what String writes, for every coefficient and
// exponent within the bound (round-4 review: the write guard and Scan counted
// differently, so a value one accepted the other refused).
func TestDecimalShapeMatchesString(t *testing.T) {
	coefficients := []string{"1", "5", "10", "1500", "123456789", "18446744073709551615", "18446744073709551616", "1000000000000000000000000000", "99999999999999999999999999999999999"}
	for _, c := range coefficients {
		for _, exponent := range []int32{-1000, -999, -500, -30, -4, -1, 0, 1, 4, 30, 500, 965, 999, 1000} {
			for _, sign := range []string{"", "-"} {
				d := decimal.NewFromBigInt(mustBig(t, sign+c), exponent)
				shape, err := types.DecimalShapeOf(d)
				written := strings.NewReplacer("-", "", ".", "").Replace(d.String())
				fits := len(written) <= types.MaxDecimalDigits
				if (err == nil) != fits {
					t.Fatalf("%se%d: DecimalShapeOf err=%v, but String writes %d digits", sign+c, exponent, err, len(written))
				}
				if err == nil && shape.Digits() != len(written) {
					t.Fatalf("%se%d: Digits()=%d, String writes %d digits", sign+c, exponent, shape.Digits(), len(written))
				}
			}
		}
	}
	// The boundary, both ways, and a value a database padded to its scale.
	if _, err := types.DecimalShapeOf(decimal.New(1, -999)); err != nil {
		t.Errorf("1e-999 (1000 digits written) refused: %v", err)
	}
	if _, err := types.DecimalShapeOf(decimal.New(1, -1000)); err == nil {
		t.Error("1e-1000 (1001 digits written) accepted")
	}
	padded := "0.05" + strings.Repeat("0", 998) // numeric(1000,1000) as PostgreSQL returns it
	var d types.Decimal
	if err := d.Scan(padded); err != nil || d.String() != "0.05" {
		t.Errorf("Scan(0.05 padded to 1000 places) = %s, %v, want 0.05", d, err)
	}
}

func mustBig(t *testing.T, s string) *big.Int {
	t.Helper()
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("bad big int %q", s)
	}
	return n
}

// TestDecimalScanRefusesNonFiniteFloats: SQLite stores the literal
// "1e1000000000" in a decimal column as REAL Inf, and shopspring panics on it
// (round-4 review); Scan refuses it instead.
func TestDecimalScanRefusesNonFiniteFloats(t *testing.T) {
	for _, v := range []any{math.Inf(1), math.Inf(-1), math.NaN(), float32(math.Inf(1))} {
		var d types.Decimal
		if err := d.Scan(v); err == nil {
			t.Errorf("Scan(%v) accepted, want a refusal", v)
		}
	}
	var d types.Decimal
	if err := d.Scan(12.5); err != nil || d.String() != "12.5" {
		t.Errorf("Scan(12.5) = %s, %v", d, err)
	}
}

// TestDecimalRefusesLongSpellingBeforeParsing: parsing a long coefficient is
// quadratic (a million digits take about a second), so a spelling no bounded
// value could have is refused on its length first.
func TestDecimalRefusesLongSpellingBeforeParsing(t *testing.T) {
	long := strings.Repeat("9", 1_000_000)
	start := time.Now()
	var d types.Decimal
	if err := json.Unmarshal([]byte(`"`+long+`"`), &d); err == nil {
		t.Error("UnmarshalJSON accepted a million digits")
	}
	if err := d.UnmarshalText([]byte(long)); err == nil {
		t.Error("UnmarshalText accepted a million digits")
	}
	if err := d.Scan(long); err == nil {
		t.Error("Scan accepted a million digits")
	}
	if _, err := types.NewDecimalFromString(long); err == nil {
		t.Error("NewDecimalFromString accepted a million digits")
	}
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Errorf("refusing four million-digit spellings took %s, want them refused on length", took)
	}
}
