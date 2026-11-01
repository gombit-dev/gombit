package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestNewHelpDescribesMUIPreset(t *testing.T) {
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	err := ExecuteRoot(context.Background(), NewRoot(stdout, stderr), []string{"new", "--help"})
	if err != nil {
		t.Fatalf("gombit new --help: %v", err)
	}
	out := stdout.String() + stderr.String()
	if !strings.Contains(out, "--ui") {
		t.Fatalf("help missing --ui:\n%s", out)
	}
	if strings.Contains(out, "recorded in gombit.yaml only") {
		t.Fatalf("--ui help still says recorded in gombit.yaml only:\n%s", out)
	}
	if !strings.Contains(out, "mui") {
		t.Fatalf("help missing mui preset:\n%s", out)
	}
}
