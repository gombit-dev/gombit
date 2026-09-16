package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestGenerateHelpDescribesCheck(t *testing.T) {
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	if err := ExecuteRoot(context.Background(), NewRoot(stdout, stderr), []string{"generate", "--help"}); err != nil {
		t.Fatalf("gombit generate --help: %v", err)
	}
	out := stdout.String() + stderr.String()
	for _, want := range []string{"--check", "*.gen.go", "hooks file"} {
		if !strings.Contains(out, want) {
			t.Fatalf("help missing %q:\n%s", want, out)
		}
	}
}

// Run outside a gombit application: it must fail on layout validation before
// attempting to run any loader (the cli package dir has no go.mod / app layout).
func TestGenerateRejectsNonAppDirectory(t *testing.T) {
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	err := ExecuteRoot(context.Background(), NewRoot(stdout, stderr), []string{"generate"})
	if err == nil {
		t.Fatal("gombit generate outside an app: error = nil, want error")
	}
	if !strings.Contains(err.Error(), "gombit generate:") {
		t.Fatalf("error should be wrapped as a gombit generate failure, got: %v", err)
	}
	if !strings.Contains(err.Error(), "application directory") {
		t.Fatalf("error should point the user at an app directory, got: %v", err)
	}
}
