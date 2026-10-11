package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/internal/atomicfile"
	"github.com/gombit-dev/gombit/upgrade"
)

func upgradeApp(t *testing.T, gombitYAML string) string {
	t.Helper()
	t.Setenv("GOWORK", "off")
	dir := t.TempDir()
	goMod := "module example.com/demo\n\ngo 1.26\n\nrequire github.com/gombit-dev/gombit v0.8.2\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o600); err != nil {
		t.Fatal(err)
	}
	if gombitYAML != "" {
		if err := os.WriteFile(filepath.Join(dir, "gombit.yaml"), []byte(gombitYAML), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func runUpgradeCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := ExecuteRoot(context.Background(), NewRoot(&stdout, &stderr), append([]string{"upgrade"}, args...))
	return stdout.String(), err
}

func TestUpgradeBaseline(t *testing.T) {
	recorded := upgradeApp(t, "name: demo\n"+upgrade.MetadataBlock(upgrade.ScaffoldVersion))
	out, err := runUpgradeCLI(t, "baseline", "--dir", recorded)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Framework: github.com/gombit-dev/gombit v0.8.2 (go.mod)", "Scaffold:  1 (recorded in gombit.yaml, metadata format 1)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}

	out, err = runUpgradeCLI(t, "baseline", "--dir", recorded, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var b upgrade.Baseline
	if err := json.Unmarshal([]byte(out), &b); err != nil || !b.Recorded || b.Scaffold != upgrade.ScaffoldVersion || b.Framework.Version != "v0.8.2" {
		t.Fatalf("--json = %s (%v)", out, err)
	}
}

// TestUpgradeBaselineOldApp: an app without metadata is detected as
// scaffold 0, and nothing is written until --write records it.
func TestUpgradeBaselineOldApp(t *testing.T) {
	dir := upgradeApp(t, "name: demo\n")
	out, err := runUpgradeCLI(t, "baseline", "--dir", dir)
	if err != nil || !strings.Contains(out, "Scaffold:  0 (no upgrade metadata") || !strings.Contains(out, "--write") {
		t.Fatalf("baseline of an old app = %q, %v", out, err)
	}
	// #nosec G304 -- under t.TempDir()
	if data, _ := os.ReadFile(filepath.Join(dir, "gombit.yaml")); string(data) != "name: demo\n" {
		t.Fatalf("baseline without --write changed gombit.yaml: %q", data)
	}
	out, err = runUpgradeCLI(t, "baseline", "--dir", dir, "--write")
	if err != nil || !strings.Contains(out, "recorded in gombit.yaml now") {
		t.Fatalf("baseline --write = %q, %v", out, err)
	}
	b, err := upgrade.Detect(dir)
	if err != nil || !b.Recorded || b.Scaffold != 0 {
		t.Fatalf("after --write: %+v, %v", b, err)
	}
	// --json says whether --write wrote: not again.
	out, err = runUpgradeCLI(t, "baseline", "--dir", dir, "--write", "--json")
	var written struct {
		Recorded bool  `json:"recorded"`
		Written  *bool `json:"written"`
	}
	if err != nil || json.Unmarshal([]byte(out), &written) != nil || !written.Recorded || written.Written == nil || *written.Written {
		t.Fatalf("--write --json on a recorded app = %s, %v; want written:false", out, err)
	}
	fresh := upgradeApp(t, "name: demo\n")
	if out, err = runUpgradeCLI(t, "baseline", "--dir", fresh, "--write", "--json"); err != nil || !strings.Contains(out, `"written": true`) {
		t.Fatalf("--write --json on an old app = %s, %v; want written:true", out, err)
	}
}

func TestUpgradeRefuses(t *testing.T) {
	if _, err := runUpgradeCLI(t, "baseline", "--dir", t.TempDir()); err == nil || !strings.Contains(err.Error(), "not a Gombit application") {
		t.Fatalf("baseline outside an app = %v", err)
	}
	if _, err := runUpgradeCLI(t); err == nil || !strings.Contains(err.Error(), "--dry-run or a subcommand is required") {
		t.Fatalf("upgrade without a subcommand = %v", err)
	}
}

// TestUpgradeNotes: the notes come from the embedded manifest; a path the
// manifest does not describe fails rather than guessing.
func TestUpgradeNotes(t *testing.T) {
	m, err := upgrade.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	first, latest := m.Releases[0].Version, m.Latest()

	out, err := runUpgradeCLI(t, "notes")
	if err != nil || !strings.Contains(out, "## "+latest+"\n") || !strings.Contains(out, "The first release the compatibility manifest covers") {
		t.Fatalf("notes = %q, %v", out, err)
	}
	var want strings.Builder
	r, err := m.Release(latest)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RenderNotes(&want, []upgrade.Release{r}); err != nil {
		t.Fatal(err)
	}
	if out, err := runUpgradeCLI(t, "notes", "--release", latest); err != nil || out != want.String() {
		t.Fatalf("notes --release %s = %q, %v; want %q", latest, out, err, want.String())
	}

	out, err = runUpgradeCLI(t, "notes", "--from", first, "--json")
	var got struct {
		Releases       []upgrade.Release      `json:"releases"`
		Classification upgrade.Classification `json:"classification"`
	}
	if err != nil || json.Unmarshal([]byte(out), &got) != nil || got.Classification.Manual == nil {
		t.Fatalf("notes --from %s --json = %s, %v", first, out, err)
	}
	path, _ := m.Path(first, latest)
	if len(got.Releases) != len(path) {
		t.Fatalf("notes --from %s: %d releases, want %d", first, len(got.Releases), len(path))
	}

	for _, tc := range []struct {
		args []string
		msg  string
	}{
		{[]string{"notes", "--from", "v0.0.1"}, "no upgrade path"},
		{[]string{"notes", "--from", first, "--to", "v99.0.0"}, "upgrade the gombit CLI"},
		{[]string{"notes", "--release", "v99.0.0"}, "no release v99.0.0"},
		{[]string{"notes", "--to", latest}, "--to needs --from"},
		{[]string{"notes", "--release", latest, "--from", first}, "cannot be combined"},
	} {
		if _, err := runUpgradeCLI(t, tc.args...); err == nil || !strings.Contains(err.Error(), tc.msg) {
			t.Fatalf("%v = %v; want an error mentioning %q", tc.args, err, tc.msg)
		}
	}
}

// TestUpgradeBaselineFrameworkLines: a fork, a local checkout and a
// workspace claim no framework version, and say why.
func TestUpgradeBaselineFrameworkLines(t *testing.T) {
	for name, tc := range map[string]struct{ replace, want string }{
		"fork":  {"replace github.com/gombit-dev/gombit => github.com/me/gombit v0.8.3-fork\n", "replaced by another module, github.com/me/gombit v0.8.3-fork (not a framework release)"},
		"local": {"replace github.com/gombit-dev/gombit => ../gombit\n", "replaced by the local directory ../gombit (no published version)"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := upgradeApp(t, "")
			goMod := filepath.Join(dir, "go.mod")
			data, err := os.ReadFile(goMod) // #nosec G304 -- under t.TempDir()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(goMod, append(data, tc.replace...), 0o600); err != nil { // #nosec G703 -- under t.TempDir()
				t.Fatal(err)
			}
			if out, err := runUpgradeCLI(t, "baseline", "--dir", dir); err != nil || !strings.Contains(out, tc.want) {
				t.Fatalf("baseline = %q, %v; want %q", out, err, tc.want)
			}
		})
	}

	root := t.TempDir()
	work := filepath.Join(root, "go.work")
	if err := os.WriteFile(work, []byte("go 1.26\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := upgradeApp(t, "") // sets GOWORK=off
	t.Setenv("GOWORK", work)
	out, err := runUpgradeCLI(t, "baseline", "--dir", dir)
	if err != nil || !strings.Contains(out, "decided by the workspace "+work) {
		t.Fatalf("baseline in a workspace = %q, %v", out, err)
	}
}

// TestUpgradeBaselineWriteNotDurable: --write that changed gombit.yaml but
// could not sync it prints what the file now records, then fails.
func TestUpgradeBaselineWriteNotDurable(t *testing.T) {
	dir := upgradeApp(t, "name: demo\n")
	restore := atomicfile.SetSyncDirForTest(func(string) error { return errors.New("injected: fsync failed") })
	out, err := runUpgradeCLI(t, "baseline", "--dir", dir, "--write")
	restore()
	if !strings.Contains(out, "recorded in gombit.yaml now") || !errors.Is(err, atomicfile.ErrNotDurable) {
		t.Fatalf("baseline --write = %q, %v; want the recorded baseline and ErrNotDurable", out, err)
	}
}

// TestUpgradeNotesReleaseGate: the release workflow's gate fails a stable
// tag the manifest is not closed at (here, the embedded manifest still has
// changes under unreleased), and needs --release.
func TestUpgradeNotesReleaseGate(t *testing.T) {
	m, err := upgrade.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	if r, err := m.Release(upgrade.Unreleased); err != nil || len(r.Changes) == 0 {
		t.Skip("the embedded manifest has nothing under unreleased")
	}
	if _, err := runUpgradeCLI(t, "notes", "--release", m.Latest(), "--release-gate"); err == nil || !strings.Contains(err.Error(), "still under unreleased") {
		t.Fatalf("notes --release %s --release-gate = %v; want the unreleased changes refused", m.Latest(), err)
	}
	if _, err := runUpgradeCLI(t, "notes", "--release-gate"); err == nil || !strings.Contains(err.Error(), "--release-gate needs --release") {
		t.Fatalf("notes --release-gate = %v", err)
	}
}

// TestUpgradeDryRun: gombit upgrade --dry-run prints the plan from the
// embedded manifest (text or JSON), writes nothing, needs no input, and
// refuses an unlisted target; applying an upgrade is not offered yet.
func TestUpgradeDryRun(t *testing.T) {
	m, err := upgrade.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	latest := m.Latest()
	goMod := "module example.com/demo\n\ngo 1.26\n\nrequire github.com/gombit-dev/gombit " + latest + "\n"
	dir := t.TempDir()
	t.Setenv("GOWORK", "off")
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runUpgradeCLI(t, "--dry-run", "--dir", dir)
	if err != nil || !strings.Contains(out, "Target:  "+latest) || !strings.Contains(out, "nothing to upgrade") {
		t.Fatalf("upgrade --dry-run = %q, %v", out, err)
	}
	out, err = runUpgradeCLI(t, "--dry-run", "--dir", dir, "--to", latest, "--json")
	var plan upgrade.Plan
	if err != nil || json.Unmarshal([]byte(out), &plan) != nil || plan.Current != latest || plan.Target != latest || plan.Dependencies == nil {
		t.Fatalf("upgrade --dry-run --json = %s, %v", out, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("the dry run left %d entries in the app (want only go.mod): %v", len(entries), err)
	}

	for _, tc := range []struct {
		args []string
		msg  string
	}{
		{[]string{"--dry-run", "--dir", dir, "--to", "v99.0.0"}, "upgrade the gombit CLI"},
		{[]string{"--to", latest, "--dir", dir}, "applying an upgrade is not supported yet"},
		{[]string{"--dry-run", "--dir", t.TempDir()}, "not a Gombit application"},
		{[]string{"nonsense"}, `unknown subcommand "nonsense"`},
	} {
		if _, err := runUpgradeCLI(t, tc.args...); err == nil || !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("upgrade %v = %v; want an error mentioning %q", tc.args, err, tc.msg)
		}
	}
}
