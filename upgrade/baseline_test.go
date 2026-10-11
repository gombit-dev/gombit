package upgrade_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/gombit-dev/gombit/internal/atomicfile"

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

// sameFramework compares Frameworks by value, Replace included.
func sameFramework(a, b upgrade.Framework) bool {
	ar, br := a.Replace, b.Replace
	a.Replace, b.Replace = nil, nil
	return a == b && (ar == nil) == (br == nil) && (ar == nil || *ar == *br)
}

func TestDetectFrameworkReplaces(t *testing.T) {
	const fw = upgrade.FrameworkModulePath
	for name, tc := range map[string]struct {
		replace string
		want    upgrade.Framework
	}{
		"local checkout": {
			"replace github.com/gombit-dev/gombit => ../gombit\n",
			upgrade.Framework{Module: fw, Required: "v0.8.2", Replace: &upgrade.Replacement{Path: "../gombit"}, Local: true},
		},
		// A fork is not a framework release: its version is not the
		// framework's, so none is claimed.
		"a fork": {
			"replace github.com/gombit-dev/gombit => github.com/me/gombit v0.8.3-fork\n",
			upgrade.Framework{Module: fw, Required: "v0.8.2", Replace: &upgrade.Replacement{Path: "github.com/me/gombit", Version: "v0.8.3-fork"}},
		},
		"another framework version": {
			"replace github.com/gombit-dev/gombit => github.com/gombit-dev/gombit v0.9.0\n",
			upgrade.Framework{Module: fw, Version: "v0.9.0", Required: "v0.8.2", Replace: &upgrade.Replacement{Path: fw, Version: "v0.9.0"}},
		},
		"a replace of another version": {
			"replace github.com/gombit-dev/gombit v0.7.0 => ../gombit\n",
			upgrade.Framework{Module: fw, Version: "v0.8.2", Required: "v0.8.2"},
		},
		// Go resolves a replace of the exact version over one of every
		// version, in either order.
		"exact version replace first": {
			"replace github.com/gombit-dev/gombit v0.8.2 => ../exact\nreplace github.com/gombit-dev/gombit => ../wildcard\n",
			upgrade.Framework{Module: fw, Required: "v0.8.2", Replace: &upgrade.Replacement{Path: "../exact", ForVersion: "v0.8.2"}, Local: true},
		},
		"exact version replace last": {
			"replace github.com/gombit-dev/gombit => ../wildcard\nreplace github.com/gombit-dev/gombit v0.8.2 => ../exact\n",
			upgrade.Framework{Module: fw, Required: "v0.8.2", Replace: &upgrade.Replacement{Path: "../exact", ForVersion: "v0.8.2"}, Local: true},
		},
	} {
		t.Run(name, func(t *testing.T) {
			b, err := upgrade.Detect(app(t, goMod+tc.replace, ""))
			if err != nil {
				t.Fatal(err)
			}
			if !sameFramework(b.Framework, tc.want) {
				t.Fatalf("Framework = %+v (replace %+v), want %+v (replace %+v)", b.Framework, b.Framework.Replace, tc.want, tc.want.Replace)
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
		"misspelt scaffold":  {goMod, "gombit:\n  metadata: 1\n  scafold: 1\n", nil, "gombit.scafold is not a key of metadata format 1"},
		"float metadata":     {goMod, "gombit:\n  metadata: 1.9\n  scaffold: 0\n", nil, `gombit.metadata is "1.9", want an integer`},
		"float scaffold":     {goMod, "gombit:\n  metadata: 1\n  scaffold: 0.5\n", nil, `gombit.scaffold is "0.5", want an integer`},
		"string scaffold":    {goMod, "gombit:\n  metadata: 1\n  scaffold: \"1\"\n", nil, "want an integer"},
		"repeated key":       {goMod, "gombit:\n  metadata: 1\n  scaffold: 0\n  scaffold: 1\n", nil, "gombit.scaffold appears twice"},
		"repeated block":     {goMod, "gombit:\n  metadata: 1\n  scaffold: 0\ngombit:\n  metadata: 1\n  scaffold: 1\n", nil, "appears twice"},
		"scalar block":       {goMod, "gombit: 1\n", nil, "gombit must be the metadata block"},
		"not a mapping":      {goMod, "- a\n- b\n", nil, "want a mapping"},
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

// TestDetectWorkspace: whether a go.work applies is the go command's answer
// (go env GOWORK), and then the workspace decides the build, so no version
// is claimed. GOWORK=off, set in the environment or with go env -w, leaves
// go.mod; GOWORK=auto searches; a relative GOWORK is refused, as by Go.
func TestDetectWorkspace(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "app")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	// go.mod's replace is not what applies in the workspace, so it is not
	// reported (nor Local) while a go.work applies.
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod+"replace github.com/gombit-dev/gombit => ../fw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(root, "go.work")
	if err := os.WriteFile(work, []byte("go 1.26\n\nuse ./app\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOENV", filepath.Join(root, "goenv")) // no go env -w settings yet

	for _, gowork := range []string{"", "auto", work} {
		t.Setenv("GOWORK", gowork)
		b, err := upgrade.Detect(dir)
		if err != nil {
			t.Fatalf("GOWORK=%q: %v", gowork, err)
		}
		want := upgrade.Framework{Module: upgrade.FrameworkModulePath, Required: "v0.8.2", Workspace: work}
		if !sameFramework(b.Framework, want) {
			t.Fatalf("GOWORK=%q: Framework = %+v, want %+v", gowork, b.Framework, want)
		}
	}

	offWant := upgrade.Framework{Module: upgrade.FrameworkModulePath, Required: "v0.8.2", Replace: &upgrade.Replacement{Path: "../fw"}, Local: true}
	t.Setenv("GOWORK", "off")
	if b, err := upgrade.Detect(dir); err != nil || !sameFramework(b.Framework, offWant) {
		t.Fatalf("GOWORK=off: %+v, %v; want %+v", b.Framework, err, offWant)
	}

	// go env -w GOWORK=off lives in the GOENV file, not the environment.
	t.Setenv("GOWORK", "")
	if err := os.WriteFile(filepath.Join(root, "goenv"), []byte("GOWORK=off\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := upgrade.Detect(dir); err != nil || !sameFramework(b.Framework, offWant) {
		t.Fatalf("go env -w GOWORK=off: %+v, %v; want %+v", b.Framework, err, offWant)
	}

	t.Setenv("GOWORK", filepath.Join("..", "go.work"))
	if _, err := upgrade.Detect(dir); err == nil || !strings.Contains(err.Error(), "not an absolute path") {
		t.Fatalf("a relative GOWORK: %v; want the go command's refusal", err)
	}
}

// TestRecordBaselineKeepsTheRest: the block is appended only where the rest
// of the file keeps its meaning. A keep-chomped block scalar at the end
// would swallow a blank separator line, so the block goes without one.
func TestRecordBaselineKeepsTheRest(t *testing.T) {
	for _, file := range []string{"notes: |+\n  keep me\n", "notes: >+\n  keep me\n\n"} {
		dir := app(t, goMod, file)
		if _, wrote, err := upgrade.RecordBaseline(dir); err != nil || !wrote {
			t.Fatalf("%q: RecordBaseline = %v, %v", file, wrote, err)
		}
		data, err := os.ReadFile(filepath.Join(dir, "gombit.yaml")) // #nosec G304 -- under t.TempDir()
		if err != nil {
			t.Fatal(err)
		}
		var before, after map[string]any
		if yaml.Unmarshal([]byte(file), &before) != nil || yaml.Unmarshal(data, &after) != nil || before["notes"] != after["notes"] {
			t.Fatalf("%q: notes changed from %q to %q", file, before["notes"], after["notes"])
		}
	}
}

// TestRecordBaselineKeepsTheFileMode: the file is replaced (atomically,
// through a temp file), keeping its permissions and leaving no temp file.
func TestRecordBaselineKeepsTheFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix permission bits")
	}
	dir := app(t, goMod, "name: demo\n")
	path := filepath.Join(dir, "gombit.yaml")
	if err := os.Chmod(path, 0o640); err != nil { // #nosec G302 -- the mode under test
		t.Fatal(err)
	}
	if _, wrote, err := upgrade.RecordBaseline(dir); err != nil || !wrote {
		t.Fatalf("RecordBaseline = %v, %v", wrote, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("gombit.yaml mode = %v, %v; want 0640", info.Mode().Perm(), err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".gombit-tmp-") {
			t.Fatalf("a temp file was left behind: %s", e.Name())
		}
	}
}

// TestRecordBaselineNotDurable: when gombit.yaml was replaced but the
// directory sync failed, RecordBaseline says it wrote (the block is in the
// file), with the error.
func TestRecordBaselineNotDurable(t *testing.T) {
	dir := app(t, goMod, "name: demo\n")
	restore := atomicfile.SetSyncDirForTest(func(string) error { return errors.New("injected: fsync failed") })
	b, wrote, err := upgrade.RecordBaseline(dir)
	restore()
	if !wrote || !b.Recorded || !errors.Is(err, atomicfile.ErrNotDurable) {
		t.Fatalf("RecordBaseline = %+v, %v, %v; want wrote, recorded, and ErrNotDurable", b, wrote, err)
	}
	if after, err := upgrade.Detect(dir); err != nil || !after.Recorded {
		t.Fatalf("Detect after = %+v, %v; want the block in gombit.yaml", after, err)
	}
}
