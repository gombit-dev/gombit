package upgrade_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/upgrade"
)

// configReleases are a made-up upgrade path with every kind of
// configuration change.
var configReleases = []upgrade.Release{
	{Version: "v0.8.0", Changes: []upgrade.Change{
		{ID: "rename-old", Area: "config", Summary: "renamed", Config: []upgrade.ConfigChange{{Var: "GOMBIT_OLD", Change: upgrade.ConfigRenamed, To: "GOMBIT_NEW"}}},
		{ID: "drop-gone", Area: "config", Summary: "removed", Config: []upgrade.ConfigChange{{Var: "GOMBIT_GONE", Change: upgrade.ConfigRemoved}}},
		{ID: "redefault", Area: "config", Summary: "defaults", Config: []upgrade.ConfigChange{
			{Var: "GOMBIT_DEF", Change: upgrade.ConfigDefault, From: "a", To: "b"},
			{Var: "GOMBIT_UNSET", Change: upgrade.ConfigDefault, From: "x", To: "y", Environment: "production"},
		}},
	}},
	{Version: "v0.9.0", Changes: []upgrade.Change{
		{ID: "proxies", Area: "security", Summary: "Production refuses trust-all proxies.", Config: []upgrade.ConfigChange{{Var: "GOMBIT_HTTP_TRUSTED_PROXIES", Change: upgrade.ConfigRefused, Environment: "production"}}},
		{ID: "need-req", Area: "config", Summary: "required", Config: []upgrade.ConfigChange{{Var: "GOMBIT_REQ", Change: upgrade.ConfigRequired, Environment: "production"}}},
	}},
}

// secretValues are values planted in the app's environment files: none
// may appear in a report.
var secretValues = []string{"s3cr3t-dotenv-old", "s3cr3t-jwt", "tmpl-placeholder-value", "prod-def-value"}

func configApp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		".env.example":    "# Copy to .env.\nGOMBIT_OLD=tmpl-placeholder-value\n# GOMBIT_GONE=1\nGOMBIT_KEEP=1\n",
		".env":            "GOMBIT_OLD=s3cr3t-dotenv-old\nGOMBIT_JWT_SECRET=s3cr3t-jwt\nexport GOMBIT_GONE=1\n",
		".env.production": "GOMBIT_DEF=prod-def-value\n",
		"env.example":     "GOMBIT_UNSET=1\n", // not an environment file (no leading dot)
	})
	return dir
}

// TestAnalyzeConfig: every configuration change of the path is reported
// with its category, whether it needs action, the files that set or
// document it, and what to do. Renames and removals are reported however
// the files stand; a default change the files override needs no action; a
// security change is a security review.
func TestAnalyzeConfig(t *testing.T) {
	r, err := upgrade.AnalyzeConfig(configApp(t), configReleases)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]bool{}
	for _, f := range r.Files {
		files[f.Path] = f.Template
	}
	if len(files) != 3 || !files[".env.example"] || files[".env"] || files[".env.production"] {
		t.Fatalf("files = %+v", r.Files)
	}
	type want struct {
		category    upgrade.ConfigCategory
		action      bool
		setIn       string
		documented  string
		fixable     bool
		adviceHints []string
	}
	for i, w := range []want{
		{upgrade.ConfigCategoryRenamed, true, ".env, .env.example", "", true, []string{"Rename GOMBIT_OLD to GOMBIT_NEW in .env, .env.example and the deployment's environment"}},
		{upgrade.ConfigCategoryActionRequired, true, ".env", ".env.example", true, []string{"Remove GOMBIT_GONE"}},
		{upgrade.ConfigCategoryDefaultChanged, false, ".env.production", "", false, []string{"Set in .env.production, so unaffected there"}},
		{upgrade.ConfigCategoryDefaultChanged, true, "", "", false, []string{"No environment file sets it", "in production", "set GOMBIT_UNSET=x"}},
		{upgrade.ConfigCategorySecurityReview, true, "", "", false, []string{"Check the value of GOMBIT_HTTP_TRUSTED_PROXIES in production", "Security review: Production refuses trust-all proxies."}},
		{upgrade.ConfigCategoryActionRequired, true, "", "", false, []string{"Set GOMBIT_REQ in production"}},
	} {
		if i >= len(r.Findings) {
			t.Fatalf("findings = %+v", r.Findings)
		}
		f := r.Findings[i]
		if f.Category != w.category || f.NeedsAction != w.action || strings.Join(f.SetIn, ", ") != w.setIn || strings.Join(f.DocumentedIn, ", ") != w.documented || f.Fixable != w.fixable {
			t.Errorf("finding %d (%s): %+v, want %+v", i, f.Var, f, w)
		}
		for _, h := range w.adviceHints {
			if !strings.Contains(f.Advice, h) {
				t.Errorf("finding %d (%s): advice %q, want it to say %q", i, f.Var, f.Advice, h)
			}
		}
	}
	if len(r.Findings) != 6 || !r.NeedsAction() {
		t.Fatalf("findings = %d, needs action %v", len(r.Findings), r.NeedsAction())
	}
}

// TestAnalyzeConfigNeverShowsValues: no value from any environment file
// (a secret in .env, a template's placeholder) reaches the text report or
// its JSON.
func TestAnalyzeConfigNeverShowsValues(t *testing.T) {
	r, err := upgrade.AnalyzeConfig(configApp(t), configReleases)
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	if err := r.Render(&text); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{text.String(), string(data)} {
		for _, v := range secretValues {
			if strings.Contains(out, v) {
				t.Fatalf("a value from an environment file is shown (%q):\n%s", v, out)
			}
		}
	}
	if !strings.Contains(text.String(), "[security review]") || !strings.Contains(text.String(), ".env (names only)") || !strings.Contains(text.String(), "No value from them is shown.") {
		t.Fatalf("report:\n%s", text.String())
	}
}

// TestWriteConfigTemplates: the templates are upgraded (a rename renamed,
// set or documented, with a note; a removal commented out, with a note;
// a rename onto a name the template already has commented out instead),
// everything else kept byte for byte, CRLF included; a file that is not
// a template is never written; and a second run writes nothing.
func TestWriteConfigTemplates(t *testing.T) {
	dir := configApp(t)
	writeTree(t, dir, map[string]string{
		".env.prod.example": "GOMBIT_NEW=already\r\nGOMBIT_OLD=dup\r\nGOMBIT_KEEP=2\r\n",
	})
	dotenv := read(t, filepath.Join(dir, ".env"))
	written, err := upgrade.WriteConfigTemplates(dir, configReleases)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(written, ",") != ".env.example,.env.prod.example" {
		t.Fatalf("written = %v", written)
	}
	want := "# Copy to .env.\n" +
		"# GOMBIT_OLD was renamed GOMBIT_NEW in v0.8.0 (rename-old).\n" +
		"GOMBIT_NEW=tmpl-placeholder-value\n" +
		"# GOMBIT_GONE is no longer read from v0.8.0 (drop-gone).\n" +
		"# GOMBIT_GONE=1\n" +
		"GOMBIT_KEEP=1\n"
	if got := read(t, filepath.Join(dir, ".env.example")); got != want {
		t.Errorf(".env.example:\n%s\nwant:\n%s", got, want)
	}
	wantCRLF := "GOMBIT_NEW=already\r\n# GOMBIT_OLD is no longer read from v0.8.0 (rename-old).\r\n# GOMBIT_OLD=dup\r\nGOMBIT_KEEP=2\r\n"
	if got := read(t, filepath.Join(dir, ".env.prod.example")); got != wantCRLF {
		t.Errorf(".env.prod.example = %q, want %q", got, wantCRLF)
	}
	if read(t, filepath.Join(dir, ".env")) != dotenv {
		t.Fatal(".env was written")
	}
	again, err := upgrade.WriteConfigTemplates(dir, configReleases)
	if err != nil || len(again) != 0 {
		t.Fatalf("second run wrote %v, %v", again, err)
	}
	r, err := upgrade.AnalyzeConfig(dir, configReleases)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range r.Findings {
		if f.Fixable {
			t.Errorf("%s is still fixable after the write", f.Var)
		}
	}
	// A rename is still reported: the deployment's environment is not ours.
	if f := r.Findings[0]; !f.NeedsAction || strings.Join(f.SetIn, ",") != ".env" {
		t.Errorf("rename after the write: %+v", f)
	}
}

// TestWriteConfigTemplatesKeepsMode: a template keeps its permissions.
func TestWriteConfigTemplatesKeepsMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits")
	}
	dir := configApp(t)
	path := filepath.Join(dir, ".env.example")
	if err := os.Chmod(path, 0o640); err != nil { // #nosec G302 -- a mode the write must keep, under t.TempDir()
		t.Fatal(err)
	}
	if _, err := upgrade.WriteConfigTemplates(dir, configReleases); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %v, %v", info.Mode().Perm(), err)
	}
}

// TestConfigChangesValidate: a manifest's config entries are checked.
func TestConfigChangesValidate(t *testing.T) {
	head := "format: 1\nreleases:\n  - version: v0.1.0\n  - version: v0.2.0\n    changes:\n      - id: x\n        kind: informational\n        area: %s\n        summary: s\n        config:\n          - %s\n"
	ok := []string{
		"{var: GOMBIT_A, change: renamed, to: GOMBIT_B}",
		"{var: GOMBIT_A, change: removed}",
		"{var: GOMBIT_A, change: default, from: a, to: b, environment: production}",
		"{var: GOMBIT_A, change: default, from: a}",
		"{var: GOMBIT_A, change: required, environment: production}",
		"{var: GOMBIT_A, change: refused}",
	}
	for _, entry := range ok {
		if _, err := upgrade.ParseManifest([]byte(fmt.Sprintf(head, "config", entry))); err != nil {
			t.Errorf("%s: %v", entry, err)
		}
	}
	for entry, area := range map[string]string{
		"{var: gombit_a, change: removed}":                          "config",
		"{var: GOMBIT_A, change: moved}":                            "config",
		"{var: GOMBIT_A, change: renamed}":                          "config",
		"{var: GOMBIT_A, change: renamed, to: GOMBIT_A}":            "config",
		"{var: GOMBIT_A, change: renamed, to: GOMBIT_B, from: x}":   "config",
		"{var: GOMBIT_A, change: removed, environment: production}": "config",
		"{var: GOMBIT_A, change: default, from: a, to: a}":          "config",
		"{var: GOMBIT_A, change: required, to: x}":                  "config",
		"{var: GOMBIT_A, change: refused, environment: staging}":    "config",
		"{var: GOMBIT_A, change: removed, extra: 1}":                "config",
		"{var: GOMBIT_A, change: removed} ":                         "behavior",
	} {
		if _, err := upgrade.ParseManifest([]byte(fmt.Sprintf(head, area, entry))); err == nil {
			t.Errorf("%s (area %s): accepted", entry, area)
		}
	}
}

func rename(from, to string) []upgrade.Release {
	return []upgrade.Release{{Version: "v0.8.0", Changes: []upgrade.Change{
		{ID: "r", Area: "config", Summary: "renamed", Config: []upgrade.ConfigChange{{Var: from, Change: upgrade.ConfigRenamed, To: to}}},
	}}}
}

// TestWriteConfigTemplatesRenameOntoDocumentedName: a template that only
// documents the new name (commented out) still gets its active old line
// renamed, so it keeps setting the value; a commented-out old line next to
// the new name's is only noted. A byte order mark does not hide the first
// line.
func TestWriteConfigTemplatesRenameOntoDocumentedName(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		".env.example":   "\ufeffGOMBIT_OLD=x\n# GOMBIT_NEW=\n",
		".env.b.example": "# GOMBIT_OLD=1\n# GOMBIT_NEW=2\n",
	})
	if _, err := upgrade.WriteConfigTemplates(dir, rename("GOMBIT_OLD", "GOMBIT_NEW")); err != nil {
		t.Fatal(err)
	}
	if got, want := read(t, filepath.Join(dir, ".env.example")), "# GOMBIT_OLD was renamed GOMBIT_NEW in v0.8.0 (r).\n\ufeffGOMBIT_NEW=x\n# GOMBIT_NEW=\n"; got != want {
		t.Errorf(".env.example = %q, want %q", got, want)
	}
	if got, want := read(t, filepath.Join(dir, ".env.b.example")), "# GOMBIT_OLD is no longer read from v0.8.0 (r).\n# GOMBIT_OLD=1\n# GOMBIT_NEW=2\n"; got != want {
		t.Errorf(".env.b.example = %q, want %q", got, want)
	}
	r, err := upgrade.AnalyzeConfig(dir, rename("GOMBIT_OLD", "GOMBIT_NEW"))
	if err != nil || len(r.Findings) != 1 || r.Findings[0].Fixable {
		t.Fatalf("after the write: %+v, %v", r, err)
	}
}

// TestAnalyzeConfigUnreadableAndSymlinked: a symlinked environment file is
// read through; one that cannot be read is listed as unchecked (the
// analysis goes on, and a change on the path then needs review), and a
// template that cannot be read is not upgraded. A symlinked template is
// written through, the link kept.
func TestAnalyzeConfigUnreadableAndSymlinked(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs unix permissions and symlinks, as a user they bind")
	}
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"shared/env.local":    "GOMBIT_OLD=s3cr3t-linked\n",
		"shared/tmpl.example": "GOMBIT_OLD=placeholder\n",
		".env.production":     "GOMBIT_OLD=x\n",
	})
	for link, target := range map[string]string{".env": "shared/env.local", ".env.example": "shared/tmpl.example"} {
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(dir, ".env.production"), 0); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(filepath.Join(dir, ".env.production"), 0o600) }()
	releases := rename("GOMBIT_OLD", "GOMBIT_NEW")
	r, err := upgrade.AnalyzeConfig(dir, releases)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(r.Unreadable, ",") != ".env.production" || strings.Join(r.Findings[0].SetIn, ",") != ".env,.env.example" || !r.NeedsAction() {
		t.Fatalf("report = %+v", r)
	}
	var text strings.Builder
	_ = r.Render(&text)
	if !strings.Contains(text.String(), "Not readable, so not checked: .env.production") || strings.Contains(text.String(), "s3cr3t-linked") {
		t.Fatalf("report:\n%s", text.String())
	}
	// A default the readable files override needs no action, but the
	// unreadable file may not override it: review.
	redefault := []upgrade.Release{{Version: "v0.8.0", Changes: []upgrade.Change{{ID: "d", Area: "config", Summary: "d",
		Config: []upgrade.ConfigChange{{Var: "GOMBIT_OLD", Change: upgrade.ConfigDefault, From: "a", To: "b"}}}}}}
	if r, err := upgrade.AnalyzeConfig(dir, redefault); err != nil || r.Findings[0].NeedsAction || !r.NeedsAction() {
		t.Fatalf("default overridden, a file unreadable: %+v, %v", r, err)
	}
	written, err := upgrade.WriteConfigTemplates(dir, releases)
	if err != nil || strings.Join(written, ",") != ".env.example" {
		t.Fatalf("WriteConfigTemplates = %v, %v", written, err)
	}
	if fi, err := os.Lstat(filepath.Join(dir, ".env.example")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the template link was replaced: %v, %v", fi.Mode(), err)
	}
	if got := read(t, filepath.Join(dir, "shared/tmpl.example")); !strings.Contains(got, "GOMBIT_NEW=placeholder") {
		t.Fatalf("the link's target = %q", got)
	}
	// An unreadable template refuses the write.
	if err := os.Chmod(filepath.Join(dir, "shared/tmpl.example"), 0); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(filepath.Join(dir, "shared/tmpl.example"), 0o600) }()
	if _, err := upgrade.WriteConfigTemplates(dir, rename("GOMBIT_NEW", "GOMBIT_NEWER")); err == nil || !strings.Contains(err.Error(), "cannot read the template .env.example") {
		t.Fatalf("WriteConfigTemplates = %v; want the unreadable template refused", err)
	}
}

// TestConfigUnreleasedIsTheNextRelease: a change of the unreleased entry
// is named the next release, in advice and in template notes.
func TestConfigUnreleasedIsTheNextRelease(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{".env.example": "GOMBIT_OLD=1\n"})
	releases := rename("GOMBIT_OLD", "GOMBIT_NEW")
	releases[0].Version = upgrade.Unreleased
	r, err := upgrade.AnalyzeConfig(dir, releases)
	if err != nil || !strings.Contains(r.Findings[0].Advice, "from the next release, GOMBIT_OLD is not read") {
		t.Fatalf("advice = %+v, %v", r, err)
	}
	if _, err := upgrade.WriteConfigTemplates(dir, releases); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(dir, ".env.example")); !strings.HasPrefix(got, "# GOMBIT_OLD was renamed GOMBIT_NEW in the next release (r).\n") {
		t.Fatalf("template = %q", got)
	}
}
