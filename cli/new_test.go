package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/internal/frameworkmod"
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

// `gombit new` pins the generated go.mod to the framework the CLI was built
// with, so an app's own `cmd/gombit` has to resolve that version from its
// framework dependency rather than from the application module.
func TestNewFromAnAppBinaryPinsTheFramework(t *testing.T) {
	// What scaffold prints when the version it has to pin is not a module
	// version — the only signal that a bad pin is coming.
	const unresolvablePinWarning = `"dev" is not a module version`

	tests := []struct {
		name  string
		info  *debug.BuildInfo
		want  string
		warns bool
	}{
		{
			// Before the fix this pinned v0.0.0-20261015013337-ba8fc61f2a66,
			// the app's own pseudo-version, and warned about nothing.
			name: "released framework dependency",
			info: &debug.BuildInfo{
				Main: debug.Module{Path: "github.com/example/rel", Version: "v0.0.0-20261015013337-ba8fc61f2a66"},
				Deps: []*debug.Module{{Path: frameworkmod.ModulePath, Version: "v0.7.1"}},
			},
			want: "v0.7.1",
		},
		{
			name: "framework replaced by a checkout",
			info: &debug.BuildInfo{
				Main: debug.Module{Path: "github.com/example/rel", Version: "(devel)"},
				Deps: []*debug.Module{{
					Path:    frameworkmod.ModulePath,
					Version: "v0.7.1",
					Replace: &debug.Module{Path: filepath.Join("..", "gombit")},
				}},
			},
			want:  "v0.0.0",
			warns: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubBuild(t, "", "", "", tt.info)
			dir := t.TempDir()
			t.Chdir(dir)

			stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
			if err := ExecuteRoot(context.Background(), NewRoot(stdout, stderr),
				[]string{"new", "demo", "--skip-tidy"}); err != nil {
				t.Fatalf("gombit new: %v\n%s\n%s", err, stdout.String(), stderr.String())
			}
			// #nosec G304 -- test-controlled path under t.TempDir()
			data, err := os.ReadFile(filepath.Join(dir, "demo", "go.mod"))
			if err != nil {
				t.Fatalf("read the generated go.mod: %v", err)
			}
			pin := "require " + frameworkmod.ModulePath + " " + tt.want
			if !strings.Contains(string(data), pin) {
				t.Fatalf("generated go.mod lacks %q:\n%s", pin, data)
			}
			if warned := strings.Contains(stderr.String(), unresolvablePinWarning); warned != tt.warns {
				t.Fatalf("warned about an unresolvable pin = %v, want %v:\n%s", warned, tt.warns, stderr.String())
			}
		})
	}
}
