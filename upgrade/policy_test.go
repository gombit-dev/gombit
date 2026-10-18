package upgrade_test

import (
	"context"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/upgrade"
)

// TestRequiredBump: a breaking change needs a minor release before 1.0
// and a major one from 1.0 on; a breaking security change ships in any
// release (the security exception); nothing breaking, any release.
func TestRequiredBump(t *testing.T) {
	fix := upgrade.Change{ID: "fix", Area: "behavior"}
	breaking := upgrade.Change{ID: "breaks", Area: "api", Breaking: true}
	security := upgrade.Change{ID: "closes-hole", Area: "security", Breaking: true}
	for _, tc := range []struct {
		prev    string
		changes []upgrade.Change
		want    upgrade.Bump
	}{
		{"v0.8.0", []upgrade.Change{fix}, upgrade.BumpPatch},
		{"v0.8.0", []upgrade.Change{security}, upgrade.BumpPatch},
		{"v0.8.0", []upgrade.Change{fix, breaking}, upgrade.BumpMinor},
		{"v1.2.0", []upgrade.Change{breaking}, upgrade.BumpMajor},
		{"v1.2.0", []upgrade.Change{security}, upgrade.BumpPatch},
	} {
		got, ids := upgrade.RequiredBump(tc.prev, tc.changes)
		if got != tc.want {
			t.Errorf("RequiredBump(%s, %v) = %s, want %s", tc.prev, tc.changes, got, tc.want)
		}
		if (got != upgrade.BumpPatch) != (len(ids) > 0) {
			t.Errorf("RequiredBump(%s): breaking ids %v", tc.prev, ids)
		}
	}
}

func manifestOf(t *testing.T, releases string) error {
	t.Helper()
	_, err := upgrade.ParseManifest([]byte("format: 1\nreleases:\n" + releases))
	return err
}

const breakingAPI = "      - {id: %s, kind: manual, breaking: true, area: api, summary: s, details: d}\n"

func rel(version string, changes ...string) string {
	out := "  - version: " + version + "\n"
	if len(changes) > 0 {
		out += "    changes:\n" + strings.Join(changes, "")
	}
	return out
}

func change(format, id string) string { return strings.Replace(format, "%s", id, 1) }

// TestManifestFollowsThePolicy: a listed release from v0.8.0 on that is a
// smaller version change than its changes need is refused, naming the
// release the policy needs; the security exception and the releases
// before the policy pass.
func TestManifestFollowsThePolicy(t *testing.T) {
	security := "      - {id: %s, kind: manual, breaking: true, area: security, summary: s, details: d}\n"
	if err := manifestOf(t, rel("v0.7.0")+rel("v0.7.1", change(breakingAPI, "old-break"))); err != nil {
		t.Errorf("a release before the policy: %v", err)
	}
	err := manifestOf(t, rel("v0.8.0")+rel("v0.8.1", change(breakingAPI, "patch-break")))
	if err == nil || !strings.Contains(err.Error(), "release v0.8.1 is a patch release after v0.8.0, but declares breaking changes (patch-break)") || !strings.Contains(err.Error(), "needs a minor release (v0.9.0)") {
		t.Errorf("a breaking patch: %v", err)
	}
	if err := manifestOf(t, rel("v0.8.0")+rel("v0.8.1", change(security, "fix-hole"))); err != nil {
		t.Errorf("a breaking security patch (the exception): %v", err)
	}
	if err := manifestOf(t, rel("v0.8.0")+rel("v0.9.0", change(breakingAPI, "minor-break"))); err != nil {
		t.Errorf("a breaking minor before 1.0: %v", err)
	}
	err = manifestOf(t, rel("v1.0.0")+rel("v1.1.0", change(breakingAPI, "minor-break")))
	if err == nil || !strings.Contains(err.Error(), "needs a major release (v2.0.0)") {
		t.Errorf("a breaking minor from 1.0: %v", err)
	}
	if err := manifestOf(t, rel("v1.0.0")+rel("v2.0.0", change(breakingAPI, "major-break"))); err != nil {
		t.Errorf("a breaking major: %v", err)
	}
	// Unreleased is not judged until it is named.
	if err := manifestOf(t, rel("v0.8.0")+rel("unreleased", change(breakingAPI, "pending-break"))); err != nil {
		t.Errorf("unreleased: %v", err)
	}
}

// TestReleaseGateNamesTheRelease: the gate, asked for a release the
// manifest does not list while unreleased declares breaking changes, says
// which release the policy needs (the 2026-10-14 v0.7.2 dispatch).
func TestReleaseGateNamesTheRelease(t *testing.T) {
	m, err := upgrade.ParseManifest([]byte("format: 1\nreleases:\n" + rel("v0.7.0") + rel("v0.7.1") + rel("unreleased", change(breakingAPI, "big-change"))))
	if err != nil {
		t.Fatal(err)
	}
	err = m.CheckRelease("v0.7.2")
	if err == nil || !strings.Contains(err.Error(), "rename its unreleased entry to the release") || !strings.Contains(err.Error(), "needs a minor release after v0.7.1: v0.8.0") {
		t.Fatalf("CheckRelease(v0.7.2) = %v", err)
	}
	m, err = upgrade.ParseManifest([]byte("format: 1\nreleases:\n" + rel("v0.7.0") + rel("v0.7.1") + rel("unreleased", "      - {id: small, kind: informational, area: behavior, summary: s}\n")))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CheckRelease("v0.7.2"); err == nil || !strings.Contains(err.Error(), "rename its unreleased entry") || strings.Contains(err.Error(), "needs") {
		t.Fatalf("CheckRelease without breaking changes = %v", err)
	}
}

// TestRemovableFrom: before 1.0 a deprecated API stays through the next
// minor; from 1.0 on, until the next major.
func TestRemovableFrom(t *testing.T) {
	for since, want := range map[string]string{"v0.8.0": "v0.10.0", "v0.8.3": "v0.10.0", "v1.2.0": "v2.0.0", "v2.0.1": "v3.0.0"} {
		if got := upgrade.RemovableFrom(since); got != want {
			t.Errorf("RemovableFrom(%s) = %s, want %s", since, got, want)
		}
	}
	for paragraph, want := range map[string]string{
		"Deprecated: since v0.8.0, use Open.": "v0.8.0",
		"Deprecated: use Open.":               "",
		"Deprecated: since 0.8, use Open.":    "",
		"X: Deprecated: since v1.2.3 use Y":   "v1.2.3",
	} {
		if got := upgrade.DeprecationSince(paragraph); got != want {
			t.Errorf("DeprecationSince(%q) = %q, want %q", paragraph, got, want)
		}
	}
}

// TestScanDeprecatedShowsTheWindow: a use of an API whose deprecation
// names its release is reported with the first release the policy lets
// remove it.
func TestScanDeprecatedShowsTheWindow(t *testing.T) {
	app := scanApp(t, map[string]string{"main.go": "package main\n\nimport \"github.com/gombit-dev/gombit/db\"\n\nfunc main() { _ = db.Dated() }\n"})
	uses, err := upgrade.ScanDeprecated(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	if len(uses) != 1 || uses[0].Since != "v0.8.0" || uses[0].RemovableFrom != "v0.10.0" {
		t.Fatalf("uses = %+v", uses)
	}
	var out strings.Builder
	if err := upgrade.RenderDeprecated(&out, uses); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "(deprecated in v0.8.0; the compatibility policy lets v0.10.0 remove it)") {
		t.Fatalf("report:\n%s", out.String())
	}
}

// TestPolicyJudgesAgainstTheLastStableRelease: a release after its own
// pre-release is judged against the stable release before both, so
// v0.9.0 after v0.9.0-rc.1 is a minor release after v0.8.0.
func TestPolicyJudgesAgainstTheLastStableRelease(t *testing.T) {
	if err := manifestOf(t, rel("v0.8.0")+rel("v0.9.0-rc.1", change(breakingAPI, "rc-break"))+rel("v0.9.0", change(breakingAPI, "final-break"))); err != nil {
		t.Fatalf("a minor after its rc: %v", err)
	}
	err := manifestOf(t, rel("v0.8.0")+rel("v0.8.1-rc.1")+rel("v0.8.1", change(breakingAPI, "patch-break")))
	if err == nil || !strings.Contains(err.Error(), "patch release after v0.8.0") {
		t.Fatalf("a breaking patch after its rc: %v", err)
	}
	if got := strings.Join(upgrade.UpcomingReleases("v0.8.3"), ","); got != "v0.8.4,v0.9.0,v1.0.0" {
		t.Fatalf("UpcomingReleases = %s", got)
	}
}
