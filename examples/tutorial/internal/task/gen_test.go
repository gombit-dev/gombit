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

// TestGeneratedFilesAreFresh is this example's per-PR `gombit generate --check`
// (RESGEN-1 / #352): it re-derives the generator-owned files from the Task model
// and byte-compares them against the committed *.gen.go, so changing the model
// without regenerating fails CI. The tutorial is part of the framework module
// rather than a standalone gombit app, so the check runs in-process through
// RenderResource — the same derivation the CLI uses — instead of the Program-Mode
// `gombit generate` loader. Seed-once files (hooks.go) are human-owned and not
// compared. Regenerate with:
//
//	go test ./examples/tutorial/internal/task/ -update
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
