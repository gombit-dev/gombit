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

// policyOf parses a manifest of releases and judges its last named release
// by the policy.
func policyOf(t *testing.T, releases string) error {
	t.Helper()
	m, err := upgrade.ParseManifest([]byte("format: 1\nreleases:\n" + releases))
	if err != nil {
		t.Fatalf("the manifest does not parse: %v", err)
	}
	return m.CheckPolicy(m.Latest())
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

// TestCheckPolicy: a release that is a smaller version change than its
// changes need is refused, naming the release the policy needs; the
// security exception passes.
func TestCheckPolicy(t *testing.T) {
	security := "      - {id: %s, kind: manual, breaking: true, area: security, summary: s, details: d}\n"
	err := policyOf(t, rel("v0.8.0")+rel("v0.8.1", change(breakingAPI, "patch-break")))
	if err == nil || !strings.Contains(err.Error(), "release v0.8.1 is a patch release after v0.8.0, but ships breaking changes (patch-break)") || !strings.Contains(err.Error(), "needs a minor release (v0.9.0)") {
		t.Errorf("a breaking patch: %v", err)
	}
	if err := policyOf(t, rel("v0.8.0")+rel("v0.8.1", change(security, "fix-hole"))); err != nil {
		t.Errorf("a breaking security patch (the exception): %v", err)
	}
	if err := policyOf(t, rel("v0.8.0")+rel("v0.9.0", change(breakingAPI, "minor-break"))); err != nil {
		t.Errorf("a breaking minor before 1.0: %v", err)
	}
	err = policyOf(t, rel("v1.0.0")+rel("v1.1.0", change(breakingAPI, "minor-break")))
	if err == nil || !strings.Contains(err.Error(), "needs a major release (v2.0.0)") {
		t.Errorf("a breaking minor from 1.0: %v", err)
	}
	if err := policyOf(t, rel("v1.0.0")+rel("v2.0.0", change(breakingAPI, "major-break"))); err != nil {
		t.Errorf("a breaking major: %v", err)
	}
	if err := policyOf(t, rel("v0.8.0")); err != nil {
		t.Errorf("the first release: %v", err)
	}
}

// TestPublishedReleaseCanBeCorrected: a published patch found to have
// broken something is recorded truthfully (breaking: true). The manifest
// still parses, so every gombit upgrade command keeps working; only the
// policy check, the release gate's and the repository test's, refuses it
// until an exception says why it stands.
func TestPublishedReleaseCanBeCorrected(t *testing.T) {
	m, err := upgrade.ParseManifest([]byte("format: 1\nreleases:\n" + rel("v0.8.0") + rel("v0.8.1", change(breakingAPI, "found-to-break")) + rel("v0.8.2")))
	if err != nil {
		t.Fatalf("a corrected published release does not parse: %v", err)
	}
	if err := m.CheckPolicy("v0.8.1"); err == nil {
		t.Fatal("the policy passed a breaking patch")
	}
	if err := m.CheckPolicy("v0.8.2"); err != nil {
		t.Fatalf("the release after it: %v", err)
	}
}

// TestEmbeddedManifestFollowsThePolicy: every release the manifest lists
// is the version change its changes need, or a recorded exception.
func TestEmbeddedManifestFollowsThePolicy(t *testing.T) {
	m, err := upgrade.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range m.Releases {
		if r.Version == upgrade.Unreleased {
			continue
		}
		if err := m.CheckPolicy(r.Version); err != nil {
			t.Error(err)
		}
	}
}

// TestReleaseGateNamesTheRelease: the gate, asked for a release the
// manifest does not list while unreleased declares breaking changes, says
// which release the policy needs (the 2026-10-14 v0.7.2 dispatch), after
// the latest stable release, as the policy judges it: a listed pre-release
// is not one. Asked for a listed release, it applies the policy.
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
	m, err = upgrade.ParseManifest([]byte("format: 1\nreleases:\n" + rel("v0.8.0") + rel("v0.9.0-rc.1", change(breakingAPI, "rc-break")) + rel("unreleased", change(breakingAPI, "final-break"))))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CheckRelease("v0.9.0"); err == nil || !strings.Contains(err.Error(), "needs a minor release after v0.8.0: v0.9.0") {
		t.Fatalf("CheckRelease(v0.9.0) with an rc listed = %v", err)
	}
	m, err = upgrade.ParseManifest([]byte("format: 1\nreleases:\n" + rel("v0.8.0") + rel("v0.8.1", change(breakingAPI, "patch-break"))))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CheckRelease("v0.8.1"); err == nil || !strings.Contains(err.Error(), "needs a minor release (v0.9.0)") {
		t.Fatalf("CheckRelease(v0.8.1), a breaking patch = %v", err)
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
	if len(uses) != 1 || uses[0].Since != "v0.8.0" || uses[0].RemovableFrom != "v0.10.0" || uses[0].RemovableFromMajor != "v1.0.0" {
		t.Fatalf("uses = %+v", uses)
	}
	var out strings.Builder
	if err := upgrade.RenderDeprecated(&out, uses); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "(deprecated in v0.8.0; the compatibility policy lets v0.10.0 (or v1.0.0, if it comes first) remove it)") {
		t.Fatalf("report:\n%s", out.String())
	}
}

// TestReleaseShipsItsPreReleases: a release ships the changes of every
// pre-release listed since the stable release before it, so a breaking
// change in v0.8.1-rc.1 makes v0.8.1 a breaking patch, at the gate and in
// its hint.
func TestReleaseShipsItsPreReleases(t *testing.T) {
	m, err := upgrade.ParseManifest([]byte("format: 1\nreleases:\n" + rel("v0.8.0") + rel("v0.8.1-rc.1", change(breakingAPI, "rc-break")) + rel("v0.8.1")))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CheckRelease("v0.8.1"); err == nil || !strings.Contains(err.Error(), "ships breaking changes (rc-break in v0.8.1-rc.1)") {
		t.Fatalf("CheckRelease(v0.8.1) after a breaking rc = %v", err)
	}
	m, err = upgrade.ParseManifest([]byte("format: 1\nreleases:\n" + rel("v0.8.0") + rel("v0.8.1-rc.1", change(breakingAPI, "rc-break")) + rel("unreleased", "      - {id: small, kind: informational, area: behavior, summary: s}\n")))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CheckRelease("v0.8.1"); err == nil || !strings.Contains(err.Error(), "ships breaking changes, rc-break in v0.8.1-rc.1, so the compatibility policy needs a minor release after v0.8.0: v0.9.0") {
		t.Fatalf("the hint after a breaking rc = %v", err)
	}
}

// TestFirstMajorFollowsAQuietMinor: v1.0.0 may remove what 0.y deprecated,
// so the gate refuses it right after a minor line that deprecates
// something; another minor first gives the deprecation its window.
func TestFirstMajorFollowsAQuietMinor(t *testing.T) {
	deprecation := "      - {id: %s, kind: informational, area: api, summary: s, deprecates: [db.Old]}\n"
	m, err := upgrade.ParseManifest([]byte("format: 1\nreleases:\n" + rel("v0.8.0") + rel("v0.9.0") + rel("v0.9.1", change(deprecation, "old-deprecated")) + rel("v1.0.0", change(breakingAPI, "one-oh"))))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CheckRelease("v1.0.0"); err == nil || !strings.Contains(err.Error(), "v1.0.0 follows v0.9.1, which deprecates db.Old") {
		t.Fatalf("CheckRelease(v1.0.0) after a deprecating minor = %v", err)
	}
	// A deprecation in a pre-release of a minor that never went stable.
	m, err = upgrade.ParseManifest([]byte("format: 1\nreleases:\n" + rel("v0.8.0") + rel("v0.9.0") + rel("v0.10.0-rc.1", change(deprecation, "old-deprecated")) + rel("v1.0.0", change(breakingAPI, "one-oh"))))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CheckRelease("v1.0.0"); err == nil || !strings.Contains(err.Error(), "v1.0.0 follows v0.10.0-rc.1, which deprecates db.Old") {
		t.Fatalf("CheckRelease(v1.0.0) after a deprecating rc = %v", err)
	}
	// A deprecation in v1.0.0's own pre-release is a 1.x one (removable
	// from v2.0.0): it does not hold v1.0.0 back.
	m, err = upgrade.ParseManifest([]byte("format: 1\nreleases:\n" + rel("v0.8.0") + rel("v0.9.0") + rel("v1.0.0-rc.1", change(deprecation, "old-deprecated")) + rel("v1.0.0", change(breakingAPI, "one-oh"))))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CheckRelease("v1.0.0"); err != nil {
		t.Fatalf("CheckRelease(v1.0.0) after its own deprecating rc = %v", err)
	}
	m, err = upgrade.ParseManifest([]byte("format: 1\nreleases:\n" + rel("v0.8.0") + rel("v0.9.0", change(deprecation, "old-deprecated")) + rel("v0.10.0") + rel("v1.0.0", change(breakingAPI, "one-oh"))))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CheckRelease("v1.0.0"); err != nil {
		t.Fatalf("CheckRelease(v1.0.0) after a quiet minor = %v", err)
	}
	if _, err := upgrade.ParseManifest([]byte("format: 1\nreleases:\n" + rel("v0.8.0") + rel("v0.9.0", change("      - {id: %s, kind: informational, area: api, summary: s, deprecates: [Old]}\n", "bad")))); err == nil || !strings.Contains(err.Error(), "want an API") {
		t.Fatalf("a deprecates entry that is not pkg.Name = %v", err)
	}
}

// TestPolicyJudgesAgainstTheLastStableRelease: a release after its own
// pre-release is judged against the stable release before both, so
// v0.9.0 after v0.9.0-rc.1 is a minor release after v0.8.0.
func TestPolicyJudgesAgainstTheLastStableRelease(t *testing.T) {
	if err := policyOf(t, rel("v0.8.0")+rel("v0.9.0-rc.1", change(breakingAPI, "rc-break"))+rel("v0.9.0", change(breakingAPI, "final-break"))); err != nil {
		t.Fatalf("a minor after its rc: %v", err)
	}
	err := policyOf(t, rel("v0.8.0")+rel("v0.8.1-rc.1")+rel("v0.8.1", change(breakingAPI, "patch-break")))
	if err == nil || !strings.Contains(err.Error(), "patch release after v0.8.0") {
		t.Fatalf("a breaking patch after its rc: %v", err)
	}
	if got := strings.Join(upgrade.UpcomingReleases("v0.8.3"), ","); got != "v0.8.4,v0.9.0,v1.0.0" {
		t.Fatalf("UpcomingReleases = %s", got)
	}
}
