package types

import "testing"

func TestDecimalWithin(t *testing.T) {
	ok := MustDecimal("10")
	if err := DecimalWithin(ok, "0", "10"); err != nil {
		t.Fatal(err)
	}
	if err := DecimalWithin(MustDecimal("10.01"), "", "10"); err == nil {
		t.Fatal("10.01 must be above max 10")
	}
	if err := DecimalWithin(MustDecimal("-1"), "0", ""); err == nil {
		t.Fatal("-1 must be below min 0")
	}
}
