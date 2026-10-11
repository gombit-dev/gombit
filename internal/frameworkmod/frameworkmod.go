// Package frameworkmod reads what an application's go.mod declares about the
// Gombit framework module: the required version, the replace directive that
// applies to it, and so the framework release it names (Release). It is the
// one reading behind every command that reports the framework an app builds
// against (gombit contract app, gombit upgrade): they share which replace
// applies and which version, if any, is a framework release.
//
// It reads go.mod alone, offline: it does not load the module graph, and
// knows nothing of a go.work (whether one applies is the go command's
// answer, `go env GOWORK`; see package upgrade). Within go.mod it picks the
// replace directive the go command applies, and refuses what the go command
// refuses: the framework required twice, or replaced twice with different
// targets.
package frameworkmod

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/mod/modfile"
)

// ModulePath is the module a Gombit application requires.
const ModulePath = "github.com/gombit-dev/gombit"

// ErrNotRequired: go.mod does not require the framework module.
var ErrNotRequired = errors.New("go.mod does not require " + ModulePath)

// Replacement is a replace directive's target.
type Replacement struct {
	// Path is a module path, or a local directory when Version is empty.
	Path string `json:"path"`
	// Version is the replacement module's version; empty for a directory.
	Version string `json:"version,omitempty"`
	// ForVersion is the framework version the replace applies to (its left
	// side's version), or empty when it applies to every version. A replace
	// of every version keeps pinning the build when the require moves; one of
	// the required version stops applying.
	ForVersion string `json:"for_version,omitempty"`
}

// Local reports whether the replacement is a local directory.
func (r Replacement) Local() bool { return r.Version == "" }

// Declaration is what go.mod declares about the framework module.
type Declaration struct {
	// Required is the require directive's version.
	Required string
	// Replace is the replace directive that applies to Required, or nil.
	Replace *Replacement
}

// Release is the framework release go.mod names: the required version, or
// the version of a replace by the framework module itself. It is empty when
// the framework is replaced by a local directory or by another module (a
// fork): neither is a release of the framework, whatever its version says.
func (d Declaration) Release() string {
	switch {
	case d.Replace == nil:
		return d.Required
	case d.Replace.Path == ModulePath && !d.Replace.Local():
		return d.Replace.Version
	default:
		return ""
	}
}

// Read reads the go.mod in workDir. A missing file is reported as an error
// wrapping os.ErrNotExist; a go.mod not requiring the framework, as
// ErrNotRequired.
func Read(workDir string) (Declaration, error) {
	path := filepath.Join(workDir, "go.mod")
	data, err := os.ReadFile(path) // #nosec G304 -- the go.mod of the app being inspected
	if err != nil {
		return Declaration{}, fmt.Errorf("read %s: %w", path, err)
	}
	return Parse(path, data)
}

// Parse reads go.mod content (path names it in errors).
func Parse(path string, data []byte) (Declaration, error) {
	f, err := modfile.Parse(path, data, nil)
	if err != nil {
		return Declaration{}, err
	}
	var d Declaration
	for _, r := range f.Require {
		if r.Mod.Path != ModulePath {
			continue
		}
		if d.Required != "" {
			return Declaration{}, fmt.Errorf("%s: %s is required twice (%s and %s), which the go command refuses", path, ModulePath, d.Required, r.Mod.Version)
		}
		d.Required = r.Mod.Version
	}
	if d.Required == "" {
		return Declaration{}, fmt.Errorf("%s: %w", path, ErrNotRequired)
	}
	r, err := replacementFor(f.Replace, d.Required)
	if err != nil {
		return Declaration{}, fmt.Errorf("%s: %w", path, err)
	}
	if r != nil {
		d.Replace = &Replacement{Path: r.New.Path, Version: r.New.Version, ForVersion: r.Old.Version}
	}
	return d, nil
}

// replacementFor is the replace directive that applies to the framework at
// version required, as the go command resolves it: a replace of that exact
// version wins over one of every version, whatever their order, and a
// replace of another version does not apply. Two replaces of the same
// framework version (or two of every version) with different targets are
// an error, as they are to the go command.
func replacementFor(replaces []*modfile.Replace, required string) (*modfile.Replace, error) {
	byOld := map[string]*modfile.Replace{}
	for _, r := range replaces {
		if r.Old.Path != ModulePath {
			continue
		}
		if prev, ok := byOld[r.Old.Version]; ok && prev.New != r.New {
			old := ModulePath
			if r.Old.Version != "" {
				old += "@" + r.Old.Version
			}
			return nil, fmt.Errorf("conflicting replacements for %s (%s and %s), which the go command refuses", old, target(prev), target(r))
		}
		byOld[r.Old.Version] = r
	}
	if r, ok := byOld[required]; ok {
		return r, nil
	}
	return byOld[""], nil
}

func target(r *modfile.Replace) string {
	if r.New.Version == "" {
		return r.New.Path
	}
	return r.New.Path + " " + r.New.Version
}
