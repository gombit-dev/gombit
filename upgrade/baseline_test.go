package upgrade_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/upgrade"
)

// app writes an application directory with goMod and, unless empty, a
// gombit.yaml.
func app(t *testing.T, goMod, gombitYAML string) string {
	t.Helper()
	t.Setenv("GOWORK", "") // the nearest go.work, as the go command finds it
	dir := t.TempDir()
	if goMod != "" {
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if gombitYAML != "" {
		if err := os.WriteFile(filepath.Join(dir, "gombit.yaml"), []byte(gombitYAML), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const goMod = `module example.com/demo

go 1.26

require github.com/gombit-dev/gombit v0.8.2
`

const projectFile = `name: demo
module: example.com/demo
database: sqlite
`

func TestDetectRecordedBaseline(t *testing.T) {
	dir := app(t, goMod, projectFile+upgrade.MetadataBlock(upgrade.ScaffoldVersion))
	b, err := upgrade.Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := upgrade.Baseline{
		Framework: upgrade.Framework{Module: upgrade.FrameworkModulePath, Version: "v0.8.2", Required: "v0.8.2"},
		Scaffold:  upgrade.ScaffoldVersion,
		Metadata:  upgrade.MetadataVersion,
		Recorded:  true,
	}
	if b != want {
		t.Fatalf("Detect = %+v, want %+v", b, want)
	}
}

// TestDetectOldApp: an app generated before upgrade metadata existed, with
// or without a gombit.yaml, is scaffold 0, not recorded.
func TestDetectOldApp(t *testing.T) {
	for name, file := range map[string]string{"no block": projectFile, "no gombit.yaml": ""} {
		t.Run(name, func(t *testing.T) {
			b, err := upgrade.Detect(app(t, goMod, file))
			if err != nil {
				t.Fatal(err)
			}
			if b.Recorded || b.Scaffold != 0 || b.Metadata != 0 || b.Framework.Version != "v0.8.2" {
				t.Fatalf("Detect = %+v, want scaffold 0, not recorded, framework v0.8.2", b)
			}
		})
	}
}

func TestDetectFrameworkReplaces(t *testing.T) {
	for name, tc := range map[string]struct {
		replace string
		want    upgrade.Framework
	}{
		"local checkout": {
			"replace github.com/gombit-dev/gombit => ../gombit\n",
			upgrade.Framework{Module: upgrade.FrameworkModulePath, Required: "v0.8.2", Replace: "../gombit", Local: true},
		},
		"another version": {
			"replace github.com/gombit-dev/gombit => github.com/me/gombit v0.8.3-fork\n",
			upgrade.Framework{Module: upgrade.FrameworkModulePath, Version: "v0.8.3-fork", Required: "v0.8.2", Replace: "github.com/me/gombit v0.8.3-fork"},
		},
		"a replace of another version": {
			"replace github.com/gombit-dev/gombit v0.7.0 => ../gombit\n",
			upgrade.Framework{Module: upgrade.FrameworkModulePath, Version: "v0.8.2", Required: "v0.8.2"},
		},
		// Go resolves a replace of the exact version over one of every
		// version, in either order.
		"exact version replace first": {
			"replace github.com/gombit-dev/gombit v0.8.2 => ../exact\nreplace github.com/gombit-dev/gombit => ../wildcard\n",
			upgrade.Framework{Module: upgrade.FrameworkModulePath, Required: "v0.8.2", Replace: "../exact", Local: true},
		},
		"exact version replace last": {
			"replace github.com/gombit-dev/gombit => ../wildcard\nreplace github.com/gombit-dev/gombit v0.8.2 => ../exact\n",
			upgrade.Framework{Module: upgrade.FrameworkModulePath, Required: "v0.8.2", Replace: "../exact", Local: true},
		},
	} {
		t.Run(name, func(t *testing.T) {
			b, err := upgrade.Detect(app(t, goMod+tc.replace, ""))
			if err != nil {
				t.Fatal(err)
			}
			if b.Framework != tc.want {
				t.Fatalf("Framework = %+v, want %+v", b.Framework, tc.want)
			}
		})
	}
}

func TestDetectRefuses(t *testing.T) {
	for name, tc := range map[string]struct {
		goMod, file string
		want        error
		msg         string
	}{
		"no go.mod":          {"", "", upgrade.ErrNotGombitApp, "no go.mod"},
		"not a gombit app":   {"module example.com/x\n\ngo 1.26\n", "", upgrade.ErrNotGombitApp, "does not require"},
		"newer metadata":     {goMod, "gombit:\n  metadata: 2\n  scaffold: 3\n", upgrade.ErrMetadataTooNew, "upgrade the gombit CLI"},
		"no metadata format": {goMod, "gombit:\n  scaffold: 1\n", nil, "no metadata format"},
		"negative scaffold":  {goMod, "gombit:\n  metadata: 1\n  scaffold: -1\n", nil, "want 0 or more"},
		"no scaffold":        {goMod, "gombit:\n  metadata: 1\n", nil, "no scaffold version"},
		"misspelt scaffold":  {goMod, "gombit:\n  metadata: 1\n  scafold: 1\n", nil, "no scaffold version"},
		"newer scaffold":     {goMod, "gombit:\n  metadata: 1\n  scaffold: 99\n", upgrade.ErrMetadataTooNew, "scaffold version 99"},
		"empty block":        {goMod, "name: demo\ngombit:\n", nil, "the gombit key is empty"},
		"malformed yaml":     {goMod, "gombit: [\n", nil, "gombit.yaml"},
		"malformed go.mod":   {"module\n", "", nil, "go.mod"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := upgrade.Detect(app(t, tc.goMod, tc.file))
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) || !strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("Detect = %v, want %v mentioning %q", err, tc.want, tc.msg)
			}
		})
	}
}

// TestRecordBaseline: an old app's baseline is recorded by appending the
// block (scaffold 0) and changing nothing else; recording again, or an app
// that already records one, writes nothing.
func TestRecordBaseline(t *testing.T) {
	dir := app(t, goMod, "name: demo # keep this comment\nui: mui")
	b, wrote, err := upgrade.RecordBaseline(dir)
	if err != nil || !wrote || !b.Recorded || b.Scaffold != 0 || b.Metadata != upgrade.MetadataVersion {
		t.Fatalf("RecordBaseline = %+v, %v, %v", b, wrote, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "gombit.yaml")) // #nosec G304 -- under t.TempDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := "name: demo # keep this comment\nui: mui\n\n" + upgrade.MetadataBlock(0); string(data) != want {
		t.Fatalf("gombit.yaml =\n%s\nwant\n%s", data, want)
	}
	if _, wrote, err := upgrade.RecordBaseline(dir); err != nil || wrote {
		t.Fatalf("recording again = %v, %v; want nothing written", wrote, err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "gombit.yaml")) // #nosec G304 -- under t.TempDir()
	if string(after) != string(data) {
		t.Fatal("recording again changed gombit.yaml")
	}

	fresh := app(t, goMod, "")
	if _, wrote, err := upgrade.RecordBaseline(fresh); err != nil || !wrote {
		t.Fatalf("RecordBaseline without a gombit.yaml = %v, %v", wrote, err)
	}
	// #nosec G304 -- under t.TempDir()
	if data, _ := os.ReadFile(filepath.Join(fresh, "gombit.yaml")); string(data) != upgrade.MetadataBlock(0) {
		t.Fatalf("created gombit.yaml = %q", data)
	}

	if _, _, err := upgrade.RecordBaseline(app(t, "", "")); !errors.Is(err, upgrade.ErrNotGombitApp) {
		t.Fatalf("RecordBaseline outside an app = %v", err)
	}
}

// TestRecordBaselineRefusesUnappendable: a gombit.yaml the block cannot be
// appended to (a flow mapping, several YAML documents) is refused and left
// as it was.
func TestRecordBaselineRefusesUnappendable(t *testing.T) {
	for name, file := range map[string]string{
		"flow mapping":      "{name: demo, ui: mui}\n",
		"several documents": "name: demo\n---\nother: doc\n",
		"document end":      "name: demo\n...\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := app(t, goMod, file)
			if _, wrote, err := upgrade.RecordBaseline(dir); err == nil || wrote || !strings.Contains(err.Error(), "add it by hand") {
				t.Fatalf("RecordBaseline = %v, %v; want a refusal", wrote, err)
			}
			// #nosec G304 -- under t.TempDir()
			if data, _ := os.ReadFile(filepath.Join(dir, "gombit.yaml")); string(data) != file {
				t.Fatalf("a refused record changed gombit.yaml to %q", data)
			}
		})
	}
}

// workspace writes go.work in root using dirs, and a framework checkout at
// root/gombit.
func workspace(t *testing.T, root, body string) {
	t.Helper()
	checkout := filepath.Join(root, "gombit")
	if err := os.MkdirAll(checkout, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "go.mod"), []byte("module "+upgrade.FrameworkModulePath+"\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.work"), []byte("go 1.26\n\n"+body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestDetectWorkspace: the go.work the go command would use decides the
// framework when it uses the app: a framework checkout among its modules
// makes it local, its replace overrides go.mod's; a workspace that does not
// use the app, or GOWORK=off, decides nothing.
func TestDetectWorkspace(t *testing.T) {
	newApp := func(t *testing.T, root string) string {
		dir := filepath.Join(root, "app")
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	t.Setenv("GOWORK", "")

	root := t.TempDir()
	workspace(t, root, "use (\n\t./app\n\t./gombit\n)\n")
	dir := newApp(t, root)
	b, err := upgrade.Detect(dir)
	want := upgrade.Framework{Module: upgrade.FrameworkModulePath, Required: "v0.8.2", Replace: "./gombit", Local: true, Workspace: filepath.Join(root, "go.work")}
	if err != nil || b.Framework != want {
		t.Fatalf("workspace with a checkout: %+v, %v; want %+v", b.Framework, err, want)
	}

	t.Setenv("GOWORK", "off")
	if b, err := upgrade.Detect(dir); err != nil || b.Framework.Version != "v0.8.2" || b.Framework.Workspace != "" {
		t.Fatalf("GOWORK=off: %+v, %v", b.Framework, err)
	}
	t.Setenv("GOWORK", "")

	root = t.TempDir()
	workspace(t, root, "use ./app\n\nreplace github.com/gombit-dev/gombit => github.com/me/gombit v0.9.0\n")
	dir = newApp(t, root)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod+"replace github.com/gombit-dev/gombit => ../elsewhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err = upgrade.Detect(dir)
	if err != nil || b.Framework.Version != "v0.9.0" || b.Framework.Local || b.Framework.Workspace == "" {
		t.Fatalf("workspace replace: %+v, %v; want v0.9.0 from go.work", b.Framework, err)
	}

	root = t.TempDir()
	workspace(t, root, "use ./gombit\n")
	dir = newApp(t, root)
	if b, err := upgrade.Detect(dir); err != nil || b.Framework.Version != "v0.8.2" || b.Framework.Workspace != "" {
		t.Fatalf("workspace that does not use the app: %+v, %v", b.Framework, err)
	}
}
