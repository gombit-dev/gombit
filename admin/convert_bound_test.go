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
		if _, err := asDecimalString("1e1000000000"); err == nil {
			t.Error("asDecimalString(1e1000000000) accepted, want a refusal")
		}
		if _, ok := decimalValue("1e1000000000"); ok {
			t.Error("decimalValue(1e1000000000) ok, want no bound comparison")
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
