package admin

import (
	"testing"
	"time"
)

// TestDecimalParsingRefusesUnboundedSizeQuickly: the admin parses a decimal and
// compares it against min/max before the write; a huge exponent must be
// refused before either formats or rescales it (#440 review).
func TestDecimalParsingRefusesUnboundedSizeQuickly(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		// A zero coefficient is no cheaper: comparing rescales to the exponent.
		for _, hostile := range []string{"1e1000000000", "0e1000000000", "-0e999999999", "0e-1000000000"} {
			if _, err := asDecimalString(hostile); err == nil {
				t.Errorf("asDecimalString(%s) accepted, want a refusal", hostile)
			}
			if _, ok := decimalValue(hostile); ok {
				t.Errorf("decimalValue(%s) ok, want no bound comparison", hostile)
			}
		}
		if _, err := asDecimalString("12.5"); err != nil {
			t.Errorf("asDecimalString(12.5) = %v, want accepted", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("admin decimal parsing still running after 2s; the value is being formatted")
	}
}
