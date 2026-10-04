package appcontract

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/gombit-dev/gombit/internal/frameworkmod"
)

// FrameworkModulePath is the module a Gombit application requires.
const FrameworkModulePath = frameworkmod.ModulePath

// ErrFrameworkVersionUnresolved is returned when go.mod names the framework but
// its version cannot be reported as a resolvable module version — a local
// filesystem replace directive (a framework dev checkout) being the common
// case. A host cannot pin such a build, so this is surfaced rather than guessed.
// A version-to-version replace (e.g. to a published fork) IS resolvable and its
// target version is reported instead.
var ErrFrameworkVersionUnresolved = errors.New("appcontract: framework version is unresolved (replaced with a local path)")

// FrameworkVersion reads the version of the Gombit framework the app in workDir
// builds against, from its go.mod. It deliberately parses the *declared*
// dependency rather than running the go toolchain, so it is offline and
// deterministic. It shares its reading of go.mod (internal/frameworkmod)
// with gombit upgrade, so the two never disagree.
//
// The replace directive that applies to the required version (as the go
// command picks it) wins over the require: a version-to-version replace (or
// a published fork with a version) reports its target version, while a local
// filesystem replace is unresolvable for a host and returns
// ErrFrameworkVersionUnresolved. A go.mod that does not require the framework
// at all is an error — it is not a Gombit app.
func FrameworkVersion(workDir string) (string, error) {
	d, err := frameworkmod.Read(workDir)
	if err != nil {
		return "", fmt.Errorf("appcontract: %w", err)
	}
	return versionOf(d)
}

// frameworkVersionFromModfile extracts the framework version from go.mod
// content. Split out for testing without touching the filesystem.
func frameworkVersionFromModfile(content string) (string, error) {
	d, err := frameworkmod.Parse(filepath.Join(".", "go.mod"), []byte(content))
	if err != nil {
		return "", fmt.Errorf("appcontract: %w", err)
	}
	return versionOf(d)
}

func versionOf(d frameworkmod.Declaration) (string, error) {
	switch {
	case d.Replace == nil:
		return d.Required, nil
	case d.Replace.Local():
		return "", ErrFrameworkVersionUnresolved
	default:
		return d.Replace.Version, nil
	}
}
