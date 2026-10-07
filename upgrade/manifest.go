package upgrade

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	"gopkg.in/yaml.v3"
)

// The compatibility manifest (manifest.yaml, embedded in the binary) is what
// each framework release declares about moving to it: every upgrade-relevant
// change, classified. It is the one source the upgrade tooling plans from and
// the release notes are rendered from (RenderNotes, docs/upgrade-notes.md, the
// GitHub release body), so the two cannot disagree.
//
// It lists every release from the first one it covers, including releases
// with nothing to declare: a version it does not list has no known upgrade
// path, and Path refuses it rather than guess.

// ManifestFormat is the format of manifest.yaml this framework reads.
const ManifestFormat = 1

// Unreleased is the version of the manifest entry collecting the changes of
// the next release while it is developed. It is renamed to that release's
// version when the release is cut (docs/releasing.md); it is never an upgrade
// target.
const Unreleased = "unreleased"

// ErrNoUpgradePath: the manifest describes no upgrade between the versions
// asked for.
var ErrNoUpgradePath = errors.New("upgrade: no upgrade path")

// Kind classifies a change by what moving across it takes.
type Kind string

const (
	// KindAutomatic changes are applied by the upgrade tooling (its Action),
	// as a reviewable change.
	KindAutomatic Kind = "automatic"
	// KindManual changes need the developer to act (Details says how).
	KindManual Kind = "manual"
	// KindInformational changes need nothing done, but are worth knowing
	// when moving across them.
	KindInformational Kind = "informational"
)

var kindOrder = []Kind{KindManual, KindAutomatic, KindInformational}

// Area is the part of an application a change touches.
type Area string

var areas = map[Area]bool{
	"api":        true, // framework Go APIs
	"behavior":   true, // runtime behavior with unchanged APIs
	"cli":        true, // the gombit command tree
	"client":     true, // the generated OpenAPI document and TypeScript client
	"config":     true, // configuration variables and their defaults
	"dependency": true, // module dependencies
	"scaffold":   true, // generated, user-owned files
	"schema":     true, // framework-owned database tables
	"security":   true, // security-sensitive behavior or defaults
}

// Action is an automatic change this framework implements: Apply carries
// it out on the application in workDir, reporting whether it changed
// anything. A manifest change of kind automatic names one, so "automatic"
// always has code behind it.
type Action struct {
	// Description says what Apply does.
	Description string
	// Apply applies the change to the application in workDir.
	Apply func(workDir string) (changed bool, err error)
	// Check reports, reading only, whether the application in workDir,
	// whose baseline is b, needs the change (Apply would change something).
	// Nil means it always does. The upgrade plan counts only the automatic
	// changes an app needs, and must write nothing: TestActionChecksWriteNothing
	// runs every Check against an app and compares its files.
	Check func(workDir string, b Baseline) (needed bool, err error)
}

// actions are the automatic changes this framework implements, by name.
var actions = map[string]Action{
	"record-baseline": {
		Description: "record the upgrade baseline in gombit.yaml (gombit upgrade baseline --write)",
		Apply: func(workDir string) (bool, error) {
			_, wrote, err := RecordBaseline(workDir)
			return wrote, err
		},
		Check: func(_ string, b Baseline) (bool, error) {
			return !b.Recorded, nil
		},
	},
}

// LookupAction returns the automatic action name, and reports whether this
// framework implements it (it has an Apply).
func LookupAction(name string) (Action, bool) {
	a, ok := actions[name]
	return a, ok && a.Apply != nil
}

// Change is one upgrade-relevant change of a release.
type Change struct {
	// ID names the change, unique across the manifest (kebab-case).
	ID string `yaml:"id" json:"id"`
	// Kind is what moving across it takes.
	Kind Kind `yaml:"kind" json:"kind"`
	// Breaking is whether an app can stop building or behave differently
	// across it without acting.
	Breaking bool `yaml:"breaking" json:"breaking"`
	// Area is the part of an application it touches.
	Area Area `yaml:"area" json:"area"`
	// Summary is one line: what changed.
	Summary string `yaml:"summary" json:"summary"`
	// Details is Markdown: what to do (required for a manual change), and
	// why.
	Details string `yaml:"details,omitempty" json:"details,omitempty"`
	// Action names the automation applying an automatic change.
	Action string `yaml:"action,omitempty" json:"action,omitempty"`
	// Refs are the GitHub issues or pull requests of the change.
	Refs []int `yaml:"refs,omitempty" json:"refs,omitempty"`
}

// Release is the changes of moving to one release from the one before it.
type Release struct {
	// Version is the release's version (canonical semver), or Unreleased.
	Version string `yaml:"version" json:"version"`
	// Changes are its upgrade-relevant changes; none for a release with
	// nothing to declare.
	Changes []Change `yaml:"changes,omitempty" json:"changes"`
}

// Manifest is the compatibility manifest.
type Manifest struct {
	// Format is the manifest's format (ManifestFormat).
	Format int `yaml:"format" json:"format"`
	// Releases are in ascending version order. The first is the baseline:
	// the first release the manifest covers, which upgrades start from, so
	// it declares no changes. Unreleased, if present, is last.
	Releases []Release `yaml:"releases" json:"releases"`
}

//go:embed manifest.yaml
var manifestYAML []byte

var loadManifest = sync.OnceValues(func() (*Manifest, error) {
	return ParseManifest(manifestYAML)
})

// LoadManifest returns the manifest embedded in this framework: the releases
// up to its own. Each call returns its own copy, free to modify.
func LoadManifest() (*Manifest, error) {
	m, err := loadManifest()
	if err != nil {
		return nil, err
	}
	return m.clone(), nil
}

func (m *Manifest) clone() *Manifest {
	c := &Manifest{Format: m.Format, Releases: make([]Release, len(m.Releases))}
	for i, r := range m.Releases {
		r.Changes = slices.Clone(r.Changes)
		for j := range r.Changes {
			r.Changes[j].Refs = slices.Clone(r.Changes[j].Refs)
		}
		c.Releases[i] = r
	}
	return c
}

var changeID = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ParseManifest decodes and validates a manifest. Unknown fields are refused,
// so a misspelt key cannot drop a change's classification.
func ParseManifest(data []byte) (*Manifest, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("upgrade: manifest: %w", err)
	}
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("upgrade: manifest: %w", err)
	}
	for i := range m.Releases {
		if m.Releases[i].Changes == nil {
			m.Releases[i].Changes = []Change{} // "changes": [] in JSON, not null
		}
	}
	return &m, nil
}

func (m *Manifest) validate() error {
	if m.Format != ManifestFormat {
		return fmt.Errorf("format %d, this gombit reads format %d", m.Format, ManifestFormat)
	}
	if len(m.Releases) == 0 {
		return errors.New("no releases")
	}
	ids := map[string]string{}
	for i, r := range m.Releases {
		switch {
		case i == 0 && (r.Version == Unreleased || len(r.Changes) > 0):
			// Path starts after its from version, so changes declared on the
			// baseline would never be planned.
			return fmt.Errorf("release %s: the first release is the baseline upgrades start from: a tagged release, with no changes", r.Version)
		case r.Version == Unreleased:
			if i != len(m.Releases)-1 {
				return fmt.Errorf("%s must be the last release", Unreleased)
			}
		case !semver.IsValid(r.Version) || semver.Canonical(r.Version) != r.Version || semver.Build(r.Version) != "":
			return fmt.Errorf("release %q: not a canonical semantic version (vMAJOR.MINOR.PATCH)", r.Version)
		case i > 0 && semver.Compare(m.Releases[i-1].Version, r.Version) >= 0:
			return fmt.Errorf("release %s: releases must be in ascending order, without repeats (after %s)", r.Version, m.Releases[i-1].Version)
		}
		for _, c := range r.Changes {
			if err := c.validate(); err != nil {
				return fmt.Errorf("release %s: %w", r.Version, err)
			}
			if prev, dup := ids[c.ID]; dup {
				return fmt.Errorf("release %s: change %q is already declared by %s", r.Version, c.ID, prev)
			}
			ids[c.ID] = r.Version
		}
	}
	return nil
}

func (c Change) validate() error {
	if !changeID.MatchString(c.ID) {
		return fmt.Errorf("change id %q: want kebab-case (a-z, 0-9, -)", c.ID)
	}
	if !areas[c.Area] {
		return fmt.Errorf("change %s: unknown area %q", c.ID, c.Area)
	}
	if strings.TrimSpace(c.Summary) == "" || strings.Contains(c.Summary, "\n") {
		return fmt.Errorf("change %s: summary must be one non-empty line", c.ID)
	}
	for _, ref := range c.Refs {
		if ref <= 0 {
			return fmt.Errorf("change %s: ref %d is not an issue or pull request number", c.ID, ref)
		}
	}
	switch c.Kind {
	case KindAutomatic:
		if _, ok := LookupAction(c.Action); !ok {
			return fmt.Errorf("change %s: automatic, but action %q is not one this gombit implements", c.ID, c.Action)
		}
	case KindManual, KindInformational:
		if c.Action != "" {
			return fmt.Errorf("change %s: only an automatic change names an action", c.ID)
		}
		if c.Kind == KindManual && strings.TrimSpace(c.Details) == "" {
			return fmt.Errorf("change %s: a manual change needs details (what to do)", c.ID)
		}
	default:
		return fmt.Errorf("change %s: kind %q, want %s, %s or %s", c.ID, c.Kind, KindAutomatic, KindManual, KindInformational)
	}
	return nil
}

// Release returns the manifest's entry for version (a release, or
// Unreleased).
func (m *Manifest) Release(version string) (Release, error) {
	for _, r := range m.Releases {
		if r.Version == version {
			return r, nil
		}
	}
	return Release{}, fmt.Errorf("%w: the compatibility manifest has no release %s (it covers %s)", ErrNoUpgradePath, version, m.coverage())
}

// CheckRelease reports whether the manifest is ready to ship as the stable
// release tag: the tag is listed, it is the newest release (Latest), and no
// change is left under Unreleased (absent, or empty). A manifest that lists
// the tag but keeps the tag's changes under Unreleased, or lists releases
// past it, would ship notes and an upgrade plan saying the release changes
// nothing; the release workflow refuses it.
func (m *Manifest) CheckRelease(tag string) error {
	if _, err := m.Release(tag); err != nil {
		return err
	}
	if latest := m.Latest(); latest != tag {
		return fmt.Errorf("upgrade: manifest: %s is not the newest release it lists (%s is)", tag, latest)
	}
	if r, err := m.Release(Unreleased); err == nil && len(r.Changes) > 0 {
		return fmt.Errorf("upgrade: manifest: %d change(s) are still under %s; move them to %s", len(r.Changes), Unreleased, tag)
	}
	return nil
}

// Latest is the newest release the manifest describes (never Unreleased).
func (m *Manifest) Latest() string {
	for i := len(m.Releases) - 1; i >= 0; i-- {
		if v := m.Releases[i].Version; v != Unreleased {
			return v
		}
	}
	return ""
}

func (m *Manifest) coverage() string {
	first, latest := m.Releases[0].Version, m.Latest()
	if first == latest {
		return first
	}
	return first + " to " + latest
}

// PathFrom is Path from the framework an application builds against (as
// Detect reports it), refusing with the reason a baseline that names no
// framework release: a go.work decides the build, or go.mod replaces the
// framework by a local directory or by another module (a fork). Planning
// from an application goes through here, so no caller can plan from a
// version that is not the framework's.
func (m *Manifest) PathFrom(fw Framework, to string) ([]Release, error) {
	switch {
	case fw.Workspace != "":
		return nil, fmt.Errorf("%w: a go.work (%s) decides the framework the app builds against; run with GOWORK=off to plan from its go.mod", ErrNoUpgradePath, fw.Workspace)
	case fw.Local && fw.Replace != nil:
		return nil, fmt.Errorf("%w: go.mod replaces the framework by the local directory %s, which is no release", ErrNoUpgradePath, fw.Replace.Path)
	case fw.Replace != nil && fw.Version == "":
		return nil, fmt.Errorf("%w: go.mod replaces the framework by another module, %s %s (a fork); the manifest describes the framework's own releases", ErrNoUpgradePath, fw.Replace.Path, fw.Replace.Version)
	}
	return m.Path(fw.Version, to)
}

// Path returns the releases an upgrade from version from to version to moves
// across, in order: every release after from, up to and including to. It
// returns none when from and to are the same, and ErrNoUpgradePath, saying
// why, when the manifest cannot describe the upgrade: a version it does not
// list (before the first release it covers, newer than this framework, a
// pseudo-version or unlisted pre-release, or Unreleased), or a downgrade.
// It trusts that the versions are the framework's; PathFrom plans from an
// application's baseline.
func (m *Manifest) Path(from, to string) ([]Release, error) {
	i, err := m.index(from, "current")
	if err != nil {
		return nil, err
	}
	j, err := m.index(to, "target")
	if err != nil {
		return nil, err
	}
	if j < i {
		return nil, fmt.Errorf("%w: %s is older than %s; downgrades are not supported", ErrNoUpgradePath, to, from)
	}
	return m.Releases[i+1 : j+1], nil
}

// index is the position of release version in the manifest, or why it has
// none.
func (m *Manifest) index(version, role string) (int, error) {
	switch {
	case version == "":
		return 0, fmt.Errorf("%w: no %s version (a framework release) was given", ErrNoUpgradePath, role)
	case version == Unreleased:
		return 0, fmt.Errorf("%w: %s is not a release", ErrNoUpgradePath, Unreleased)
	case !semver.IsValid(version):
		return 0, fmt.Errorf("%w: the %s version %q is not a semantic version", ErrNoUpgradePath, role, version)
	}
	for i, r := range m.Releases {
		if r.Version == version {
			return i, nil
		}
	}
	first, latest := m.Releases[0].Version, m.Latest()
	switch {
	// What the version is comes before where it sorts: the first commit
	// after the newest release is a pseudo-version newer than it, and
	// "upgrade the CLI" would be no help.
	case module.IsPseudoVersion(version):
		return 0, fmt.Errorf("%w: the %s version %s is a pseudo-version (an untagged commit); the compatibility manifest describes tagged releases only, so use one", ErrNoUpgradePath, role, version)
	case semver.Prerelease(version) != "":
		return 0, fmt.Errorf("%w: the %s version %s is a pre-release the compatibility manifest does not list; use a release it lists (%s)", ErrNoUpgradePath, role, version, m.coverage())
	case semver.Compare(version, first) < 0:
		return 0, fmt.Errorf("%w: the %s version %s is older than %s, the first release the compatibility manifest covers; see CHANGELOG.md for earlier releases", ErrNoUpgradePath, role, version, first)
	case semver.Compare(version, latest) > 0:
		return 0, fmt.Errorf("%w: the %s version %s is newer than this gombit knows (its manifest covers %s); upgrade the gombit CLI", ErrNoUpgradePath, role, version, m.coverage())
	default:
		return 0, fmt.Errorf("%w: the %s version %s is within the releases the compatibility manifest covers (%s) but missing from it", ErrNoUpgradePath, role, version, m.coverage())
	}
}

// Classification groups changes by Kind; Breaking holds the breaking ones of
// every kind.
type Classification struct {
	Automatic     []Change `json:"automatic"`
	Manual        []Change `json:"manual"`
	Informational []Change `json:"informational"`
	Breaking      []Change `json:"breaking"`
}

// Classify groups the changes of releases (a Path) by kind, in release order.
func Classify(releases []Release) Classification {
	c := Classification{Automatic: []Change{}, Manual: []Change{}, Informational: []Change{}, Breaking: []Change{}}
	for _, r := range releases {
		for _, ch := range r.Changes {
			switch ch.Kind {
			case KindAutomatic:
				c.Automatic = append(c.Automatic, ch)
			case KindManual:
				c.Manual = append(c.Manual, ch)
			case KindInformational:
				c.Informational = append(c.Informational, ch)
			}
			if ch.Breaking {
				c.Breaking = append(c.Breaking, ch)
			}
		}
	}
	return c
}
