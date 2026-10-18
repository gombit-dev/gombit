package upgrade_test

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/mod/semver"

	"github.com/gombit-dev/gombit/upgrade"
)

const planFixture = `format: 1
releases:
  - version: v0.6.1
  - version: v0.7.0
    changes:
      - id: record-upgrade-baseline
        kind: automatic
        action: record-baseline
        area: scaffold
        summary: Record the baseline.
      - id: rename-setting
        kind: manual
        breaking: true
        area: config
        summary: A setting is renamed.
        details: |
          Rename GOMBIT_OLD to GOMBIT_NEW.
        config:
          - {var: GOMBIT_OLD, change: renamed, to: GOMBIT_NEW}
      - id: faster-lists
        kind: informational
        area: behavior
        summary: Lists are faster.
  - version: v0.8.0
    changes:
      - id: review-timeouts
        kind: manual
        area: behavior
        summary: Review the shutdown timeout.
        details: Check the orchestrator's grace period.
`

const planGoMod = "module example.com/demo\n\ngo 1.26\n\nrequire github.com/gombit-dev/gombit v0.6.1\n"

func planManifest(t *testing.T) *upgrade.Manifest {
	t.Helper()
	m, err := upgrade.ParseManifest([]byte(planFixture))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func ids(cs []upgrade.Change) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.ID)
	}
	return out
}

// snapshot is every file under dir with its content and modification time.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path) // #nosec G304 G122 -- under t.TempDir(), no symlinks
		if err != nil {
			return err
		}
		out[path] = info.ModTime().Format(time.RFC3339Nano) + "\x00" + string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestPlanUpgrade: the plan reports the dependency change, the automatic
// changes the app needs, every manual action, the breaking and the
// informational changes, from every release between current and target.
func TestPlanUpgrade(t *testing.T) {
	m := planManifest(t)
	dir := app(t, planGoMod, "name: demo\n") // no upgrade metadata yet
	before := snapshot(t, dir)

	p, err := upgrade.PlanUpgrade(dir, m, "v0.8.0")
	if err != nil {
		t.Fatal(err)
	}
	if p.Current != "v0.6.1" || p.Target != "v0.8.0" || len(p.Releases) != 2 {
		t.Fatalf("plan = %s -> %s over %d releases", p.Current, p.Target, len(p.Releases))
	}
	if len(p.Dependencies) != 1 || p.Dependencies[0] != (upgrade.DependencyChange{Module: upgrade.FrameworkModulePath, Directive: "require", From: "v0.6.1", To: "v0.8.0"}) {
		t.Fatalf("Dependencies = %+v", p.Dependencies)
	}
	if len(p.Automatic) != 1 || p.Automatic[0].Change.ID != "record-upgrade-baseline" || p.Automatic[0].Description == "" {
		t.Fatalf("Automatic = %+v; want record-upgrade-baseline, which this app needs", p.Automatic)
	}
	if got := strings.Join(ids(p.Manual), ","); got != "rename-setting,review-timeouts" {
		t.Fatalf("Manual = %s", got)
	}
	if got := strings.Join(ids(p.Breaking), ","); got != "rename-setting" {
		t.Fatalf("Breaking = %s", got)
	}
	if got := strings.Join(ids(p.Informational), ","); got != "faster-lists" {
		t.Fatalf("Informational = %s", got)
	}
	if f := p.Config.Findings; len(f) != 1 || f[0].Var != "GOMBIT_OLD" || f[0].Category != upgrade.ConfigCategoryRenamed || !f[0].NeedsAction || len(p.Config.Files) != 0 {
		t.Fatalf("Config = %+v", p.Config)
	}

	var out strings.Builder
	if err := p.Render(&out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Current: v0.6.1\nTarget:  v0.8.0\n",
		"  github.com/gombit-dev/gombit v0.6.1 -> v0.8.0 (require)\n",
		"Automatic changes available: 1\n  - record-upgrade-baseline: Record the baseline.\n",
		"Manual actions: 2\n  - rename-setting [breaking]: A setting is renamed.\n      Rename GOMBIT_OLD to GOMBIT_NEW.\n",
		"  - review-timeouts: Review the shutdown timeout.\n      Check the orchestrator's grace period.\n",
		"Breaking behavior changes: 1\n  - rename-setting (manual): A setting is renamed.\n",
		"Informational: 1\n  - faster-lists: Lists are faster.\n",
		"Configuration changes: 1\n  - [renamed] `GOMBIT_OLD` is renamed `GOMBIT_NEW` (v0.7.0, rename-setting)\n",
		"Environment files read: none",
		"No files changed.\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("plan output lacks %q:\n%s", want, out.String())
		}
	}

	// A dry run writes nothing.
	after := snapshot(t, dir)
	if len(after) != len(before) {
		t.Fatalf("the plan changed the file set: %d files before, %d after", len(before), len(after))
	}
	for path, v := range before {
		if after[path] != v {
			t.Fatalf("the plan changed %s", path)
		}
	}
}

// TestPlanUpgradeSkipsAutomaticChangesNotNeeded: an app that already
// records its baseline does not count record-baseline.
func TestPlanUpgradeSkipsAutomaticChangesNotNeeded(t *testing.T) {
	dir := app(t, planGoMod, "name: demo\n"+upgrade.MetadataBlock(upgrade.ScaffoldVersion))
	p, err := upgrade.PlanUpgrade(dir, planManifest(t), "v0.7.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Automatic) != 0 || len(p.Manual) != 1 {
		t.Fatalf("plan = %d automatic, %d manual; want 0 automatic (the baseline is recorded), 1 manual", len(p.Automatic), len(p.Manual))
	}
}

// TestPlanUpgradeTarget: with no target, the newest release the manifest
// lists; at the target already, nothing to upgrade.
func TestPlanUpgradeTarget(t *testing.T) {
	m := planManifest(t)
	p, err := upgrade.PlanUpgrade(app(t, planGoMod, ""), m, "")
	if err != nil || p.Target != "v0.8.0" {
		t.Fatalf("PlanUpgrade with no target = %s, %v; want v0.8.0", p.Target, err)
	}
	at := app(t, strings.Replace(planGoMod, "v0.6.1", "v0.8.0", 1), "")
	p, err = upgrade.PlanUpgrade(at, m, "v0.8.0")
	if err != nil || len(p.Dependencies) != 0 || len(p.Releases) != 0 || len(p.Automatic) != 0 {
		t.Fatalf("PlanUpgrade at the target = %+v, %v; want an empty plan", p, err)
	}
	var out strings.Builder
	if err := p.Render(&out); err != nil || !strings.Contains(out.String(), "Already at v0.8.0: nothing to upgrade.") {
		t.Fatalf("Render at the target = %q, %v", out.String(), err)
	}
}

// TestPlanUpgradeRefuses: a version the manifest does not list, a
// downgrade, or a baseline that names no framework release fails with
// ErrNoUpgradePath and the reason.
func TestPlanUpgradeRefuses(t *testing.T) {
	m := planManifest(t)
	for name, tc := range map[string]struct{ goMod, to, msg string }{
		"unlisted target": {planGoMod, "v0.9.0", "newer than this gombit knows"},
		"downgrade":       {strings.Replace(planGoMod, "v0.6.1", "v0.8.0", 1), "v0.7.0", "downgrades are not supported"},
		"local checkout":  {planGoMod + "replace github.com/gombit-dev/gombit => ../gombit\n", "v0.8.0", "the local directory ../gombit"},
		"fork":            {planGoMod + "replace github.com/gombit-dev/gombit => github.com/me/gombit v0.7.0\n", "v0.8.0", "(a fork)"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := upgrade.PlanUpgrade(app(t, tc.goMod, ""), m, tc.to); !errors.Is(err, upgrade.ErrNoUpgradePath) || !strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("PlanUpgrade = %v; want ErrNoUpgradePath mentioning %q", err, tc.msg)
			}
		})
	}
	if _, err := upgrade.PlanUpgrade(t.TempDir(), m, ""); !errors.Is(err, upgrade.ErrNotGombitApp) {
		t.Fatalf("PlanUpgrade outside an app = %v", err)
	}
}

// TestPlanUpgradeReplaces: the plan resolves every replace of the
// framework at the target, as the go command would once the require moves,
// and reports what keeps the upgraded build from being the target release:
// an active replace, a dormant replace of the target version that would
// start to apply, and a replace of every version an exact one shadows today.
func TestPlanUpgradeReplaces(t *testing.T) {
	m := planManifest(t)
	const fw = upgrade.FrameworkModulePath
	const require060 = "module example.com/demo\n\ngo 1.26\n\nrequire github.com/gombit-dev/gombit v0.6.0\n"
	require := func(from string) upgrade.DependencyChange {
		return upgrade.DependencyChange{Module: fw, Directive: "require", From: from, To: "v0.8.0"}
	}
	for name, tc := range map[string]struct {
		goMod  string
		want   []upgrade.DependencyChange
		render []string
	}{
		"a self-replace of every version moves": {
			require060 + "replace github.com/gombit-dev/gombit => github.com/gombit-dev/gombit v0.6.1\n",
			[]upgrade.DependencyChange{
				require("v0.6.0"),
				{Module: fw, Directive: "replace", From: fw + " v0.6.1", To: "v0.8.0", Note: "it replaces every version of the framework, so it would keep the build on v0.6.1 (move it, or drop it)"},
			},
			[]string{"  replace github.com/gombit-dev/gombit => github.com/gombit-dev/gombit v0.6.1 -> v0.8.0\n"},
		},
		"a self-replace of the required version is dropped": {
			require060 + "replace github.com/gombit-dev/gombit v0.6.0 => github.com/gombit-dev/gombit v0.6.1\n",
			[]upgrade.DependencyChange{
				require("v0.6.0"),
				{Module: fw, Directive: "replace", ForVersion: "v0.6.0", From: fw + " v0.6.1", Note: "it replaces v0.6.0 only, so it stops applying once the require moves: drop it"},
			},
			[]string{"  replace github.com/gombit-dev/gombit v0.6.0 => github.com/gombit-dev/gombit v0.6.1: drop it\n"},
		},
		// Inactive today, it would decide the build once the require is at
		// v0.8.0.
		"a dormant replace of the target version is dropped": {
			planGoMod + "replace github.com/gombit-dev/gombit v0.8.0 => ../gombit\n",
			[]upgrade.DependencyChange{
				require("v0.6.1"),
				{Module: fw, Directive: "replace", ForVersion: "v0.8.0", From: "../gombit", Note: "it replaces v0.8.0, so once the require is at v0.8.0 the build would use ../gombit, not the release: drop it"},
			},
			[]string{"  replace github.com/gombit-dev/gombit v0.8.0 => ../gombit: drop it\n"},
		},
		// The exact replace of v0.6.1 shadows the local one of every version
		// today; once the require moves, that one would apply.
		"a replace of every version an exact one shadows is dropped": {
			planGoMod + "replace github.com/gombit-dev/gombit v0.6.1 => github.com/gombit-dev/gombit v0.6.1\nreplace github.com/gombit-dev/gombit => ../every\n",
			[]upgrade.DependencyChange{
				require("v0.6.1"),
				{Module: fw, Directive: "replace", From: "../every", Note: "it replaces every version of the framework, so once the require is at v0.8.0 the build would use ../every, not the release: drop it"},
				{Module: fw, Directive: "replace", ForVersion: "v0.6.1", From: fw + " v0.6.1", Note: "it replaces v0.6.1 only, so it stops applying once the require moves: drop it"},
			},
			[]string{"  replace github.com/gombit-dev/gombit => ../every: drop it\n"},
		},
		// Dropping the exact replace of the target exposes the one of every
		// version, which moves.
		"dropping an exact replace exposes one of every version": {
			planGoMod + "replace github.com/gombit-dev/gombit v0.8.0 => ../next\nreplace github.com/gombit-dev/gombit => github.com/gombit-dev/gombit v0.6.1\n",
			[]upgrade.DependencyChange{
				require("v0.6.1"),
				{Module: fw, Directive: "replace", ForVersion: "v0.8.0", From: "../next", Note: "it replaces v0.8.0, so once the require is at v0.8.0 the build would use ../next, not the release: drop it"},
				{Module: fw, Directive: "replace", From: fw + " v0.6.1", To: "v0.8.0", Note: "it replaces every version of the framework, so it would keep the build on v0.6.1 (move it, or drop it)"},
			},
			nil,
		},
		"a replace that already names the target changes nothing": {
			planGoMod + "replace github.com/gombit-dev/gombit v0.8.0 => github.com/gombit-dev/gombit v0.8.0\n",
			[]upgrade.DependencyChange{require("v0.6.1")},
			nil,
		},
	} {
		t.Run(name, func(t *testing.T) {
			p, err := upgrade.PlanUpgrade(app(t, tc.goMod, ""), m, "v0.8.0")
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Dependencies) != len(tc.want) {
				t.Fatalf("Dependencies = %+v; want %+v", p.Dependencies, tc.want)
			}
			for i := range tc.want {
				if p.Dependencies[i] != tc.want[i] {
					t.Fatalf("Dependencies[%d] = %+v; want %+v", i, p.Dependencies[i], tc.want[i])
				}
			}
			var out strings.Builder
			if err := p.Render(&out); err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.render {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("Render lacks %q:\n%s", want, out.String())
				}
			}
		})
	}
}

// TestPlanUpgradeBreakingFollowsAutomatic: a breaking automatic change the
// app does not need is left out of Breaking as it is of Automatic, so the
// plan does not count a change the app has absorbed (and a CI gate on
// .breaking does not fail on it).
func TestPlanUpgradeBreakingFollowsAutomatic(t *testing.T) {
	data := strings.Replace(planFixture, "        action: record-baseline\n", "        action: record-baseline\n        breaking: true\n", 1)
	m, err := upgrade.ParseManifest([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	recorded := app(t, planGoMod, "name: demo\n"+upgrade.MetadataBlock(upgrade.ScaffoldVersion))
	p, err := upgrade.PlanUpgrade(recorded, m, "v0.7.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Automatic) != 0 || strings.Join(ids(p.Breaking), ",") != "rename-setting" {
		t.Fatalf("recorded app: Automatic = %d, Breaking = %v; want none, and only rename-setting", len(p.Automatic), ids(p.Breaking))
	}
	old := app(t, planGoMod, "name: demo\n")
	p, err = upgrade.PlanUpgrade(old, m, "v0.7.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Automatic) != 1 || strings.Join(ids(p.Breaking), ",") != "record-upgrade-baseline,rename-setting" {
		t.Fatalf("old app: Automatic = %d, Breaking = %v; want record-upgrade-baseline counted in both", len(p.Automatic), ids(p.Breaking))
	}
}

// published are the framework releases already tagged from the manifest's
// baseline on: the embedded manifest must list every one, or apps on it
// have no upgrade path. Add each new tag here when it is cut
// (TestEmbeddedManifestListsPublishedTags checks the repo's tags too).
var published = []string{"v0.6.1", "v0.7.0", "v0.7.1", "v0.8.0"}

// TestEmbeddedManifestPlansFromPublishedReleases: the manifest built into
// this gombit lists every published release, and plans an upgrade from
// each to the newest.
func TestEmbeddedManifestPlansFromPublishedReleases(t *testing.T) {
	m, err := upgrade.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range published {
		if _, err := m.Release(v); err != nil {
			t.Errorf("the embedded manifest does not list the published release %s: %v", v, err)
		}
	}
	latest := m.Latest()
	for _, r := range m.Releases {
		if r.Version == upgrade.Unreleased {
			continue
		}
		dir := app(t, strings.Replace(planGoMod, "v0.6.1", r.Version, 1), "")
		p, err := upgrade.PlanUpgrade(dir, m, "")
		if err != nil {
			t.Fatalf("plan from %s: %v", r.Version, err)
		}
		if p.Target != latest {
			t.Fatalf("plan from %s targets %s, want the newest release %s", r.Version, p.Target, latest)
		}
		if r.Version != latest && len(p.Releases) == 0 {
			t.Fatalf("plan from %s to %s crosses no release", r.Version, latest)
		}
	}
}

// TestEmbeddedManifestListsPublishedTags: where the repository's tags are
// at hand (not in a shallow CI checkout), every stable release tag from the
// manifest's baseline on is listed.
func TestEmbeddedManifestListsPublishedTags(t *testing.T) {
	out, err := exec.Command("git", "tag", "--list", "v*").Output()
	if err != nil || len(strings.TrimSpace(string(out))) == 0 {
		t.Skip("no git tags here (a shallow checkout)")
	}
	m, err := upgrade.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	first := m.Releases[0].Version
	for _, tag := range strings.Fields(string(out)) {
		if !semver.IsValid(tag) || semver.Prerelease(tag) != "" || semver.Build(tag) != "" || semver.Compare(tag, first) < 0 {
			continue
		}
		if _, err := m.Release(tag); err != nil {
			t.Errorf("the release %s is tagged but not in upgrade/manifest.yaml", tag)
		}
	}
}

// TestPlanUpgradeContract: the plan tells the developer to check the
// generated contract artifacts after the dependency change, and names the
// path's contract (client area) changes.
func TestPlanUpgradeContract(t *testing.T) {
	m, err := upgrade.ParseManifest([]byte(`format: 1
releases:
  - version: v0.6.1
  - version: v0.7.0
    changes:
      - id: client-error-type
        kind: informational
        area: client
        summary: The generated client types errors more precisely.
`))
	if err != nil {
		t.Fatal(err)
	}
	p, err := upgrade.PlanUpgrade(app(t, planGoMod, ""), m, "v0.7.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Contract) != 1 || p.Contract[0].ID != "client-error-type" {
		t.Fatalf("Contract = %+v", p.Contract)
	}
	var out strings.Builder
	if err := p.Render(&out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"After the dependency change, check them: gombit upgrade contract", "Contract changes on this path: client-error-type."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("plan lacks %q:\n%s", want, out.String())
		}
	}
	p, err = upgrade.PlanUpgrade(app(t, planGoMod, ""), planManifest(t), "v0.8.0")
	if err != nil || len(p.Contract) != 0 {
		t.Fatalf("no contract changes: %+v, %v", p.Contract, err)
	}
}
