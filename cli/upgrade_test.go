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
	if _, err := runUpgradeCLI(t); err == nil || !strings.Contains(err.Error(), "--to (apply) or --dry-run (plan) or a subcommand is required") {
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
	// Applying to the release the app is at already changes nothing.
	if out, err := runUpgradeCLI(t, "--to", latest, "--dir", dir); err != nil || !strings.Contains(out, "Already at "+latest) {
		t.Fatalf("upgrade --to %s at %s = %q, %v", latest, latest, out, err)
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
		{[]string{"--dry-run", "--dir", t.TempDir()}, "not a Gombit application"},
		{[]string{"nonsense"}, `unknown subcommand "nonsense"`},
	} {
		if _, err := runUpgradeCLI(t, tc.args...); err == nil || !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("upgrade %v = %v; want an error mentioning %q", tc.args, err, tc.msg)
		}
	}
}

// TestUpgradeCheck: gombit upgrade check reports the uses of deprecated
// framework APIs (text or JSON) and exits non-zero when there are any.
func TestUpgradeCheck(t *testing.T) {
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "-mod=mod")
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	root := t.TempDir()
	files := map[string]string{
		"fw/go.mod":   "module github.com/gombit-dev/gombit\n\ngo 1.21\n",
		"fw/db/db.go": "package db\n\n// Old.\n//\n// Deprecated: use New.\nfunc Old() int { return 1 }\n\n// New.\nfunc New() int { return 2 }\n",
		"app/go.mod":  "module example.com/app\n\ngo 1.21\n\nrequire github.com/gombit-dev/gombit v0.0.0\n\nreplace github.com/gombit-dev/gombit => ../fw\n",
		"app/main.go": "package main\n\nimport \"github.com/gombit-dev/gombit/db\"\n\nfunc main() { _ = db.Old() }\n",
		"ok/go.mod":   "module example.com/ok\n\ngo 1.21\n\nrequire github.com/gombit-dev/gombit v0.0.0\n\nreplace github.com/gombit-dev/gombit => ../fw\n",
		"ok/main.go":  "package main\n\nimport \"github.com/gombit-dev/gombit/db\"\n\nfunc main() { _ = db.New() }\n",
	}
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil { // #nosec G703 -- under t.TempDir()
			t.Fatal(err)
		}
	}
	out, err := runUpgradeCLI(t, "check", "--dir", filepath.Join(root, "app"))
	if err == nil || !strings.Contains(err.Error(), "1 use(s) of deprecated framework APIs") ||
		!strings.Contains(out, "main.go:5:22: db.Old\n      Deprecated: use New.") {
		t.Fatalf("upgrade check = %q, %v", out, err)
	}
	out, err = runUpgradeCLI(t, "check", "--dir", filepath.Join(root, "app"), "--json")
	var uses []upgrade.DeprecatedUse
	if err == nil || json.Unmarshal([]byte(out), &uses) != nil || len(uses) != 1 || uses[0].Symbol != "db.Old" {
		t.Fatalf("upgrade check --json = %s, %v", out, err)
	}
	out, err = runUpgradeCLI(t, "check", "--dir", filepath.Join(root, "ok"), "--json")
	if err != nil || strings.TrimSpace(out) != "[]" {
		t.Fatalf("upgrade check --json on a clean app = %q, %v", out, err)
	}
}

// TestUpgradeFix: gombit upgrade fix prints the codemods' patch on stdout
// and changes nothing; --patch writes it to a file, --write rewrites the
// files (and a second run has nothing to do); a use left for the developer
// is listed and fails the command; with no codemods it says so.
func TestUpgradeFix(t *testing.T) {
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "-mod=mod")
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	root := t.TempDir()
	const main = "package main\n\nimport \"github.com/gombit-dev/gombit/db\"\n\nfunc main() { _ = db.Old() }\n"
	files := map[string]string{
		"fw/go.mod":    "module github.com/gombit-dev/gombit\n\ngo 1.21\n",
		"fw/db/db.go":  "package db\n\n// Old.\n//\n// Deprecated: use New.\nfunc Old() int { return 1 }\n\n// New.\nfunc New() int { return 2 }\n",
		"app/go.mod":   "module example.com/app\n\ngo 1.21\n\nrequire github.com/gombit-dev/gombit v0.0.0\n\nreplace github.com/gombit-dev/gombit => ../fw\n",
		"app/main.go":  main,
		"app/gen.go":   "// Code generated by test. DO NOT EDIT.\n\npackage main\n\nimport \"github.com/gombit-dev/gombit/db\"\n\nvar _ = db.Old\n",
		"app2/go.mod":  "module example.com/app2\n\ngo 1.21\n\nrequire github.com/gombit-dev/gombit v0.0.0\n\nreplace github.com/gombit-dev/gombit => ../fw\n",
		"app2/main.go": main,
	}
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil { // #nosec G703 -- under t.TempDir()
			t.Fatal(err)
		}
	}
	readApp := func(name string) string {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name))) // #nosec G304 -- under t.TempDir()
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	run := func(args ...string) (string, string, error) {
		var stdout, stderr bytes.Buffer
		err := ExecuteRoot(context.Background(), NewRoot(&stdout, &stderr), append([]string{"upgrade", "fix"}, args...))
		return stdout.String(), stderr.String(), err
	}

	old := upgradeCodemods
	defer func() { upgradeCodemods = old }()
	upgradeCodemods = func() []upgrade.Codemod { return nil }
	if out, errOut, err := run("--dir", filepath.Join(root, "app")); err != nil || out != "" || !strings.Contains(errOut, "this gombit ships none") {
		t.Fatalf("no codemods: %q, %q, %v", out, errOut, err)
	}
	// --patch with no codemods (what this gombit ships) still empties the
	// file: no stale patch survives.
	stalePatch := filepath.Join(root, "none.patch")
	if err := os.WriteFile(stalePatch, []byte("old patch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, _, err := run("--dir", filepath.Join(root, "app"), "--patch", stalePatch); err != nil || !strings.Contains(out, "ships none") || !strings.Contains(out, stalePatch+" is empty.") || readApp("none.patch") != "" {
		t.Fatalf("no codemods, --patch: %q, %v (file %q)", out, err, readApp("none.patch"))
	}
	if out, _, err := run("--dir", filepath.Join(root, "app"), "--json"); err != nil || !strings.Contains(out, `"codemods": []`) {
		t.Fatalf("no codemods, --json: %q, %v", out, err)
	}

	upgradeCodemods = func() []upgrade.Codemod {
		return []upgrade.Codemod{{ID: "old-to-new", Description: "db.Old is db.New", Package: "github.com/gombit-dev/gombit/db", Symbol: "Old", Rename: "New"}}
	}
	app := filepath.Join(root, "app")
	// Default: the patch on stdout, the summary on stderr, nothing written;
	// the generated file's use is left, so it fails.
	out, errOut, err := run("--dir", app)
	if err == nil || !strings.Contains(err.Error(), "1 use(s) left") {
		t.Fatalf("fix = %v", err)
	}
	if !strings.HasPrefix(out, "--- a/main.go\n+++ b/main.go\n") || !strings.Contains(out, "+func main() { _ = db.New() }\n") || strings.Contains(out, "Left") {
		t.Fatalf("stdout:\n%s", out)
	}
	if !strings.Contains(errOut, "nothing was changed") || !strings.Contains(errOut, "gen.go:7:12: github.com/gombit-dev/gombit/db.Old (old-to-new)") {
		t.Fatalf("stderr:\n%s", errOut)
	}
	if readApp("app/main.go") != main {
		t.Fatal("fix wrote without --write")
	}
	// --patch FILE.
	patchFile := filepath.Join(root, "upgrade.patch")
	if out, _, _ := run("--dir", app, "--patch", patchFile); !strings.Contains(out, "Wrote the patch for 1 file(s) to "+patchFile) {
		t.Fatalf("--patch: %s", out)
	}
	if got := readApp("upgrade.patch"); !strings.HasPrefix(got, "--- a/main.go\n") || readApp("app/main.go") != main {
		t.Fatalf("patch file:\n%s", got)
	}
	// --json.
	out, _, _ = run("--dir", app, "--json")
	var res upgradeFixResult
	if err := json.Unmarshal([]byte(out), &res); err != nil || len(res.Files) != 1 || len(res.Left) != 1 || !strings.Contains(res.Patch, "db.New()") || len(res.Written) != 0 {
		t.Fatalf("--json: %s (%v)", out, err)
	}
	// --write, then a second run with nothing to do.
	app2 := filepath.Join(root, "app2")
	if out, _, err := run("--dir", app2, "--write"); err != nil || !strings.Contains(out, "Rewrote 1 of 1 file(s):\n  main.go\n") {
		t.Fatalf("--write: %q, %v", out, err)
	}
	if got := readApp("app2/main.go"); got != strings.Replace(main, "db.Old()", "db.New()", 1) {
		t.Fatalf("written:\n%s", got)
	}
	if out, errOut, err := run("--dir", app2); err != nil || out != "" || !strings.Contains(errOut, "Nothing to rewrite.") {
		t.Fatalf("second run: %q, %q, %v", out, errOut, err)
	}
	// --patch with nothing to rewrite empties the file: no stale patch.
	stale := filepath.Join(root, "app2.patch")
	if err := os.WriteFile(stale, []byte("old patch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, _, err := run("--dir", app2, "--patch", stale); err != nil || !strings.Contains(out, "Nothing to rewrite; "+stale+" is empty.") || readApp("app2.patch") != "" {
		t.Fatalf("--patch, nothing to do: %q, %v", out, err)
	}
	// Refusals.
	for _, tc := range []struct {
		args []string
		msg  string
	}{
		{[]string{"--dir", app, "--write", "--patch", "x"}, "not both"},
		{[]string{"--dir", app, "--only", "nope"}, `no codemod "nope"`},
	} {
		if _, _, err := run(tc.args...); err == nil || !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("fix %v = %v; want %q", tc.args, err, tc.msg)
		}
	}
}

// TestUpgradeConfig: gombit upgrade config reports the configuration
// changes between the app's framework version and the target, from the
// built-in manifest, exits 1 while one needs review (the production
// refusal of trust-all GOMBIT_HTTP_TRUSTED_PROXIES is a security change),
// and never prints a value from the app's environment files.
func TestUpgradeConfig(t *testing.T) {
	t.Setenv("GOWORK", "off")
	dir := t.TempDir()
	for name, content := range map[string]string{
		"go.mod":       "module example.com/demo\n\ngo 1.26\n\nrequire github.com/gombit-dev/gombit v0.6.1\n",
		".env.example": "GOMBIT_HTTP_TRUSTED_PROXIES=10.0.0.1\n",
		".env":         "GOMBIT_JWT_SECRET=do-not-print-this-value\nGOMBIT_HTTP_TRUSTED_PROXIES=0.0.0.0/0\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out, err := runUpgradeCLI(t, "config", "--dir", dir, "--to", "v0.7.1")
	if err == nil || !strings.Contains(err.Error(), "need action or review") {
		t.Fatalf("upgrade config = %v", err)
	}
	for _, want := range []string{
		"Upgrade v0.6.1 -> v0.7.1\n",
		"[security review] `GOMBIT_HTTP_TRUSTED_PROXIES`: some values are now refused in production (v0.7.0, trusted-proxies-trust-all)",
		"against trusted-proxies-trust-all (in .env, .env.example and the deployment's environment)",
		".env (names only), .env.example (template)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	for _, secret := range []string{"do-not-print-this-value", "0.0.0.0/0", "10.0.0.1"} {
		if strings.Contains(out, secret) {
			t.Fatalf("printed a value from an environment file (%s):\n%s", secret, out)
		}
	}
	out, _ = runUpgradeCLI(t, "config", "--dir", dir, "--to", "v0.7.1", "--json")
	var res upgradeConfigResult
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.From != "v0.6.1" || len(res.Findings) != 1 || res.Findings[0].Category != upgrade.ConfigCategorySecurityReview || strings.Contains(out, "do-not-print") {
		t.Fatalf("--json: %s (%v)", out, err)
	}
	// --write: nothing to rename on this path; still a review.
	before := map[string]string{}
	for _, name := range []string{".env", ".env.example"} {
		data, _ := os.ReadFile(filepath.Join(dir, name)) // #nosec G304 -- under t.TempDir()
		before[name] = string(data)
	}
	if out, err := runUpgradeCLI(t, "config", "--dir", dir, "--to", "v0.7.1", "--write"); err == nil || !strings.Contains(out, "No template needed upgrading.") {
		t.Fatalf("--write: %q, %v", out, err)
	}
	for name, content := range before {
		if data, _ := os.ReadFile(filepath.Join(dir, name)); string(data) != content { // #nosec G304 -- under t.TempDir()
			t.Fatalf("%s changed", name)
		}
	}
	// From past the change: nothing to do, exit 0.
	if out, err := runUpgradeCLI(t, "config", "--dir", dir, "--from", "v0.7.0", "--to", "v0.7.1"); err != nil || !strings.Contains(out, "Configuration changes: 0\n") {
		t.Fatalf("--from v0.7.0: %q, %v", out, err)
	}
	if _, err := runUpgradeCLI(t, "config", "--dir", dir, "--from", "v0.0.1"); err == nil {
		t.Fatal("an unlisted --from was accepted")
	}
}
