package types

import (
	"fmt"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

// TestDecimalSpellingShapeMatchesValueShape: the parse and read paths measure
// a decimal from its text (spellingShape) to avoid copying and formatting a
// wide coefficient; that must give exactly what DecimalShapeOf gives for the
// parsed value, the one size rule, for every plain spelling.
func TestDecimalSpellingShapeMatchesValueShape(t *testing.T) {
	var spellings []string
	for _, sign := range []string{"", "-", "+"} {
		for _, intPart := range []string{"", "0", "00", "1", "10", "1000", "0012", "123456789012345678901234567890", strings.Repeat("9", 999), strings.Repeat("9", 1000), "1" + strings.Repeat("0", 999)} {
			for _, frac := range []string{"", ".", ".0", ".5", ".50", ".05", ".000", "." + strings.Repeat("0", 998) + "1", "." + strings.Repeat("0", 999) + "1", "." + strings.Repeat("9", 30), "." + strings.Repeat("0", 30)} {
				for _, exp := range []string{"", "e0", "e5", "E-3", "e+2", "e999", "e-999", "e1000", "e-1000", "e1001", "e-1001"} {
					s := sign + intPart + frac + exp
					spellings = append(spellings, s)
				}
			}
		}
	}
	checked := 0
	for _, s := range spellings {
		if len(s) > maxDecimalSpelling {
			continue
		}
		parsed, err := decimal.NewFromString(s)
		if err != nil {
			continue
		}
		fromText, ok, textErr := spellingShape(s)
		if !ok {
			continue
		}
		fromValue, valueErr := DecimalShapeOf(parsed)
		if (textErr == nil) != (valueErr == nil) || fromText != fromValue {
			t.Fatalf("%.60q: text shape %+v (%v), value shape %+v (%v)", s, fromText, textErr, fromValue, valueErr)
		}
		checked++
	}
	if checked < 1000 {
		t.Fatalf("only %d spellings compared; the sweep lost its coverage", checked)
	}
}

func FuzzDecimalSpellingShapeMatchesValueShape(f *testing.F) {
	for _, seed := range []string{"1.5", "-0.050", "1e-999", "0e1000", "123.450000000000000000000000000000", "+.5e1", "1.", "00"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > maxDecimalSpelling {
			return
		}
		parsed, err := decimal.NewFromString(s)
		fromText, ok, textErr := spellingShape(s)
		if !ok {
			return
		}
		if err != nil {
			t.Fatalf("%q: spellingShape understood it, shopspring did not parse it: %v", s, err)
		}
		fromValue, valueErr := DecimalShapeOf(parsed)
		if (textErr == nil) != (valueErr == nil) || fromText != fromValue {
			t.Fatalf("%q: text shape %+v (%v), value shape %+v (%v)", s, fromText, textErr, fromValue, valueErr)
		}
	})
}

// TestDecimalScanAddsNoAllocations: reading a decimal through types.Decimal
// costs what shopspring's own Scan costs, for an ordinary value and for a
// wide one (MySQL pads DECIMAL(65,30) to 30 places): the size rule is measured
// from the text, not by copying and formatting the coefficient.
func TestDecimalScanAddsNoAllocations(t *testing.T) {
	for _, v := range []any{
		[]byte("12345.6789"),
		[]byte("123.450000000000000000000000000000"),
		"123.450000000000000000000000000000",
	} {
		var plain decimal.Decimal
		base := testing.AllocsPerRun(200, func() { _ = plain.Scan(v) })
		var wrapped Decimal
		got := testing.AllocsPerRun(200, func() { _ = wrapped.Scan(v) })
		if got > base {
			t.Errorf("Scan(%v): %v allocs, shopspring's own Scan %v", fmt.Sprint(v), got, base)
		}
	}
}
