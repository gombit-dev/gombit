package upgrade_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/internal/atomicfile"
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
		{ID: "scoped", Area: "config", Summary: "scoped defaults", Config: []upgrade.ConfigChange{
			{Var: "GOMBIT_TMPL", Change: upgrade.ConfigDefault, From: "old", To: "new", Environment: "production"},
			{Var: "GOMBIT_OPP", Change: upgrade.ConfigDefault, From: "old", To: "new", Environment: "production"},
			{Var: "GOMBIT_PRODSET", Change: upgrade.ConfigDefault, From: "old", To: "new", Environment: "production"},
			{Var: "GOMBIT_DEVSET", Change: upgrade.ConfigDefault, From: "old", To: "new", Environment: "development"},
		}},
	}},
}

// secretValues are values planted in the app's environment files: none
// may appear in a report.
var secretValues = []string{"s3cr3t-dotenv-old", "s3cr3t-jwt", "tmpl-placeholder-value", "prod-def-value"}

func configApp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		".env.example":     "# Copy to .env.\nGOMBIT_OLD=tmpl-placeholder-value\n# GOMBIT_GONE=1\nGOMBIT_KEEP=1\nGOMBIT_TMPL=old\n",
		".env":             "GOMBIT_OLD=s3cr3t-dotenv-old\nGOMBIT_JWT_SECRET=s3cr3t-jwt\nexport GOMBIT_GONE=1\nGOMBIT_DEVSET=old\n",
		".env.production":  "GOMBIT_DEF=prod-def-value\nGOMBIT_PRODSET=old\n",
		".env.development": "GOMBIT_OPP=old\n",
		"env.example":      "GOMBIT_UNSET=1\n", // not an environment file (no leading dot)
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
	if len(files) != 4 || !files[".env.example"] || files[".env"] || files[".env.production"] {
		t.Fatalf("files = %+v", r.Files)
	}
	type want struct {
		category    upgrade.ConfigCategory
		action      bool
		setIn       string
		templateIn  string
		documented  string
		fixable     bool
		adviceHints []string
	}
	for i, w := range []want{
		{upgrade.ConfigCategoryRenamed, true, ".env", ".env.example", "", true, []string{"Rename GOMBIT_OLD to GOMBIT_NEW in .env, .env.example and the deployment's environment"}},
		// .env's `export GOMBIT_GONE=1` sets "export GOMBIT_GONE" for the
		// loader, not GOMBIT_GONE: not a mention.
		{upgrade.ConfigCategoryActionRequired, true, "", "", ".env.example", true, []string{"Remove GOMBIT_GONE"}},
		// .env.production is a mention, not something gombit loads.
		{upgrade.ConfigCategoryDefaultChanged, true, ".env.production", "", "", false, []string{"Nothing in the application directory proves it set for development, test, production"}},
		{upgrade.ConfigCategoryDefaultChanged, true, "", "", "", false, []string{"Nothing in the application directory proves it set for production", "set GOMBIT_UNSET=x"}},
		{upgrade.ConfigCategorySecurityReview, true, "", "", "", false, []string{"Check the value of GOMBIT_HTTP_TRUSTED_PROXIES in production", "Security review: Production refuses trust-all proxies."}},
		{upgrade.ConfigCategoryActionRequired, true, "", "", "", false, []string{"Set GOMBIT_REQ in production"}},
		// A template is not a runtime override.
		{upgrade.ConfigCategoryDefaultChanged, true, "", ".env.example", "", false, []string{"Nothing in the application directory proves it set for production", "other files (templates, .env.*) are not loaded by gombit"}},
		// Nor is a file named for an environment.
		{upgrade.ConfigCategoryDefaultChanged, true, ".env.development", "", "", false, []string{"Nothing in the application directory proves it set for production"}},
		// An unused .env.production proves nothing about production: a
		// production default change still needs action.
		{upgrade.ConfigCategoryDefaultChanged, true, ".env.production", "", "", false, []string{"Nothing in the application directory proves it set for production"}},
		// .env, which gombit loads, covers local development runs.
		{upgrade.ConfigCategoryDefaultChanged, false, ".env", "", "", false, []string{"Set in .env, which gombit loads for local runs (development), so unaffected there"}},
	} {
		if i >= len(r.Findings) {
			t.Fatalf("findings = %+v", r.Findings)
		}
		f := r.Findings[i]
		if f.Category != w.category || f.NeedsAction != w.action || strings.Join(f.SetIn, ", ") != w.setIn || strings.Join(f.TemplateSetIn, ", ") != w.templateIn || strings.Join(f.DocumentedIn, ", ") != w.documented || f.Fixable != w.fixable {
			t.Errorf("finding %d (%s): %+v, want %+v", i, f.Var, f, w)
		}
		for _, h := range w.adviceHints {
			if !strings.Contains(f.Advice, h) {
				t.Errorf("finding %d (%s): advice %q, want it to say %q", i, f.Var, f.Advice, h)
			}
		}
	}
	if len(r.Findings) != 10 || !r.NeedsAction() {
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
		"GOMBIT_KEEP=1\n" +
		"GOMBIT_TMPL=old\n"
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
	if f := r.Findings[0]; !f.NeedsAction || strings.Join(f.SetIn, ",") != ".env" || len(f.TemplateSetIn) != 0 {
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
	head := "format: 1\nreleases:\n  - version: v0.1.0\n  - version: v0.2.0\n    changes:\n      - id: x\n        kind: manual\n        area: %s\n        summary: s\n        details: d\n        config:\n          - %s\n"
	ok := []string{
		"{var: GOMBIT_A, change: renamed, to: GOMBIT_B}",
		"{var: GOMBIT_A, change: removed}",
		"{var: GOMBIT_A, change: default, from: a, to: b, environment: production}",
		"{var: GOMBIT_A, change: default, from: a}",
		"{var: GOMBIT_A, change: required, environment: production}",
		"{var: GOMBIT_A, change: refused}",
		"{var: GOMBIT_A, change: added}",
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
		"{var: GOMBIT_A, change: added, to: x}":                     "config",
		"{var: GOMBIT_A, change: added, environment: production}":   "config",
	} {
		if _, err := upgrade.ParseManifest([]byte(fmt.Sprintf(head, area, entry))); err == nil {
			t.Errorf("%s (area %s): accepted", entry, area)
		}
	}
	// A change listing variables is manual: the notes and the plan must
	// not call it nothing to do (informational) or something the tooling
	// applies (automatic) while the analysis asks the developer to act.
	for _, kind := range []string{"informational", "automatic\n        action: record-baseline"} {
		data := strings.Replace(fmt.Sprintf(head, "security", "{var: GOMBIT_A, change: refused}"), "kind: manual", "kind: "+kind, 1)
		if _, err := upgrade.ParseManifest([]byte(data)); err == nil || !strings.Contains(err.Error(), "so it is manual") {
			t.Errorf("kind %s with config entries: %v", kind, err)
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
	if got, want := read(t, filepath.Join(dir, ".env.example")), "\ufeff# GOMBIT_OLD was renamed GOMBIT_NEW in v0.8.0 (r).\nGOMBIT_NEW=x\n# GOMBIT_NEW=\n"; got != want {
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

// TestAnalyzeConfigChainedRenames: each change is judged against the
// templates as the path's earlier changes leave them, the order
// WriteConfigTemplates applies them in, and against the other files as
// they are. Renaming A to B, then B to C (in a later release), a template
// setting A sets B for the second rename and is fixable for both, and
// writing it settles both; a .env setting A proves nothing about B, so a
// later default change of B still needs action.
func TestAnalyzeConfigChainedRenames(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{".env.example": "GOMBIT_A=1\n", ".env": "GOMBIT_A=2\n"})
	path := append(rename("GOMBIT_A", "GOMBIT_B"), upgrade.Release{Version: "v0.9.0", Changes: []upgrade.Change{
		{ID: "b-to-c", Area: "config", Summary: "renamed", Config: []upgrade.ConfigChange{{Var: "GOMBIT_B", Change: upgrade.ConfigRenamed, To: "GOMBIT_C"}}},
	}})
	r, err := upgrade.AnalyzeConfig(dir, path)
	if err != nil || len(r.Findings) != 2 {
		t.Fatalf("AnalyzeConfig = %+v, %v", r, err)
	}
	for i, f := range r.Findings {
		wantSetIn := []string{".env", ""}[i]
		if !f.Fixable || strings.Join(f.TemplateSetIn, ", ") != ".env.example" || strings.Join(f.SetIn, ", ") != wantSetIn {
			t.Errorf("%s: fixable %v, template_set_in %v, set_in %v; want fixable, in .env.example, set_in %q", f.ChangeID, f.Fixable, f.TemplateSetIn, f.SetIn, wantSetIn)
		}
	}
	// .env still sets the chain's first name: the second rename says so.
	if b := r.Findings[1]; b.SetAs[".env"] != "GOMBIT_A" || !strings.Contains(b.Advice, ".env sets it as GOMBIT_A") {
		t.Errorf("b-to-c: set_as %v, advice %q; want .env setting it as GOMBIT_A", b.SetAs, b.Advice)
	}
	// A default change of B after the rename: .env still sets A only.
	withDefault := append(path[:1:1], upgrade.Release{Version: "v0.9.0", Changes: []upgrade.Change{
		{ID: "b-default", Area: "config", Summary: "default", Config: []upgrade.ConfigChange{{Var: "GOMBIT_B", Change: upgrade.ConfigDefault, From: "1", To: "2", Environment: "development"}}},
	}})
	if r, err := upgrade.AnalyzeConfig(dir, withDefault); err != nil || len(r.Findings) != 2 || !r.Findings[1].NeedsAction || len(r.Findings[1].SetIn) != 0 || r.Findings[1].SetAs[".env"] != "GOMBIT_A" {
		t.Fatalf("a default change after the rename = %+v, %v; .env does not set GOMBIT_B", r.Findings, err)
	}
	if _, err := upgrade.WriteConfigTemplates(dir, path); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(t, filepath.Join(dir, ".env.example")), "\nGOMBIT_C=1\n") {
		t.Errorf(".env.example = %q, want GOMBIT_C set", read(t, filepath.Join(dir, ".env.example")))
	}
	if r, err = upgrade.AnalyzeConfig(dir, path); err != nil {
		t.Fatal(err)
	}
	for _, f := range r.Findings {
		if f.Fixable {
			t.Errorf("%s still fixable after the write", f.ChangeID)
		}
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
	if strings.Join(r.Unreadable, ",") != ".env.production" || strings.Join(r.Findings[0].SetIn, ",") != ".env" || strings.Join(r.Findings[0].TemplateSetIn, ",") != ".env.example" || !r.NeedsAction() {
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
		Config: []upgrade.ConfigChange{{Var: "GOMBIT_OLD", Change: upgrade.ConfigDefault, From: "a", To: "b", Environment: "development"}}}}}}
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

// TestWriteConfigTemplatesBOMRoundTrip: a template's byte order mark stays
// the file's first bytes after the rewrite (a note line inserted above the
// first assignment goes after it), so the upgraded template, copied to
// .env, still loads: the renamed variable is read.
func TestWriteConfigTemplatesBOMRoundTrip(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{".env.example": "\ufeffGOMBIT_OLD_NAME=demo-app\n"})
	if _, err := upgrade.WriteConfigTemplates(dir, rename("GOMBIT_OLD_NAME", "GOMBIT_APP_NAME")); err != nil {
		t.Fatal(err)
	}
	upgraded := read(t, filepath.Join(dir, ".env.example"))
	if !strings.HasPrefix(upgraded, "\ufeff# GOMBIT_OLD_NAME was renamed") || strings.Count(upgraded, "\ufeff") != 1 {
		t.Fatalf("upgraded = %q", upgraded)
	}
	app := t.TempDir()
	writeTree(t, app, map[string]string{".env": upgraded})
	if old, set := os.LookupEnv("GOMBIT_APP_NAME"); set {
		_ = os.Unsetenv("GOMBIT_APP_NAME")
		defer func() { _ = os.Setenv("GOMBIT_APP_NAME", old) }()
	}
	cfg, err := config.LoadFromDir(app)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AppName != "demo-app" {
		t.Fatalf("the upgraded template, loaded as .env, gives AppName %q; want demo-app", cfg.AppName)
	}
}

// TestWriteConfigTemplatesCountsACommittedWrite: a template whose new
// content is in place, but whose directory sync failed (not durable), is
// reported written, with the error.
func TestWriteConfigTemplatesCountsACommittedWrite(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{".env.example": "GOMBIT_OLD=1\n"})
	defer atomicfile.SetSyncDirForTest(func(string) error { return errors.New("sync failed") })()
	written, err := upgrade.WriteConfigTemplates(dir, rename("GOMBIT_OLD", "GOMBIT_NEW"))
	if !errors.Is(err, atomicfile.ErrNotDurable) || strings.Join(written, ",") != ".env.example" {
		t.Fatalf("WriteConfigTemplates = %v, %v; want .env.example written and ErrNotDurable", written, err)
	}
	if !strings.Contains(read(t, filepath.Join(dir, ".env.example")), "GOMBIT_NEW=1") {
		t.Fatal("the template does not have its new content")
	}
}

// TestAnalyzeConfigDotEnvIsLocalOnly: .env, the file gombit loads, proves a
// variable set for local development runs only. An unscoped default change
// it sets still needs action: test binaries run elsewhere, and a
// deployment sets its own environment.
func TestAnalyzeConfigDotEnvIsLocalOnly(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{".env": "GOMBIT_X=old\n"})
	r, err := upgrade.AnalyzeConfig(dir, []upgrade.Release{{Version: "v0.8.0", Changes: []upgrade.Change{{ID: "d", Area: "config", Summary: "d",
		Config: []upgrade.ConfigChange{{Var: "GOMBIT_X", Change: upgrade.ConfigDefault, From: "old", To: "new"}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if f := r.Findings[0]; !f.NeedsAction || !strings.Contains(f.Advice, "proves it set for test, production") || strings.Join(f.SetIn, ",") != ".env" {
		t.Fatalf("finding = %+v", f)
	}
}

// loadAppName loads dir/.env through the runtime loader (as gombit does)
// and returns the app name it configures.
func loadAppName(t *testing.T, dir string) string {
	t.Helper()
	if old, set := os.LookupEnv("GOMBIT_APP_NAME"); set {
		_ = os.Unsetenv("GOMBIT_APP_NAME")
		t.Cleanup(func() { _ = os.Setenv("GOMBIT_APP_NAME", old) })
	}
	cfg, err := config.LoadFromDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.AppName
}

// TestConfigUsesTheLoaderGrammar: the analysis and the template writer read
// a line as the runtime loader does. `export GOMBIT_APP_NAME=x` in .env
// does not set GOMBIT_APP_NAME for gombit (config.LoadFromDir keeps the
// default), so it is no override of a development default change; and in a
// template it is not the rename's new name already set, so the old line is
// renamed, not commented out, and the upgraded template, loaded as .env,
// still sets the value.
func TestConfigUsesTheLoaderGrammar(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{".env": "export GOMBIT_APP_NAME=exported\n"})
	if got := loadAppName(t, dir); got != "Gombit" {
		t.Fatalf("the loader set GOMBIT_APP_NAME = %q from an export line; the test's premise is wrong", got)
	}
	r, err := upgrade.AnalyzeConfig(dir, []upgrade.Release{{Version: "v0.8.0", Changes: []upgrade.Change{{ID: "d", Area: "config", Summary: "d",
		Config: []upgrade.ConfigChange{{Var: "GOMBIT_APP_NAME", Change: upgrade.ConfigDefault, From: "Gombit", To: "App", Environment: "development"}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if f := r.Findings[0]; !f.NeedsAction || len(f.SetIn) != 0 {
		t.Fatalf("an export line counted as an override: %+v", f)
	}

	tmpl := t.TempDir()
	writeTree(t, tmpl, map[string]string{".env.example": "GOMBIT_OLD_NAME=demo-app\nexport GOMBIT_APP_NAME=ignored\n"})
	if _, err := upgrade.WriteConfigTemplates(tmpl, rename("GOMBIT_OLD_NAME", "GOMBIT_APP_NAME")); err != nil {
		t.Fatal(err)
	}
	upgraded := read(t, filepath.Join(tmpl, ".env.example"))
	app := t.TempDir()
	writeTree(t, app, map[string]string{".env": upgraded})
	if got := loadAppName(t, app); got != "demo-app" {
		t.Fatalf("the upgraded template, loaded as .env, sets AppName %q; want demo-app:\n%s", got, upgraded)
	}
}

// TestConfigReadsTheWholeFileAsTheLoaderDoes: an assignment after a line
// longer than 64 KiB is read by the loader (config.LoadFromDir) and by
// the analysis alike: the development override it makes is one.
func TestConfigReadsTheWholeFileAsTheLoaderDoes(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{".env": "GOMBIT_PAD=" + strings.Repeat("p", 100<<10) + "\nGOMBIT_APP_NAME=after-long\n"})
	if got := loadAppName(t, dir); got != "after-long" {
		t.Fatalf("the loader gives AppName %q", got)
	}
	r, err := upgrade.AnalyzeConfig(dir, []upgrade.Release{{Version: "v0.8.0", Changes: []upgrade.Change{{ID: "d", Area: "config", Summary: "d",
		Config: []upgrade.ConfigChange{{Var: "GOMBIT_APP_NAME", Change: upgrade.ConfigDefault, From: "Gombit", To: "App", Environment: "development"}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if f := r.Findings[0]; f.NeedsAction || strings.Join(f.SetIn, ",") != ".env" {
		t.Fatalf("finding = %+v", f)
	}
}

// TestConfigAdditions: an addition asks nothing of an application, so a
// change that only adds variables may be informational, the analysis
// reports nothing for it, and the notes say the variable is new; a change
// that also requires one is manual.
func TestConfigAdditions(t *testing.T) {
	head := "format: 1\nreleases:\n  - version: v0.1.0\n  - version: v0.2.0\n    changes:\n      - id: x\n        kind: informational\n        area: config\n        summary: s\n        config:\n"
	if _, err := upgrade.ParseManifest([]byte(head + "          - {var: GOMBIT_NEW, change: added}\n")); err != nil {
		t.Fatalf("an informational addition: %v", err)
	}
	if _, err := upgrade.ParseManifest([]byte(head + "          - {var: GOMBIT_NEW, change: added}\n          - {var: GOMBIT_NEW, change: required, environment: production}\n")); err == nil || !strings.Contains(err.Error(), "so it is manual") {
		t.Fatalf("an informational addition and requirement: %v", err)
	}
	r, err := upgrade.AnalyzeConfig(t.TempDir(), []upgrade.Release{{Version: "v0.2.0", Changes: []upgrade.Change{{ID: "x", Kind: upgrade.KindManual, Area: "config", Summary: "s",
		Config: []upgrade.ConfigChange{{Var: "GOMBIT_NEW", Change: upgrade.ConfigAdded}, {Var: "GOMBIT_NEW", Change: upgrade.ConfigRequired, Environment: "production"}}}}}})
	if err != nil || len(r.Findings) != 1 || r.Findings[0].Change != upgrade.ConfigRequired {
		t.Fatalf("findings = %+v, %v", r.Findings, err)
	}
	if got := (upgrade.ConfigChange{Var: "GOMBIT_NEW", Change: upgrade.ConfigAdded}).Describe(); got != "`GOMBIT_NEW` is new" {
		t.Fatalf("Describe = %q", got)
	}
}

// TestConfigSecurityAdditionsAreReviewed: an addition in a security change
// is not silent. The manifest refuses it as anything but manual, and the
// analysis reports it as a security review needing action (what makes
// gombit upgrade config exit 1).
func TestConfigSecurityAdditionsAreReviewed(t *testing.T) {
	data := "format: 1\nreleases:\n  - version: v0.1.0\n  - version: v0.2.0\n    changes:\n      - id: x\n        kind: informational\n        area: security\n        summary: A new auth setting.\n        config:\n          - {var: GOMBIT_AUTH_MODE, change: added}\n"
	if _, err := upgrade.ParseManifest([]byte(data)); err == nil || !strings.Contains(err.Error(), "so it is manual") {
		t.Fatalf("an informational security addition: %v", err)
	}
	manual := strings.Replace(strings.Replace(data, "kind: informational", "kind: manual", 1), "summary: A new auth setting.\n", "summary: A new auth setting.\n        details: Review it.\n", 1)
	m, err := upgrade.ParseManifest([]byte(manual))
	if err != nil {
		t.Fatalf("a manual security addition: %v", err)
	}
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{".env": "GOMBIT_AUTH_MODE=cookie\n"})
	r, err := upgrade.AnalyzeConfig(dir, m.Releases[1:])
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Findings) != 1 || r.Findings[0].Category != upgrade.ConfigCategorySecurityReview || !r.Findings[0].NeedsAction || !r.NeedsAction() ||
		!strings.Contains(r.Findings[0].Advice, "GOMBIT_AUTH_MODE is new from v0.2.0") || strings.Join(r.Findings[0].SetIn, ",") != ".env" {
		t.Fatalf("report = %+v", r)
	}
	var text strings.Builder
	_ = r.Render(&text)
	if !strings.Contains(text.String(), "[security review] `GOMBIT_AUTH_MODE` is new") || strings.Contains(text.String(), "cookie") {
		t.Fatalf("report:\n%s", text.String())
	}
}
