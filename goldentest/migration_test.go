package goldentest

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/gombit-dev/gombit/generate"
	"github.com/gombit-dev/gombit/resourcegen"
)

// TestMigrateLegacyResourceToModelFirst executes the documented migration path
// (docs/migration-model-first-resources.md) end to end: it stands up a resource on
// the legacy human-owned handler layout, then performs the exact steps the guide
// prescribes — delete handler.go/routes.go, add the .gombit-resource marker, run
// gombit generate, build — and asserts the app compiles. It is the proof the guide
// asks for: the order matters (generate produces Register + the create-body type
// BEFORE anything references them), and gombit generate's Program-Mode loader
// completes a package whose Register main.go still calls but whose legacy handler
// has been removed.
func TestMigrateLegacyResourceToModelFirst(t *testing.T) {
	appDir := scaffoldDemo(t)
	copyDir := filepath.Join(t.TempDir(), "migrate")
	copyTree(t, appDir, copyDir)
	appendLocalReplace(t, copyDir)

	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = copyDir
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}

	// Bootstrap a resource, then rewrite it into the legacy shape: a human-owned
	// handler.go/routes.go (Register lives in routes.go) and no marker — exactly
	// what make resource used to emit before ADR-016.
	stdout := new(bytes.Buffer)
	if err := resourcegen.Generate(context.Background(), resourcegen.Options{
		WorkDir:  copyDir,
		Name:     "Note",
		Fields:   []string{"title:string:required"},
		AtlasBin: missingAtlas,
		Stdout:   stdout,
		Stderr:   io.Discard,
	}); err != nil {
		t.Fatalf("bootstrap resource: %v\n%s", err, stdout.String())
	}
	noteDir := filepath.Join(copyDir, "internal", "note")
	// Drop the marker (a legacy resource has none) and add the legacy plumbing:
	// routes.go provides the Register main.go calls, so the app compiles as it would
	// have on the old layout.
	if err := os.Remove(filepath.Join(noteDir, resourcegen.ResourceMarkerFile)); err != nil {
		t.Fatalf("remove marker: %v", err)
	}
	writeFile(t, filepath.Join(noteDir, "handler.go"), "package note\n\n// legacy human-owned handler; migration removes it.\ntype Handler struct{}\n")
	writeFile(t, filepath.Join(noteDir, "routes.go"), "package note\n\nimport \"github.com/gombit-dev/gombit/framework\"\n\n// Register is the legacy human-owned route registration; handler.gen.go takes it\n// over after migration.\nfunc Register(app *framework.App) { _ = app }\n")

	build := exec.Command("go", "build", "./...")
	build.Dir = copyDir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("legacy resource must compile before migration: %v\n%s", err, out)
	}

	// gombit generate must refuse the legacy layout once the package is marked but
	// still carries handler.go — the guide tells users to migrate, not half-convert.
	writeFile(t, filepath.Join(noteDir, resourcegen.ResourceMarkerFile), "# marker\n")
	if err := generate.Generate(context.Background(), generate.Options{WorkDir: copyDir, Stdout: io.Discard, Stderr: io.Discard}); err == nil {
		t.Fatal("gombit generate must refuse a marked package that still has the legacy handler.go")
	}

	// Migration steps, in the documented order: remove the legacy plumbing (the
	// marker is already present), then generate, then build.
	if err := os.Remove(filepath.Join(noteDir, "handler.go")); err != nil {
		t.Fatalf("rm handler.go: %v", err)
	}
	if err := os.Remove(filepath.Join(noteDir, "routes.go")); err != nil {
		t.Fatalf("rm routes.go: %v", err)
	}
	if err := generate.Generate(context.Background(), generate.Options{WorkDir: copyDir, Stdout: io.Discard, Stderr: io.Discard}); err != nil {
		t.Fatalf("gombit generate after removing legacy plumbing: %v", err)
	}
	// generate must have produced the generator-owned handler (with Register) and
	// seeded hooks referencing the create-body type.
	for _, f := range []string{"dto.gen.go", "handler.gen.go", "hooks.go"} {
		if _, err := os.Stat(filepath.Join(noteDir, f)); err != nil {
			t.Fatalf("generate did not produce internal/note/%s: %v", f, err)
		}
	}
	build = exec.Command("go", "build", "./...")
	build.Dir = copyDir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("migrated app must compile: %v\n%s", err, out)
	}
}
