package upgrade_test

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/upgrade"
)

var update = flag.Bool("update", false, "rewrite docs/upgrade-notes.md from the manifest")

// TestEmbeddedManifest: the manifest this framework ships is valid, and
// describes releases.
func TestEmbeddedManifest(t *testing.T) {
	m, err := upgrade.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	if m.Format != upgrade.ManifestFormat || m.Latest() == "" {
		t.Fatalf("manifest format %d, latest %q", m.Format, m.Latest())
	}
	if _, err := m.Release(m.Latest()); err != nil {
		t.Fatal(err)
	}
}

// TestUpgradeNotesDoc: docs/upgrade-notes.md is the manifest rendered,
// newest release first. Regenerate with -update.
func TestUpgradeNotesDoc(t *testing.T) {
	m, err := upgrade.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := upgrade.RenderNotesDoc(&b, m); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("..", "docs", "upgrade-notes.md")
	if *update {
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil { // #nosec G306 -- a committed doc
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path) // #nosec G304 -- the committed doc
	if err != nil {
		t.Fatal(err)
	}
	// A Windows checkout may convert the doc to CRLF; its content is what
	// counts.
	if strings.ReplaceAll(string(got), "\r\n", "\n") != b.String() {
		t.Fatalf("docs/upgrade-notes.md is stale: regenerate it with\n  go test ./upgrade -run TestUpgradeNotesDoc -update")
	}
}

const fixture = `format: 1
releases:
  - version: v0.6.1
  - version: v0.7.0
    changes:
      - id: rename-option
        kind: automatic
        action: record-baseline
        area: api
        summary: Option renamed.
        refs: [10]
      - id: review-fk
        kind: manual
        breaking: true
        area: schema
        summary: Review FK delete semantics.
        details: |
          Check every relation.

          Then migrate.
      - id: faster
        kind: informational
        area: behavior
        summary: Faster.
  - version: v0.7.1
  - version: v0.8.0
    changes:
      - id: cookie-secure
        kind: manual
        area: config
        summary: Cookie setting renamed.
        details: Rename it.
  - version: unreleased
    changes:
      - id: next
        kind: informational
        area: cli
        summary: Next.
`

func fixtureManifest(t *testing.T) *upgrade.Manifest {
	t.Helper()
	m, err := upgrade.ParseManifest([]byte(fixture))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func versions(releases []upgrade.Release) []string {
	var vs []string
	for _, r := range releases {
		vs = append(vs, r.Version)
	}
	return vs
}

func TestManifestPath(t *testing.T) {
	m := fixtureManifest(t)
	if m.Latest() != "v0.8.0" {
		t.Fatalf("Latest = %s", m.Latest())
	}
	for _, tc := range []struct {
		from, to string
		want     []string
	}{
		{"v0.6.1", "v0.8.0", []string{"v0.7.0", "v0.7.1", "v0.8.0"}},
		{"v0.7.0", "v0.7.1", []string{"v0.7.1"}},
		{"v0.7.1", "v0.7.1", nil},
	} {
		got, err := m.Path(tc.from, tc.to)
		if err != nil || !slices.Equal(versions(got), tc.want) {
			t.Fatalf("Path(%s, %s) = %v, %v; want %v", tc.from, tc.to, versions(got), err, tc.want)
		}
	}
	for _, tc := range []struct{ from, to, msg string }{
		{"v0.5.0", "v0.8.0", "older than v0.6.1, the first release"},
		{"v0.6.1", "v0.9.0", "newer than this gombit knows (its manifest covers v0.6.1 to v0.8.0); upgrade the gombit CLI"},
		// A pseudo-version or pre-release is classified before it is
		// placed: newer than the newest release (the first commit after it,
		// as on @main), it is still not a reason to upgrade the CLI.
		{"v0.7.2-0.20261001120000-abcdefabcdef", "v0.8.0", "is a pseudo-version (an untagged commit)"},
		{"v0.8.1-0.20261001120000-abcdefabcdef", "v0.8.0", "is a pseudo-version (an untagged commit)"},
		{"v0.6.1", "v0.9.0-rc.1", "is a pre-release the compatibility manifest does not list"},
		{"v0.6.1", "v0.7.5", "within the releases the compatibility manifest covers (v0.6.1 to v0.8.0) but missing from it"},
		{"v0.8.0", "v0.7.0", "downgrades are not supported"},
		{"", "v0.8.0", "no current version (a framework release) was given"},
		{"v0.6.1", "unreleased", "unreleased is not a release"},
		{"0.7.0", "v0.8.0", "not a semantic version"},
	} {
		if _, err := m.Path(tc.from, tc.to); !errors.Is(err, upgrade.ErrNoUpgradePath) || !strings.Contains(err.Error(), tc.msg) {
			t.Fatalf("Path(%q, %q) = %v; want ErrNoUpgradePath mentioning %q", tc.from, tc.to, err, tc.msg)
		}
	}
	if _, err := m.Release("v0.9.0"); !errors.Is(err, upgrade.ErrNoUpgradePath) {
		t.Fatalf("Release(v0.9.0) = %v", err)
	}
	if r, err := m.Release(upgrade.Unreleased); err != nil || len(r.Changes) != 1 {
		t.Fatalf("Release(unreleased) = %+v, %v", r, err)
	}
}

func TestClassify(t *testing.T) {
	m := fixtureManifest(t)
	path, err := m.Path("v0.6.1", "v0.8.0")
	if err != nil {
		t.Fatal(err)
	}
	c := upgrade.Classify(path)
	ids := func(cs []upgrade.Change) []string {
		var out []string
		for _, ch := range cs {
			out = append(out, ch.ID)
		}
		return out
	}
	if !slices.Equal(ids(c.Automatic), []string{"rename-option"}) ||
		!slices.Equal(ids(c.Manual), []string{"review-fk", "cookie-secure"}) ||
		!slices.Equal(ids(c.Informational), []string{"faster"}) ||
		!slices.Equal(ids(c.Breaking), []string{"review-fk"}) {
		t.Fatalf("Classify = %+v", c)
	}
}

func TestRenderNotes(t *testing.T) {
	m := fixtureManifest(t)
	var b strings.Builder
	if err := m.RenderNotes(&b, m.Releases[1:3]); err != nil {
		t.Fatal(err)
	}
	want := "## v0.7.0\n\n" +
		"### Manual: action required\n\n" +
		"- **Breaking.** Review FK delete semantics. (`review-fk`, schema)\n\n" +
		"  Check every relation.\n\n" +
		"  Then migrate.\n\n" +
		"### Automatic: applied by the upgrade tooling\n\n" +
		"- Option renamed. (`rename-option`, api, action `record-baseline`, [#10](https://github.com/gombit-dev/gombit/issues/10))\n\n" +
		"### Informational\n\n" +
		"- Faster. (`faster`, behavior)\n\n" +
		"## v0.7.1\n\n" +
		"No upgrade-relevant changes.\n"
	if b.String() != want {
		t.Fatalf("RenderNotes =\n%s\nwant\n%s", b.String(), want)
	}
	b.Reset()
	if err := m.RenderNotes(&b, m.Releases[:1]); err != nil || !strings.Contains(b.String(), "## v0.6.1\n\nThe first release the compatibility manifest covers") {
		t.Fatalf("baseline release notes = %q, %v", b.String(), err)
	}
	b.Reset()
	if err := m.RenderNotes(&b, m.Releases[4:]); err != nil || !strings.HasPrefix(b.String(), "## Unreleased\n") {
		t.Fatalf("unreleased notes = %q, %v", b.String(), err)
	}
}

func TestParseManifestRefuses(t *testing.T) {
	change := func(fields string) string {
		return "format: 1\nreleases:\n  - version: v0.1.0\n  - version: v0.2.0\n    changes:\n      - " + strings.ReplaceAll(strings.TrimSpace(fields), "\n", "\n        ") + "\n"
	}
	for name, tc := range map[string]struct{ data, msg string }{
		"newer format":             {"format: 2\nreleases:\n  - version: v0.1.0\n", "format 2"},
		"no releases":              {"format: 1\n", "no releases"},
		"unknown field":            {"format: 1\nreleases:\n  - version: v0.1.0\n    chnages: []\n", "chnages"},
		"not canonical":            {"format: 1\nreleases:\n  - version: v0.1\n", "canonical"},
		"build metadata":           {"format: 1\nreleases:\n  - version: v0.1.0+meta\n", "canonical"},
		"out of order":             {"format: 1\nreleases:\n  - version: v0.2.0\n  - version: v0.1.0\n", "ascending"},
		"repeated":                 {"format: 1\nreleases:\n  - version: v0.1.0\n  - version: v0.1.0\n", "ascending"},
		"unreleased not last":      {"format: 1\nreleases:\n  - version: v0.1.0\n  - version: unreleased\n  - version: v0.2.0\n", "must be the last"},
		"unreleased baseline":      {"format: 1\nreleases:\n  - version: unreleased\n", "the first release is the baseline"},
		"baseline changes":         {"format: 1\nreleases:\n  - version: v0.1.0\n    changes:\n      - {id: x, kind: informational, area: api, summary: a}\n", "the first release is the baseline"},
		"bad id":                   {change("id: Bad_ID\nkind: informational\narea: api\nsummary: x"), "kebab-case"},
		"unknown area":             {change("id: x\nkind: informational\narea: ui\nsummary: x"), "unknown area"},
		"unknown kind":             {change("id: x\nkind: breaking\narea: api\nsummary: x"), `kind "breaking"`},
		"no summary":               {change("id: x\nkind: informational\narea: api"), "summary"},
		"multiline summary":        {change("id: x\nkind: informational\narea: api\nsummary: \"a\\nb\""), "summary"},
		"manual no details":        {change("id: x\nkind: manual\narea: api\nsummary: x"), "needs details"},
		"automatic unknown action": {change("id: x\nkind: automatic\naction: rewrite-everything\narea: api\nsummary: x"), "not one this gombit implements"},
		"automatic no action":      {change("id: x\nkind: automatic\narea: api\nsummary: x"), "not one this gombit implements"},
		"action on manual":         {change("id: x\nkind: manual\naction: record-baseline\narea: api\nsummary: x\ndetails: y"), "only an automatic change"},
		"bad ref":                  {change("id: x\nkind: informational\narea: api\nsummary: x\nrefs: [0]"), "ref 0"},
		"duplicate id":             {"format: 1\nreleases:\n  - version: v0.1.0\n  - version: v0.2.0\n    changes:\n      - {id: x, kind: informational, area: api, summary: a}\n  - version: v0.3.0\n    changes:\n      - {id: x, kind: informational, area: api, summary: b}\n", "already declared by v0.2.0"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := upgrade.ParseManifest([]byte(tc.data)); err == nil || !strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("ParseManifest = %v; want an error mentioning %q", err, tc.msg)
			}
		})
	}
}

// TestLoadManifestCopies: each LoadManifest is the caller's own; changing
// one does not change the next.
func TestLoadManifestCopies(t *testing.T) {
	a, err := upgrade.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	a.Releases[0].Version = "v9.9.9"
	for i := range a.Releases {
		for j := range a.Releases[i].Changes {
			a.Releases[i].Changes[j].Summary = "changed"
		}
	}
	b, err := upgrade.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	if b.Releases[0].Version == "v9.9.9" {
		t.Fatal("a caller's change to the manifest reached the next LoadManifest")
	}
	for _, r := range b.Releases {
		for _, c := range r.Changes {
			if c.Summary == "changed" {
				t.Fatal("a caller's change to a change reached the next LoadManifest")
			}
		}
	}
}

func TestReleaseWithoutChangesIsAnEmptyList(t *testing.T) {
	r, err := fixtureManifest(t).Release("v0.7.1")
	if err != nil || r.Changes == nil {
		t.Fatalf("Release(v0.7.1) = %+v, %v; want an empty, non-nil change list", r, err)
	}
}

// TestCheckRelease: the release gate wants the tag listed, the newest
// release, and nothing left under unreleased; listing the tag alone is not
// enough.
func TestCheckRelease(t *testing.T) {
	parse := func(data string) *upgrade.Manifest {
		t.Helper()
		m, err := upgrade.ParseManifest([]byte(data))
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	const change = "      - {id: x, kind: informational, area: api, summary: s}\n"
	closed := parse("format: 1\nreleases:\n  - version: v0.1.0\n  - version: v0.2.0\n    changes:\n" + change)
	if err := closed.CheckRelease("v0.2.0"); err != nil {
		t.Fatalf("closed at v0.2.0: %v", err)
	}
	emptyUnreleased := parse("format: 1\nreleases:\n  - version: v0.1.0\n  - version: v0.2.0\n  - version: unreleased\n")
	if err := emptyUnreleased.CheckRelease("v0.2.0"); err != nil {
		t.Fatalf("an empty unreleased entry: %v", err)
	}
	for name, tc := range map[string]struct {
		m        *upgrade.Manifest
		tag, msg string
	}{
		"changes left under unreleased": {
			parse("format: 1\nreleases:\n  - version: v0.1.0\n  - version: v0.2.0\n  - version: unreleased\n    changes:\n" + change),
			"v0.2.0", "1 change(s) are still under unreleased",
		},
		"a newer release listed": {closed, "v0.1.0", "v0.1.0 is not the newest release it lists (v0.2.0 is)"},
		"unlisted":               {closed, "v0.3.0", "has no release v0.3.0"},
	} {
		if err := tc.m.CheckRelease(tc.tag); err == nil || !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("%s: CheckRelease(%s) = %v; want an error mentioning %q", name, tc.tag, err, tc.msg)
		}
	}
}

// TestPathFrom: planning from an application's baseline refuses one that
// names no framework release, with the reason, and is Path otherwise.
func TestPathFrom(t *testing.T) {
	m := fixtureManifest(t)
	const fw = upgrade.FrameworkModulePath
	got, err := m.PathFrom(upgrade.Framework{Module: fw, Version: "v0.7.0", Required: "v0.7.0"}, "v0.8.0")
	if err != nil || !slices.Equal(versions(got), []string{"v0.7.1", "v0.8.0"}) {
		t.Fatalf("PathFrom a release = %v, %v", versions(got), err)
	}
	for name, tc := range map[string]struct {
		fw  upgrade.Framework
		msg string
	}{
		"workspace": {upgrade.Framework{Module: fw, Required: "v0.7.0", Workspace: "/w/go.work"}, "a go.work (/w/go.work) decides"},
		"local":     {upgrade.Framework{Module: fw, Required: "v0.7.0", Replace: &upgrade.Replacement{Path: "../gombit"}, Local: true}, "the local directory ../gombit"},
		"fork":      {upgrade.Framework{Module: fw, Required: "v0.7.0", Replace: &upgrade.Replacement{Path: "github.com/me/gombit", Version: "v0.7.0"}}, "another module, github.com/me/gombit v0.7.0 (a fork)"},
	} {
		if _, err := m.PathFrom(tc.fw, "v0.8.0"); !errors.Is(err, upgrade.ErrNoUpgradePath) || !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("%s: PathFrom = %v; want ErrNoUpgradePath mentioning %q", name, err, tc.msg)
		}
	}
}

// TestActionsAreImplemented: every action an automatic change can name
// carries its implementation, and record-baseline's records the baseline.
func TestActionsAreImplemented(t *testing.T) {
	a, ok := upgrade.LookupAction("record-baseline")
	if !ok || a.Description == "" {
		t.Fatalf("LookupAction(record-baseline) = %+v, %v", a, ok)
	}
	if _, ok := upgrade.LookupAction("rename-foo"); ok {
		t.Fatal("LookupAction found an action nothing implements")
	}
	dir := app(t, goMod, "name: demo\n")
	if changed, err := a.Apply(dir); err != nil || !changed {
		t.Fatalf("record-baseline Apply = %v, %v", changed, err)
	}
	if b, err := upgrade.Detect(dir); err != nil || !b.Recorded {
		t.Fatalf("after record-baseline: %+v, %v", b, err)
	}
	if changed, err := a.Apply(dir); err != nil || changed {
		t.Fatalf("record-baseline Apply again = %v, %v; want nothing changed", changed, err)
	}
}

// changelogExempt are the issues the CHANGELOG's Unreleased section links
// that the manifest does not declare, each with the reason.
var changelogExempt = map[int]string{
	291: "benchmark tooling: nothing in an application",
	342: "the compatibility manifest and gombit upgrade notes themselves: new, nothing to act on",
	343: "gombit upgrade --dry-run itself: new, nothing to act on",
	446: "a gombit make resource error message: nothing in an application",
	447: "shipped in v0.6.1 (the CHANGELOG has no 0.6.1 section yet)",
	451: "a CI gate on the committed admin build: nothing in an application",
}

var changelogIssueLink = regexp.MustCompile(`github\.com/gombit-dev/gombit/(?:issues|pull)/(\d+)`)

// TestChangelogIsDeclared: every issue the CHANGELOG's Unreleased section
// links is declared by some change of the manifest (in its refs), or exempt
// above with a reason, so a change the CHANGELOG records cannot be left out
// of the upgrade notes and plans.
func TestChangelogIsDeclared(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "CHANGELOG.md")) // #nosec G304 -- the committed changelog
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	start := strings.Index(text, "\n## [Unreleased]\n")
	if start < 0 {
		t.Fatal("CHANGELOG.md has no [Unreleased] section")
	}
	unreleased := text[start+1:]
	if end := strings.Index(unreleased[1:], "\n## ["); end >= 0 {
		unreleased = unreleased[:end+1]
	}
	m, err := upgrade.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	declared := map[int]bool{}
	for _, r := range m.Releases {
		for _, c := range r.Changes {
			for _, ref := range c.Refs {
				declared[ref] = true
			}
		}
	}
	for _, match := range changelogIssueLink.FindAllStringSubmatch(unreleased, -1) {
		n, _ := strconv.Atoi(match[1])
		if !declared[n] && changelogExempt[n] == "" {
			t.Errorf("CHANGELOG.md's Unreleased section links #%d, which no change in upgrade/manifest.yaml declares (refs) and changelogExempt does not exempt", n)
		}
	}
	for n := range changelogExempt {
		if declared[n] {
			t.Errorf("#%d is exempt but also declared; drop the exemption", n)
		}
	}
}
