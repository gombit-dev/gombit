package task_test

import (
	"bytes"
	"flag"
	"os"
	"path"
	"path/filepath"
	"testing"

	"github.com/gombit-dev/gombit/examples/tutorial/internal/task"
	"github.com/gombit-dev/gombit/resourcegen"
)

var updateGen = flag.Bool("update", false, "rewrite the committed task *.gen.go from the model")

// TestGeneratedFilesAreFresh keeps this example's committed *.gen.go in sync with
// the Task model: it re-derives the generator-owned files with the same derivation
// core the generator uses (resourcegen.RenderResource) and byte-compares them, so
// changing the model without regenerating fails CI. Seed-once files (hooks.go) are
// human-owned and not compared. Regenerate with:
//
//	go test ./examples/tutorial/internal/task/ -update
//
// This is NOT `gombit generate --check`: it exercises only the derivation +
// byte-compare, not the CLI's marker discovery, Program-Mode compilation,
// app-layout / legacy-handler checks, or checkArtifacts. The tutorial is part of
// the framework module, not a standalone app, so it cannot host that CLI. The
// per-PR `gombit generate --check` gate over a real app layout (RESGEN-1 / #352)
// is still open — this is an in-process substitute for a resource that lives here.
func TestGeneratedFilesAreFresh(t *testing.T) {
	arts, err := resourcegen.RenderResource(&task.Task{}, "task")
	if err != nil {
		t.Fatalf("RenderResource: %v", err)
	}
	for _, a := range arts {
		if a.Ownership != resourcegen.GeneratorOwned {
			continue
		}
		name := path.Base(a.Path)
		file := filepath.Join(".", name)
		if *updateGen {
			if err := os.WriteFile(file, a.Content, 0o600); err != nil {
				t.Fatalf("update %s: %v", name, err)
			}
			continue
		}
		got, err := os.ReadFile(file) // #nosec G304 -- fixed generated-file name in the test's own package dir
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !bytes.Equal(got, a.Content) {
			t.Errorf("%s is stale — the Task model changed without regenerating; run:\n\tgo test ./examples/tutorial/internal/task/ -update", name)
		}
	}
}
