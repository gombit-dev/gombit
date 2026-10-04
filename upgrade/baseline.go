// Package upgrade is Gombit's application upgrade tooling (UPGRADE-0): it
// works out where an application stands (its baseline), what moving it to
// another framework version requires, and proposes those changes for review.
// It never rewrites user-owned source without an explicit write action.
//
// The baseline is two facts. The framework version comes from the app's
// go.mod, the source of truth Go itself builds from, and is never copied
// anywhere else. The scaffold version, the generation conventions the app
// was created with, is recorded by gombit new in a versioned block of
// gombit.yaml:
//
//	gombit:
//	  metadata: 1   # the format of this block
//	  scaffold: 1   # the scaffold conventions the app was generated with
//
// An app generated before that block existed has none: Detect reports it as
// scaffold 0, and RecordBaseline (gombit upgrade baseline --write) records
// that explicitly.
package upgrade

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/mod/modfile"
	"gopkg.in/yaml.v3"
)

// FrameworkModulePath is the module a Gombit application requires.
const FrameworkModulePath = "github.com/gombit-dev/gombit"

// MetadataVersion is the format of the gombit block in gombit.yaml that this
// framework reads and writes. A block with a newer format is refused
// (ErrMetadataTooNew): it was written by a newer framework, whose meaning this
// one cannot know.
const MetadataVersion = 1

// ScaffoldVersion is the version of the conventions gombit new generates
// with (the layout, the framework-owned files, their contents). It moves when
// those conventions change in a way an upgrade has to account for, not with
// every framework release. 0 is an app generated before upgrade metadata
// existed.
const ScaffoldVersion = 1

// ProjectFile is the file the scaffold version is recorded in, at the root of
// an application.
const ProjectFile = "gombit.yaml"

var (
	// ErrNotGombitApp: the directory has no go.mod requiring the framework.
	ErrNotGombitApp = errors.New("upgrade: not a Gombit application")
	// ErrMetadataTooNew: gombit.yaml's metadata format is newer than this
	// framework understands.
	// ErrMetadataTooNew also covers a scaffold version newer than
	// ScaffoldVersion: conventions this framework cannot know.
	ErrMetadataTooNew = errors.New("upgrade: the upgrade metadata is newer than this gombit understands")
)

// Baseline is where an application stands: the framework it builds against
// and the conventions it was generated with.
type Baseline struct {
	// Framework is what go.mod says about the framework module.
	Framework Framework `json:"framework"`
	// Scaffold is the scaffold version the app was generated with: recorded
	// in gombit.yaml, or 0 when no metadata is recorded (Recorded false).
	Scaffold int `json:"scaffold"`
	// Metadata is the format of the recorded block (0 when there is none).
	Metadata int `json:"metadata"`
	// Recorded is whether gombit.yaml carries the metadata block. An app
	// without it was generated before upgrade metadata existed; its
	// baseline is detected (scaffold 0), and can be recorded with
	// RecordBaseline.
	Recorded bool `json:"recorded"`
}

// Framework is the framework dependency as go.mod declares it.
type Framework struct {
	// Module is the framework's module path.
	Module string `json:"module"`
	// Version is the version the app builds against: the require
	// directive's, or a version replace's target. Empty when the framework
	// is replaced by a local directory (Local).
	Version string `json:"version,omitempty"`
	// Required is the require directive's version.
	Required string `json:"required"`
	// Replace is the replace directive's target, if the framework module is
	// replaced: a local path, or "module version". In a workspace that uses
	// a framework checkout, it is that checkout's directory.
	Replace string `json:"replace,omitempty"`
	// Local is true when the framework is replaced by a local directory (a
	// framework checkout): there is no published version to upgrade from.
	Local bool `json:"local,omitempty"`
	// Workspace is the go.work file whose use or replace directive decides
	// the framework the app builds against, when one does.
	Workspace string `json:"workspace,omitempty"`
}

// Detect reads the baseline of the application in workDir: the framework
// from go.mod, the scaffold version from gombit.yaml. It writes nothing.
func Detect(workDir string) (Baseline, error) {
	fw, err := detectFramework(workDir)
	if err != nil {
		return Baseline{}, err
	}
	meta, ok, err := readMetadata(workDir)
	if err != nil {
		return Baseline{}, err
	}
	b := Baseline{Framework: fw}
	if ok {
		b.Recorded = true
		b.Metadata = meta.Metadata
		b.Scaffold = *meta.Scaffold
	}
	return b, nil
}

func detectFramework(workDir string) (Framework, error) {
	path := filepath.Join(workDir, "go.mod")
	data, err := os.ReadFile(path) // #nosec G304 -- go.mod of the app being upgraded
	if errors.Is(err, os.ErrNotExist) {
		return Framework{}, fmt.Errorf("%w: no go.mod in %s", ErrNotGombitApp, workDir)
	}
	if err != nil {
		return Framework{}, fmt.Errorf("upgrade: read %s: %w", path, err)
	}
	f, err := modfile.Parse(path, data, nil)
	if err != nil {
		return Framework{}, fmt.Errorf("upgrade: %w", err)
	}
	fw := Framework{Module: FrameworkModulePath}
	for _, r := range f.Require {
		if r.Mod.Path == FrameworkModulePath {
			fw.Required = r.Mod.Version
		}
	}
	if fw.Required == "" {
		return Framework{}, fmt.Errorf("%w: %s does not require %s", ErrNotGombitApp, path, FrameworkModulePath)
	}
	fw.Version = fw.Required
	if r := frameworkReplace(f.Replace, fw.Required); r != nil {
		fw.applyReplace(r)
	}
	if err := fw.applyWorkspace(workDir); err != nil {
		return Framework{}, err
	}
	return fw, nil
}

// frameworkReplace is the replace directive that applies to the framework at
// version required, as Go resolves it: a replace of that exact version wins
// over one of every version, whatever their order.
func frameworkReplace(replaces []*modfile.Replace, required string) *modfile.Replace {
	var wildcard *modfile.Replace
	for _, r := range replaces {
		switch {
		case r.Old.Path != FrameworkModulePath:
		case r.Old.Version == required:
			return r
		case r.Old.Version == "":
			wildcard = r
		}
	}
	return wildcard
}

func (fw *Framework) applyReplace(r *modfile.Replace) {
	if r.New.Version == "" {
		fw.Replace, fw.Local, fw.Version = r.New.Path, true, ""
	} else {
		fw.Replace, fw.Local, fw.Version = r.New.Path+" "+r.New.Version, false, r.New.Version
	}
}

// applyWorkspace applies the go.work the go command would use for the app
// in workDir (GOWORK, or the nearest go.work above it that uses the app): a
// workspace module that is the framework replaces it with that checkout,
// and the workspace's replace directives override go.mod's.
func (fw *Framework) applyWorkspace(workDir string) error {
	path, err := findWorkFile(workDir)
	if err != nil || path == "" {
		return err
	}
	data, err := os.ReadFile(path) // #nosec G304 -- the go.work the go command uses for the app
	if err != nil {
		return fmt.Errorf("upgrade: read %s: %w", path, err)
	}
	wf, err := modfile.ParseWork(path, data, nil)
	if err != nil {
		return fmt.Errorf("upgrade: %w", err)
	}
	app, err := filepath.Abs(workDir)
	if err != nil {
		return fmt.Errorf("upgrade: %w", err)
	}
	inWorkspace := false
	var checkout string
	for _, u := range wf.Use {
		dir := u.Path
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(filepath.Dir(path), dir)
		}
		dir = filepath.Clean(dir)
		if dir == app {
			inWorkspace = true
			continue
		}
		mod, err := os.ReadFile(filepath.Join(dir, "go.mod")) // #nosec G304 -- a module the workspace uses
		if err == nil && modfile.ModulePath(mod) == FrameworkModulePath {
			checkout = u.Path
		}
	}
	if !inWorkspace {
		// The go command refuses to build the app from this workspace; GOWORK
		// pointing elsewhere is the only way here, and it decides nothing.
		return nil
	}
	switch r := frameworkReplace(wf.Replace, fw.Required); {
	case checkout != "":
		fw.Replace, fw.Local, fw.Version, fw.Workspace = checkout, true, "", path
	case r != nil:
		fw.applyReplace(r)
		fw.Workspace = path
	}
	return nil
}

// findWorkFile is the go.work the go command uses for workDir: GOWORK when
// set ("off" disables workspaces), else the nearest go.work in workDir or
// one of its parents. "" when there is none.
func findWorkFile(workDir string) (string, error) {
	if gowork := os.Getenv("GOWORK"); gowork != "" {
		if gowork == "off" {
			return "", nil
		}
		return gowork, nil
	}
	dir, err := filepath.Abs(workDir)
	if err != nil {
		return "", fmt.Errorf("upgrade: %w", err)
	}
	for {
		path := filepath.Join(dir, "go.work")
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", nil
		}
		dir = parent
	}
}

// metadata is the gombit block of gombit.yaml.
type metadata struct {
	Metadata int  `yaml:"metadata"`
	Scaffold *int `yaml:"scaffold"`
}

// readMetadata reads the gombit block of gombit.yaml; ok is false when the
// file or the block is absent.
func readMetadata(workDir string) (metadata, bool, error) {
	path := filepath.Join(workDir, ProjectFile)
	data, err := os.ReadFile(path) // #nosec G304 -- the app's project file
	if errors.Is(err, os.ErrNotExist) {
		return metadata{}, false, nil
	}
	if err != nil {
		return metadata{}, false, fmt.Errorf("upgrade: read %s: %w", path, err)
	}
	return parseMetadata(path, data)
}

func parseMetadata(path string, data []byte) (metadata, bool, error) {
	var doc struct {
		Gombit *metadata `yaml:"gombit"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return metadata{}, false, fmt.Errorf("upgrade: %s: %w", path, err)
	}
	if doc.Gombit == nil {
		// An empty gombit key is not "no metadata": recording a block
		// would then duplicate the key.
		var keys map[string]any
		if err := yaml.Unmarshal(data, &keys); err == nil {
			if _, present := keys["gombit"]; present {
				return metadata{}, false, fmt.Errorf("upgrade: %s: the gombit key is empty (want the metadata block: gombit.metadata, gombit.scaffold)", path)
			}
		}
		return metadata{}, false, nil
	}
	m := *doc.Gombit
	switch {
	case m.Metadata > MetadataVersion:
		return metadata{}, false, fmt.Errorf("%w: %s has metadata format %d, this gombit reads up to %d; upgrade the gombit CLI", ErrMetadataTooNew, path, m.Metadata, MetadataVersion)
	case m.Metadata < 1:
		return metadata{}, false, fmt.Errorf("upgrade: %s: the gombit block has no metadata format (want gombit.metadata: %d)", path, MetadataVersion)
	case m.Scaffold == nil:
		return metadata{}, false, fmt.Errorf("upgrade: %s: the gombit block has no scaffold version (want gombit.scaffold)", path)
	case *m.Scaffold < 0:
		return metadata{}, false, fmt.Errorf("upgrade: %s: gombit.scaffold is %d, want 0 or more", path, *m.Scaffold)
	case *m.Scaffold > ScaffoldVersion:
		return metadata{}, false, fmt.Errorf("%w: %s has scaffold version %d, this gombit knows up to %d; upgrade the gombit CLI", ErrMetadataTooNew, path, *m.Scaffold, ScaffoldVersion)
	}
	return m, true, nil
}

// MetadataBlock is the gombit.yaml block recording scaffold version scaffold,
// as gombit new writes it.
func MetadataBlock(scaffold int) string {
	return fmt.Sprintf(`# Upgrade baseline (gombit upgrade): the format of this block, and the
# scaffold conventions the app was generated with. The framework version is
# go.mod's, and is not repeated here.
gombit:
  metadata: %d
  scaffold: %d
`, MetadataVersion, scaffold)
}

// RecordBaseline records the detected baseline of an app that has none (one
// generated before upgrade metadata existed) by appending the metadata block,
// with scaffold 0, to its gombit.yaml (creating the file if there is none).
// Nothing else in the file changes. It reports whether it wrote; an app that
// already records its baseline is left as it is.
func RecordBaseline(workDir string) (Baseline, bool, error) {
	b, err := Detect(workDir)
	if err != nil || b.Recorded {
		return b, false, err
	}
	path := filepath.Join(workDir, ProjectFile)
	data, err := os.ReadFile(path) // #nosec G304 -- the app's project file
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return b, false, fmt.Errorf("upgrade: read %s: %w", path, err)
	}
	var out bytes.Buffer
	out.Write(data)
	if len(data) > 0 {
		if data[len(data)-1] != '\n' {
			out.WriteByte('\n')
		}
		out.WriteByte('\n')
	}
	out.WriteString(MetadataBlock(0))
	// Appending is only safe when the result reads back as recorded: a flow
	// mapping, or a file of several YAML documents, would be corrupted or
	// keep the block out of reach. Check before touching the file.
	meta, ok, err := parseMetadata(path, out.Bytes())
	if err == nil && !ok {
		err = errors.New("the block would not be read back")
	}
	if err != nil {
		return b, false, fmt.Errorf("upgrade: %s cannot take the metadata block appended (%v); add it by hand:\n%s", path, err, MetadataBlock(0))
	}
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil { // #nosec G306 -- a project file, committed with the app
		return b, false, fmt.Errorf("upgrade: write %s: %w", path, err)
	}
	b.Recorded, b.Metadata, b.Scaffold = true, meta.Metadata, *meta.Scaffold
	return b, true, nil
}
