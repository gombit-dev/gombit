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
		all := maps.Clone(actions)
		codemods["check-reads-only"] = Codemod{ID: "check-reads-only", Description: "d", Package: FrameworkModulePath + "/db", Symbol: "Old", Rename: "New"}
		for id := range codemods {
			all[codemodActionPrefix+id], _ = LookupAction(codemodActionPrefix + id)
		}
		delete(codemods, "check-reads-only")
		for name, a := range all {
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

// TestCodemodAction: a registered codemod is the action "codemod:<id>",
// which an automatic manifest change can name; its Check and Apply run the
// codemod (here on an app that does not use its API: nothing to do).
func TestCodemodAction(t *testing.T) {
	codemods["rename-x"] = Codemod{ID: "rename-x", Description: "db.Old is db.New", Package: FrameworkModulePath + "/db", Symbol: "Old", Rename: "New"}
	defer delete(codemods, "rename-x")
	a, ok := LookupAction("codemod:rename-x")
	if !ok || !strings.Contains(a.Description, "gombit upgrade fix --only rename-x") {
		t.Fatalf("LookupAction = %+v, %v", a, ok)
	}
	if _, ok := LookupAction("codemod:missing"); ok {
		t.Fatal("LookupAction found an unregistered codemod")
	}
	data := "format: 1\nreleases:\n  - version: v0.1.0\n  - version: v0.2.0\n    changes:\n      - {id: x, kind: automatic, action: \"codemod:rename-x\", area: api, summary: s}\n"
	if _, err := ParseManifest([]byte(data)); err != nil {
		t.Fatalf("ParseManifest = %v", err)
	}
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "-mod=mod")
	t.Setenv("GOPROXY", "off")
	dir := t.TempDir()
	for name, content := range map[string]string{"go.mod": "module example.com/x\n\ngo 1.21\n", "main.go": "package main\n\nfunc main() {}\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if needed, err := a.Check(dir, Baseline{}); err != nil || needed {
		t.Fatalf("Check = %v, %v", needed, err)
	}
	if changed, err := a.Apply(dir); err != nil || changed {
		t.Fatalf("Apply = %v, %v", changed, err)
	}
}

// TestShippedCodemodsAreValid: every codemod this framework ships is well
// formed and registered under its ID.
func TestShippedCodemodsAreValid(t *testing.T) {
	for id, c := range codemods {
		if c.ID != id {
			t.Errorf("codemod %s registered as %s", c.ID, id)
		}
		if err := c.validate(); err != nil {
			t.Error(err)
		}
		// The policy's support window starts at the release that ships it.
		if c.Since == "" {
			t.Errorf("codemod %s: Since names no release (the compatibility policy keeps it until RemovableFrom(Since))", id)
		}
	}
}
