// Package frameworkmod reads what an application's go.mod declares about the
// Gombit framework module: the required version and the replace directive
// that applies to it. It is the one reading behind every command that
// reports the framework an app builds against (gombit contract app, gombit
// upgrade), so they cannot disagree.
//
// It reads go.mod alone, offline: it does not load the module graph, and
// knows nothing of a go.work (whether one applies is the go command's
// answer, `go env GOWORK`; see package upgrade). Within go.mod it resolves
// replace directives as the go command does.
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
		if r.Mod.Path == ModulePath {
			d.Required = r.Mod.Version
		}
	}
	if d.Required == "" {
		return Declaration{}, fmt.Errorf("%s: %w", path, ErrNotRequired)
	}
	if r := replacementFor(f.Replace, d.Required); r != nil {
		d.Replace = &Replacement{Path: r.New.Path, Version: r.New.Version}
	}
	return d, nil
}

// replacementFor is the replace directive that applies to the framework at
// version required, as the go command resolves it: a replace of that exact
// version wins over one of every version, whatever their order, and a
// replace of another version does not apply.
func replacementFor(replaces []*modfile.Replace, required string) *modfile.Replace {
	var wildcard *modfile.Replace
	for _, r := range replaces {
		switch {
		case r.Old.Path != ModulePath:
		case r.Old.Version == required:
			return r
		case r.Old.Version == "":
			wildcard = r
		}
	}
	return wildcard
}
