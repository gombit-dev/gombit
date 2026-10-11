package upgrade

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestActionWithoutApplyIsNotImplemented: an action registered without an
// implementation is not one automatic changes may name.
func TestActionWithoutApplyIsNotImplemented(t *testing.T) {
	actions["described-only"] = Action{Description: "says what it would do"}
	defer delete(actions, "described-only")
	if _, ok := LookupAction("described-only"); ok {
		t.Fatal("LookupAction found an action with no Apply")
	}
	data := "format: 1\nreleases:\n  - version: v0.1.0\n  - version: v0.2.0\n    changes:\n      - {id: x, kind: automatic, action: described-only, area: api, summary: s}\n"
	if _, err := ParseManifest([]byte(data)); err == nil || !strings.Contains(err.Error(), "not one this gombit implements") {
		t.Fatalf("ParseManifest = %v; want the action refused", err)
	}
}

// TestActionChecksWriteNothing: every registered action's Check reads
// only, whether or not the app needs the change, so a dry run's plan
// cannot change the app it plans for.
func TestActionChecksWriteNothing(t *testing.T) {
	goMod := "module example.com/demo\n\ngo 1.26\n\nrequire github.com/gombit-dev/gombit v0.6.1\n"
	for _, gombitYAML := range []string{"name: demo\n", "name: demo\n" + MetadataBlock(ScaffoldVersion)} {
		dir := t.TempDir()
		t.Setenv("GOWORK", "off")
		for name, content := range map[string]string{"go.mod": goMod, "gombit.yaml": gombitYAML} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		b, err := Detect(dir)
		if err != nil {
			t.Fatal(err)
		}
		before := files(t, dir)
		for name, a := range actions {
			if a.Check == nil {
				continue
			}
			if _, err := a.Check(dir, b); err != nil {
				t.Fatalf("%s: Check = %v", name, err)
			}
			if after := files(t, dir); !maps.Equal(before, after) {
				t.Fatalf("%s: Check changed the app's files", name)
			}
		}
	}
}

// files is every file in dir with its content and modification time.
func files(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name())) // #nosec G304 -- under t.TempDir()
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = info.ModTime().String() + "\x00" + string(data)
	}
	return out
}
