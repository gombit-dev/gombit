package cli

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/dev"
)

// stubRunDev fails the test if dev reaches the point of starting processes:
// both of these cases must be refused by the CLI's validation.
func stubRunDev(t *testing.T) {
	t.Helper()
	old := RunDev
	RunDev = func(context.Context, dev.Options) error {
		t.Fatal("dev started with an address it should have refused")
		return nil
	}
	t.Cleanup(func() { RunDev = old })
}

// An address read from configuration did not come from --http, so the error
// must name GOMBIT_HTTP_ADDR. ValidateFlags labels every address --http, which
// is where `GOMBIT_HTTP_ADDR=:99999 gombit dev` used to print the wrong source
// (#513 review).
func TestDevLabelsAConfiguredAddressAsGOMBITHTTPADDR(t *testing.T) {
	t.Setenv("GOMBIT_HTTP_ADDR", ":99999")
	stubRunDev(t)

	err := ExecuteRoot(context.Background(), NewRoot(io.Discard, io.Discard), []string{"dev"})
	if err == nil {
		t.Fatal("dev with GOMBIT_HTTP_ADDR=:99999 error = nil, want the address refused")
	}
	for _, want := range []string{"GOMBIT_HTTP_ADDR", `port "99999" must be between 1 and 65535`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "--http") {
		t.Fatalf("error = %q, want no claim that the flag supplied the address", err)
	}
}

// The flag still wins over configuration, and its error names --http.
func TestDevLabelsAFlagAddressAsHTTP(t *testing.T) {
	t.Setenv("GOMBIT_HTTP_ADDR", ":99999")
	stubRunDev(t)

	err := ExecuteRoot(context.Background(), NewRoot(io.Discard, io.Discard), []string{"dev", "--http", ":99998"})
	if err == nil {
		t.Fatal("dev --http :99998 error = nil, want the address refused")
	}
	if !strings.Contains(err.Error(), "--http") {
		t.Fatalf("error = %q, want it to name --http", err)
	}
	if strings.Contains(err.Error(), "GOMBIT_HTTP_ADDR") {
		t.Fatalf("error = %q, want no claim that configuration supplied the address", err)
	}
}
