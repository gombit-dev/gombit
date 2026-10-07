package upgrade_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if len(p.Dependencies) != 1 || p.Dependencies[0] != (upgrade.DependencyChange{Module: upgrade.FrameworkModulePath, From: "v0.6.1", To: "v0.8.0"}) {
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

	var out strings.Builder
	if err := p.Render(&out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Current: v0.6.1\nTarget:  v0.8.0\n",
		"  github.com/gombit-dev/gombit v0.6.1 -> v0.8.0\n",
		"Automatic changes available: 1\n  - record-upgrade-baseline: Record the baseline.\n",
		"Manual actions: 2\n  - rename-setting [breaking]: A setting is renamed.\n      Rename GOMBIT_OLD to GOMBIT_NEW.\n",
		"  - review-timeouts: Review the shutdown timeout.\n      Check the orchestrator's grace period.\n",
		"Breaking behavior changes: 1\n  - rename-setting (manual): A setting is renamed.\n",
		"Informational: 1\n  - faster-lists: Lists are faster.\n",
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
