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
// no framework release: it is replaced by a local filesystem path (a framework
// dev checkout, the common case) or by another module (a fork, whose version
// is not a framework release). A host cannot pin such a build as a framework
// version, so this is surfaced rather than guessed. A replace by the framework
// module itself at another version IS resolvable, and that version is
// reported instead.
var ErrFrameworkVersionUnresolved = errors.New("appcontract: framework version is unresolved (replaced by a local path or another module)")

// FrameworkVersion reads the version of the Gombit framework the app in workDir
// builds against, from its go.mod. It deliberately parses the *declared*
// dependency rather than running the go toolchain, so it is offline and
// deterministic. It is internal/frameworkmod's Release, the same answer
// gombit upgrade baseline gives for go.mod.
//
// The replace directive that applies to the required version (as the go
// command picks it) wins over the require: a replace by the framework module
// at another version reports that version, while a local filesystem replace
// or a fork is not a framework release and returns
// ErrFrameworkVersionUnresolved. A go.mod that does not require the framework
// at all, or that the go command refuses (the framework required twice,
// conflicting replaces), is an error.
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
	if v := d.Release(); v != "" {
		return v, nil
	}
	return "", ErrFrameworkVersionUnresolved
}
