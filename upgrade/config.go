package upgrade

import (
	"bytes"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/internal/atomicfile"
)

// ConfigChangeKind is what a release does to a configuration variable.
type ConfigChangeKind string

const (
	// ConfigRenamed: the variable is read under another name (To) now; the
	// old one is ignored.
	ConfigRenamed ConfigChangeKind = "renamed"
	// ConfigRemoved: the variable is no longer read.
	ConfigRemoved ConfigChangeKind = "removed"
	// ConfigDefault: the variable's default (when unset) changed, From to
	// To.
	ConfigDefault ConfigChangeKind = "default"
	// ConfigRequired: the variable must now be set (in Environment, or
	// everywhere).
	ConfigRequired ConfigChangeKind = "required"
	// ConfigRefused: values the variable accepted before are now refused
	// (in Environment, or everywhere); the change's details say which.
	ConfigRefused ConfigChangeKind = "refused"
	// ConfigAdded: the variable is new, read from this release on. It is
	// declared when a later declaration (a requirement, a refusal, a
	// default change, a rename) needs it to exist, or when a security
	// change introduces it. An ordinary addition asks nothing of an
	// application; a security one is a security review.
	ConfigAdded ConfigChangeKind = "added"
)

// ConfigChange is one configuration variable a release changes.
type ConfigChange struct {
	// Var is the variable, e.g. GOMBIT_HTTP_ADDR.
	Var string `yaml:"var" json:"var"`
	// Change is what the release does to it.
	Change ConfigChangeKind `yaml:"change" json:"change"`
	// To is, for a rename, the new name; for a default change, the new
	// default.
	To string `yaml:"to,omitempty" json:"to,omitempty"`
	// From is, for a default change, the old default.
	From string `yaml:"from,omitempty" json:"from,omitempty"`
	// Environment limits a default change, a requirement or a refusal to
	// one runtime environment (development, test, production); empty is
	// every one.
	Environment string `yaml:"environment,omitempty" json:"environment,omitempty"`
}

var configVar = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

func (cc ConfigChange) validate() error {
	if !configVar.MatchString(cc.Var) {
		return fmt.Errorf("config var %q: want an environment variable name (A-Z, 0-9, _)", cc.Var)
	}
	switch cc.Environment {
	case "", "development", "test", "production":
	default:
		return fmt.Errorf("config %s: environment %q, want development, test or production", cc.Var, cc.Environment)
	}
	switch cc.Change {
	case ConfigRenamed:
		if !configVar.MatchString(cc.To) || cc.To == cc.Var {
			return fmt.Errorf("config %s: renamed, so to names its new variable", cc.Var)
		}
		if cc.From != "" || cc.Environment != "" {
			return fmt.Errorf("config %s: a rename has no from or environment", cc.Var)
		}
	case ConfigRemoved:
		if cc.To != "" || cc.From != "" || cc.Environment != "" {
			return fmt.Errorf("config %s: a removal has no to, from or environment", cc.Var)
		}
	case ConfigDefault:
		if cc.From == cc.To {
			return fmt.Errorf("config %s: a default change has a from and a to that differ", cc.Var)
		}
	case ConfigRequired, ConfigRefused:
		if cc.To != "" || cc.From != "" {
			return fmt.Errorf("config %s: %s has no to or from", cc.Var, cc.Change)
		}
	case ConfigAdded:
		if cc.To != "" || cc.From != "" || cc.Environment != "" {
			return fmt.Errorf("config %s: an addition has no to, from or environment", cc.Var)
		}
	default:
		return fmt.Errorf("config %s: change %q, want added, renamed, removed, default, required or refused", cc.Var, cc.Change)
	}
	return nil
}

// Describe is the change in one line of Markdown, e.g. "`GOMBIT_A` is
// renamed `GOMBIT_B`".
func (cc ConfigChange) Describe() string {
	where := ""
	if cc.Environment != "" {
		where = " in " + cc.Environment
	}
	switch cc.Change {
	case ConfigRenamed:
		return fmt.Sprintf("`%s` is renamed `%s`", cc.Var, cc.To)
	case ConfigRemoved:
		return fmt.Sprintf("`%s` is removed: it is no longer read", cc.Var)
	case ConfigDefault:
		return fmt.Sprintf("`%s` defaults to %s%s (was %s)", cc.Var, quoteDefault(cc.To), where, quoteDefault(cc.From))
	case ConfigRequired:
		return fmt.Sprintf("`%s` must be set%s", cc.Var, where)
	case ConfigRefused:
		return fmt.Sprintf("`%s`: some values are now refused%s", cc.Var, where)
	case ConfigAdded:
		return fmt.Sprintf("`%s` is new", cc.Var)
	}
	return "`" + cc.Var + "`"
}

func quoteDefault(v string) string {
	if v == "" {
		return "empty"
	}
	return "`" + v + "`"
}

// ConfigCategory is how a configuration change concerns an application.
type ConfigCategory string

const (
	// ConfigCategoryRenamed: a variable it may set has a new name.
	ConfigCategoryRenamed ConfigCategory = "renamed"
	// ConfigCategoryDefaultChanged: a variable it may leave unset defaults
	// to something else.
	ConfigCategoryDefaultChanged ConfigCategory = "default-changed"
	// ConfigCategoryActionRequired: a variable is removed, now required,
	// or refuses values it accepted.
	ConfigCategoryActionRequired ConfigCategory = "action-required"
	// ConfigCategorySecurityReview: the change is security-sensitive.
	ConfigCategorySecurityReview ConfigCategory = "security-review"
)

// ConfigFinding is a configuration change of the upgrade, and what it
// means for the application: where its environment files mention the
// variable, and what to do. It never carries a value from those files.
type ConfigFinding struct {
	ConfigChange
	// Release and ChangeID say where the manifest declares it.
	Release  string `json:"release"`
	ChangeID string `json:"change_id"`
	// Category is how it concerns the application.
	Category ConfigCategory `json:"category"`
	// NeedsAction is whether the developer must act (or review): every
	// rename, removal, requirement, refusal and security change, and a
	// default change the application's files do not override.
	NeedsAction bool `json:"needs_action"`
	// SetIn are the environment files (not templates) that set the
	// variable; TemplateSetIn the templates that set it, and DocumentedIn
	// those that mention it commented out: each template as the path's
	// earlier changes leave it (WriteConfigTemplates rewrites it), every
	// other file as it is (see AnalyzeConfig). They are mentions: only
	// .env, for local development runs, is a runtime override gombit loads.
	SetIn         []string `json:"set_in"`
	TemplateSetIn []string `json:"template_set_in"`
	DocumentedIn  []string `json:"documented_in"`
	// SetAs are the files (not templates) that set the variable under an
	// earlier name of a rename on the path, each with that name: renaming
	// A to B, then B to C, a .env setting A is listed for B. It proves
	// nothing about the new name (a default change still needs action),
	// but the file needs the renames.
	SetAs map[string]string `json:"set_as,omitempty"`
	// Fixable is whether WriteConfigTemplates would change a template for
	// it (false once it has).
	Fixable bool `json:"fixable"`
	// Advice says what to do.
	Advice string `json:"advice"`
}

// ConfigFile is an environment file of the application the analysis read.
type ConfigFile struct {
	// Path is relative to the application directory.
	Path string `json:"path"`
	// Template is whether it is a template (.env.example and the like),
	// which WriteConfigTemplates may upgrade. Any other environment file
	// (.env, .env.production) may hold secrets: only the names it sets are
	// read, and it is never written.
	Template bool `json:"template"`
}

// ConfigReport is the configuration side of an upgrade for an application.
type ConfigReport struct {
	// Files are the environment files read.
	Files []ConfigFile `json:"files"`
	// Unreadable are the environment files that could not be read (no
	// permission, not a regular file): what they set is unknown, so check
	// them by hand.
	Unreadable []string `json:"unreadable"`
	// Findings are the upgrade's configuration changes, in release order.
	Findings []ConfigFinding `json:"findings"`
}

// NeedsAction reports whether any finding needs the developer.
func (r ConfigReport) NeedsAction() bool {
	if len(r.Unreadable) > 0 && len(r.Findings) > 0 {
		return true // what those files set is unknown
	}
	for _, f := range r.Findings {
		if f.NeedsAction {
			return true
		}
	}
	return false
}

var utf8BOM = []byte("\ufeff")

// templateSuffixes mark an environment file as a template: committed,
// placeholders only.
var templateSuffixes = []string{".example", ".sample", ".template", ".dist"}

func isTemplate(name string) bool {
	for _, suffix := range templateSuffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// envFile is an environment file: the variables it sets and documents (a
// commented-out assignment), and, for a template only, its lines.
type envFile struct {
	ConfigFile
	abs        string
	set        map[string]bool
	documented map[string]bool
	lines      []string          // templates only: what WriteConfigTemplates rewrites
	setAs      map[string]string // not templates: a later name of a rename -> the one the file sets
	bom        bool              // a template starting with a UTF-8 byte order mark
	mode       os.FileMode
}

// readNames records the variables body sets and documents. What a file
// sets is what the loader would read from it: its own traversal (the whole
// file) and grammar.
func (f *envFile) readNames(body []byte) {
	f.set, f.documented = map[string]bool{}, map[string]bool{}
	for _, key := range config.DotEnvKeys(body) {
		if configVar.MatchString(key) {
			f.set[key] = true
		}
	}
	for _, line := range bytes.Split(body, []byte("\n")) {
		if key, commented, ok := envAssignment(line); ok && commented {
			f.documented[key] = true
		}
	}
}

// advance makes a template what WriteConfigTemplates leaves it once it has
// applied cc, so the findings for later changes are about the templates as
// the earlier ones leave them, the order WriteConfigTemplates applies them
// in: renaming A to B, then B to C, a template setting A is fixable for
// both. Another file is never written, so it stays as it is: what it sets
// is what it proves. It only remembers, for a renamed variable it sets,
// the later names (SetAs), so the findings that follow still point at it.
func (f *envFile) advance(release, changeID string, cc ConfigChange) {
	if !f.Template {
		if cc.Change != ConfigRenamed {
			return
		}
		name := f.setAs[cc.Var]
		if f.set[cc.Var] {
			name = cc.Var
		}
		if name != "" {
			if f.setAs == nil {
				f.setAs = map[string]string{}
			}
			f.setAs[cc.To] = name
		}
		return
	}
	f.lines = upgradeTemplate(f.lines, release, changeID, cc)
	f.readNames([]byte(strings.Join(f.lines, "")))
}

// envAssignment is the variable an environment file line assigns, by the
// loader's grammar (config.DotEnvKey: "export KEY=" assigns no KEY), and
// whether the line is a commented-out assignment ("# KEY=...", documenting
// it). It works on the line's bytes and copies out the name only, so no
// value becomes a string.
func envAssignment(line []byte) (key string, commented, ok bool) {
	s := bytes.TrimSpace(line)
	if bytes.HasPrefix(s, []byte("#")) {
		commented = true
		s = bytes.TrimSpace(s[1:])
	}
	key, ok = config.DotEnvKey(s)
	if !ok || !configVar.MatchString(key) {
		return "", false, false
	}
	return key, commented, true
}

// lineKey is envAssignment of a template's line.
func lineKey(line string) (key string, commented, ok bool) {
	return envAssignment([]byte(line))
}

// readEnvFiles reads the application's environment files: .env and .env.*
// in its directory, a symlink read through. A file that is not a template
// is read for its names only, over its bytes, which are then zeroed. A file
// that cannot be read is listed in unreadable, not an error: what it sets
// is unknown, and the report says so.
func readEnvFiles(workDir string) (files []*envFile, unreadable []string, err error) {
	entries, err := os.ReadDir(workDir)
	if err != nil {
		return nil, nil, fmt.Errorf("upgrade: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		if name != ".env" && !strings.HasPrefix(name, ".env.") {
			continue
		}
		f := &envFile{ConfigFile: ConfigFile{Path: name}, abs: filepath.Join(workDir, name), set: map[string]bool{}, documented: map[string]bool{}}
		info, err := os.Stat(f.abs) // through a symlink
		if err != nil || !info.Mode().IsRegular() {
			if err != nil || !info.IsDir() {
				unreadable = append(unreadable, name)
			}
			continue
		}
		f.Template = isTemplate(name)
		f.mode = info.Mode().Perm()
		data, err := os.ReadFile(f.abs) // #nosec G304 -- an environment file in the application directory
		if err != nil {
			unreadable = append(unreadable, name)
			continue
		}
		body := data
		if f.Template {
			// A byte order mark belongs to the file, not its first line:
			// kept apart, and written back first.
			f.bom = bytes.HasPrefix(data, utf8BOM)
			body = bytes.TrimPrefix(data, utf8BOM)
		}
		f.readNames(body)
		if f.Template {
			f.lines = strings.SplitAfter(string(body), "\n")
			if f.lines[len(f.lines)-1] == "" {
				f.lines = f.lines[:len(f.lines)-1]
			}
		}
		clear(data)
		files = append(files, f)
	}
	return files, unreadable, nil
}

// AnalyzeConfig reports what the configuration changes of releases (an
// upgrade's path, in order) mean for the application in workDir: per
// change, its category, whether the developer must act, the environment
// files that set or document the variable, and what to do. It reads
// workDir's .env and .env.* files and never reports a value from them:
// from a file that is not a template, only the names it sets are read.
// Every rename and removal is reported whether or not a file mentions the
// variable (a deployment sets its environment elsewhere), and every change
// of a security change is a security review. Each change is judged
// against the templates as the path's earlier changes leave them, in the
// order WriteConfigTemplates applies them, and against the other files as
// they are, since nothing writes them.
func AnalyzeConfig(workDir string, releases []Release) (ConfigReport, error) {
	files, unreadable, err := readEnvFiles(workDir)
	if err != nil {
		return ConfigReport{}, err
	}
	report := ConfigReport{Files: []ConfigFile{}, Unreadable: append([]string{}, unreadable...), Findings: []ConfigFinding{}}
	for _, f := range files {
		report.Files = append(report.Files, f.ConfigFile)
	}
	for _, r := range releases {
		for _, c := range r.Changes {
			for _, cc := range c.Config {
				// An ordinary addition needs nothing: what follows it, if
				// anything, does.
				if cc.Change != ConfigAdded || c.Area == "security" {
					report.Findings = append(report.Findings, finding(r.Version, c, cc, files))
				}
				for _, f := range files {
					f.advance(r.Version, c.ID, cc)
				}
			}
		}
	}
	return report, nil
}

func finding(release string, c Change, cc ConfigChange, files []*envFile) ConfigFinding {
	f := ConfigFinding{ConfigChange: cc, Release: release, ChangeID: c.ID, SetIn: []string{}, TemplateSetIn: []string{}, DocumentedIn: []string{}}
	for _, file := range files {
		switch {
		case file.set[cc.Var] && file.Template:
			f.TemplateSetIn = append(f.TemplateSetIn, file.Path)
		case file.set[cc.Var]:
			f.SetIn = append(f.SetIn, file.Path)
		case file.setAs[cc.Var] != "":
			if f.SetAs == nil {
				f.SetAs = map[string]string{}
			}
			f.SetAs[file.Path] = file.setAs[cc.Var]
		}
		if file.Template && file.documented[cc.Var] {
			f.DocumentedIn = append(f.DocumentedIn, file.Path)
		}
		if file.Template {
			// Fixable: writing the templates would change this one.
			f.Fixable = f.Fixable || strings.Join(upgradeTemplate(file.lines, release, c.ID, cc), "") != strings.Join(file.lines, "")
		}
	}
	where := "the deployment's environment"
	if mentioned := append(append([]string{}, f.SetIn...), f.TemplateSetIn...); len(mentioned) > 0 {
		where = strings.Join(mentioned, ", ") + " and the deployment's environment"
	}
	scope := ""
	if cc.Environment != "" {
		scope = " in " + cc.Environment
	}
	switch cc.Change {
	case ConfigRenamed:
		f.Category, f.NeedsAction = ConfigCategoryRenamed, true
		f.Advice = fmt.Sprintf("Rename %s to %s in %s: from %s, %s is not read.", cc.Var, cc.To, where, releaseName(release), cc.Var)
	case ConfigRemoved:
		f.Category, f.NeedsAction = ConfigCategoryActionRequired, true
		f.Advice = fmt.Sprintf("Remove %s from %s: from %s it has no effect.", cc.Var, where, releaseName(release))
	case ConfigDefault:
		f.Category = ConfigCategoryDefaultChanged
		// Unaffected only where a runtime file for that environment sets
		// it: every environment the change applies to must be covered.
		affected := []string{"development", "test", "production"}
		if cc.Environment != "" {
			affected = []string{cc.Environment}
		}
		var covered, uncovered []string
		for _, env := range affected {
			if overridesFor(files, cc.Var, env) {
				covered = append(covered, env)
			} else {
				uncovered = append(uncovered, env)
			}
		}
		if len(uncovered) == 0 {
			f.Advice = fmt.Sprintf("Set in .env, which gombit loads for local runs (%s), so unaffected there; wherever else it is unset, it now defaults to %s.", strings.Join(covered, ", "), quoteDefault(cc.To))
		} else {
			f.NeedsAction = true
			f.Advice = fmt.Sprintf("Nothing in the application directory proves it set for %s: .env covers only local development runs, and other files (templates, .env.*) are not loaded by gombit. Where it is unset it now defaults to %s; set %s=%s there to keep the old behavior.", strings.Join(uncovered, ", "), quoteDefault(cc.To), cc.Var, cc.From)
		}
	case ConfigRequired:
		f.Category, f.NeedsAction = ConfigCategoryActionRequired, true
		f.Advice = fmt.Sprintf("Set %s%s (in %s).", cc.Var, scope, where)
	case ConfigRefused:
		f.Category, f.NeedsAction = ConfigCategoryActionRequired, true
		f.Advice = fmt.Sprintf("Check the value of %s%s against %s (in %s).", cc.Var, scope, c.ID, where)
	case ConfigAdded: // reported only for a security change
		f.Category, f.NeedsAction = ConfigCategoryActionRequired, true
		f.Advice = fmt.Sprintf("%s is new from %s: review what it controls and how the app sets it (in %s).", cc.Var, releaseName(release), where)
	}
	for _, path := range slices.Sorted(maps.Keys(f.SetAs)) {
		f.Advice += fmt.Sprintf(" %s sets it as %s, its name before an earlier rename on this path: rename that line too.", path, f.SetAs[path])
	}
	if c.Area == "security" {
		f.Category, f.NeedsAction = ConfigCategorySecurityReview, true
		f.Advice += " Security review: " + strings.TrimSpace(c.Summary)
	}
	return f
}

// overridesFor reports whether the application's files prove name set in
// env, for runs the framework configures from them. The only file gombit
// loads is .env, from the directory a process runs in (config.Load), and
// only a run from the application directory reads this one: a local run,
// in development. Test binaries run in their package directories, and a
// deployment sets its own environment, so no file here proves test or
// production; other .env.* files are loaded by nothing in gombit, and a
// template is copied, not loaded.
func overridesFor(files []*envFile, name, env string) bool {
	if env != "development" {
		return false
	}
	for _, f := range files {
		if f.Path == ".env" && f.set[name] {
			return true
		}
	}
	return false
}

// WriteConfigTemplates upgrades the application's environment templates
// (.env.example and the like) for the configuration changes of releases,
// in order, and returns the templates it wrote. A renamed variable is
// renamed where a template sets or documents it (unless the template
// already has the new name: then the old line is commented out, as a
// removed variable's is), with a note above saying why. Every other line
// is kept as it is, values and line endings included, and each template
// is written atomically. A file that is not a template (.env, which may
// hold secrets) is never written. Running it again changes nothing.
func WriteConfigTemplates(workDir string, releases []Release) ([]string, error) {
	files, unreadable, err := readEnvFiles(workDir)
	if err != nil {
		return nil, err
	}
	for _, name := range unreadable {
		if isTemplate(name) {
			return nil, fmt.Errorf("upgrade: cannot read the template %s, so it cannot be upgraded", name)
		}
	}
	var written []string
	for _, f := range files {
		if !f.Template {
			continue
		}
		before := strings.Join(f.lines, "")
		lines := f.lines
		for _, r := range releases {
			for _, c := range r.Changes {
				for _, cc := range c.Config {
					lines = upgradeTemplate(lines, r.Version, c.ID, cc)
				}
			}
		}
		after := strings.Join(lines, "")
		if after == before {
			continue
		}
		if f.bom {
			after = string(utf8BOM) + after
		}
		// A symlinked template is written through: the link stays.
		target, err := filepath.EvalSymlinks(f.abs)
		if err != nil {
			return written, fmt.Errorf("upgrade: %w", err)
		}
		if err := atomicfile.Write(target, []byte(after), f.mode); err != nil {
			if atomicfile.Committed(err) { // its new content is in place
				written = append(written, f.Path)
			}
			sort.Strings(written)
			return written, fmt.Errorf("upgrade: write %s: %w", f.Path, err)
		}
		written = append(written, f.Path)
	}
	sort.Strings(written)
	return written, nil
}

// upgradeTemplate applies one configuration change to a template's lines.
// A renamed variable's line is renamed, unless the template already has
// the new name: in an active line, when the old one is active too (the old
// line is then commented out, as a removed variable's is), or at all, for
// a commented-out old line (then just noted).
func upgradeTemplate(lines []string, release, changeID string, cc ConfigChange) []string {
	if cc.Change != ConfigRenamed && cc.Change != ConfigRemoved {
		return lines
	}
	newSet, newAny := false, false
	for _, line := range lines {
		if key, commented, ok := lineKey(line); ok && cc.Change == ConfigRenamed && key == cc.To {
			newAny = true
			newSet = newSet || !commented
		}
	}
	var out []string
	for _, line := range lines {
		key, commented, ok := lineKey(line)
		if !ok || key != cc.Var {
			out = append(out, line)
			continue
		}
		eol := lineEnding(line)
		if cc.Change == ConfigRenamed && (commented && !newAny || !commented && !newSet) {
			out = append(out, fmt.Sprintf("# %s was renamed %s in %s (%s).%s", cc.Var, cc.To, releaseName(release), changeID, eol))
			out = append(out, renameKey(line, cc.Var, cc.To))
			continue
		}
		note := fmt.Sprintf("# %s is no longer read from %s (%s).%s", cc.Var, releaseName(release), changeID, eol)
		if len(out) == 0 || out[len(out)-1] != note {
			out = append(out, note)
		}
		if commented {
			out = append(out, line)
		} else {
			out = append(out, "# "+line)
		}
	}
	return out
}

// releaseName is how notes and advice name a release.
func releaseName(version string) string {
	if version == Unreleased {
		return "the next release"
	}
	return version
}

// renameKey is line with its variable old renamed new, nothing else
// changed.
func renameKey(line, old, new string) string {
	i := strings.Index(line, old)
	return line[:i] + new + line[i+len(old):]
}

func lineEnding(line string) string {
	switch {
	case strings.HasSuffix(line, "\r\n"):
		return "\r\n"
	case strings.HasSuffix(line, "\n"):
		return "\n"
	}
	return "\n"
}

var categoryLabels = map[ConfigCategory]string{
	ConfigCategoryRenamed:        "renamed",
	ConfigCategoryDefaultChanged: "default changed",
	ConfigCategoryActionRequired: "action required",
	ConfigCategorySecurityReview: "security review",
}

// Render writes the report as text: each finding with its category, where
// it is declared and what to do, then the environment files read.
func (r ConfigReport) Render(w io.Writer) error {
	var b strings.Builder
	r.render(&b)
	_, err := io.WriteString(w, b.String())
	return err
}

func (r ConfigReport) render(b *strings.Builder) {
	fmt.Fprintf(b, "Configuration changes: %d\n", len(r.Findings))
	for _, f := range r.Findings {
		label := categoryLabels[f.Category]
		if !f.NeedsAction {
			label += ", no action"
		}
		fmt.Fprintf(b, "  - [%s] %s (%s, %s)\n      %s\n", label, f.Describe(), f.Release, f.ChangeID, f.Advice)
		if f.Fixable {
			b.WriteString("      gombit upgrade config --write upgrades the templates for it.\n")
		}
	}
	if len(r.Unreadable) > 0 {
		fmt.Fprintf(b, "  Not readable, so not checked: %s. Check them by hand.\n", strings.Join(r.Unreadable, ", "))
	}
	if len(r.Files) == 0 {
		b.WriteString("  Environment files read: none (no readable .env or .env.* in the application directory)\n")
		return
	}
	var names []string
	for _, f := range r.Files {
		if f.Template {
			names = append(names, f.Path+" (template)")
		} else {
			names = append(names, f.Path+" (names only)")
		}
	}
	fmt.Fprintf(b, "  Environment files read: %s. No value from them is shown.\n", strings.Join(names, ", "))
}
