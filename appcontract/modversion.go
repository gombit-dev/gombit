package appcontract

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// FrameworkModulePath is the module a Gombit application requires.
const FrameworkModulePath = "github.com/gombit-dev/gombit"

// ErrFrameworkVersionUnresolved is returned when go.mod names the framework but
// its version cannot be reported as a resolvable module version — a local
// filesystem replace directive (a framework dev checkout) being the common
// case. A host cannot pin such a build, so this is surfaced rather than guessed.
// A version-to-version replace (e.g. to a published fork) IS resolvable and its
// target version is reported instead.
var ErrFrameworkVersionUnresolved = errors.New("appcontract: framework version is unresolved (replaced with a local path)")

// semverPattern matches a canonical module version, including Go
// pseudo-versions. Mirrors scaffold.semverPattern; build metadata is excluded.
var semverPattern = regexp.MustCompile(
	`^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-[0-9A-Za-z.-]+)?$`)

// FrameworkVersion reads the version of the Gombit framework the app in workDir
// builds against, straight from its go.mod require directive. It deliberately
// parses the *declared* dependency rather than running the go toolchain, so it
// is offline and deterministic.
//
// A replace of the framework module wins over the require: a version-to-version
// replace (or a published fork with a version) reports its target version,
// while a local filesystem replace is unresolvable for a host and returns
// ErrFrameworkVersionUnresolved. A go.mod that does not require the framework at
// all is an error — it is not a Gombit app.
func FrameworkVersion(workDir string) (string, error) {
	path := filepath.Join(workDir, "go.mod")
	data, err := os.ReadFile(path) // #nosec G304 -- go.mod at a caller-provided project dir
	if err != nil {
		return "", fmt.Errorf("appcontract: read %s: %w", path, err)
	}
	return frameworkVersionFromModfile(string(data))
}

// frameworkVersionFromModfile extracts the framework version from go.mod
// content. A replace of the framework module wins over the require: a
// version-to-version replace reports its target version; a local filesystem
// replace is unresolvable. Split out for testing without touching the
// filesystem.
func frameworkVersionFromModfile(content string) (string, error) {
	requireVer := ""
	replaceVer := ""
	replacedLocal := false

	inRequireBlock := false
	inReplaceBlock := false

	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		trimmed := strings.TrimSpace(stripComment(scanner.Text()))
		if trimmed == "" {
			continue
		}

		switch {
		case inRequireBlock:
			if trimmed == ")" {
				inRequireBlock = false
				continue
			}
			if v, ok := requireVersion(trimmed); ok {
				requireVer = v
			}
		case inReplaceBlock:
			if trimmed == ")" {
				inReplaceBlock = false
				continue
			}
			applyReplace(trimmed, &replaceVer, &replacedLocal)
		case strings.HasPrefix(trimmed, "require ("):
			inRequireBlock = true
		case strings.HasPrefix(trimmed, "replace ("):
			inReplaceBlock = true
		case strings.HasPrefix(trimmed, "require "):
			if v, ok := requireVersion(strings.TrimSpace(trimmed[len("require"):])); ok {
				requireVer = v
			}
		case strings.HasPrefix(trimmed, "replace "):
			applyReplace(strings.TrimSpace(trimmed[len("replace"):]), &replaceVer, &replacedLocal)
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("appcontract: scan go.mod: %w", err)
	}

	switch {
	case replacedLocal:
		return "", ErrFrameworkVersionUnresolved
	case replaceVer != "":
		return replaceVer, nil
	case requireVer != "":
		return requireVer, nil
	default:
		return "", fmt.Errorf("appcontract: go.mod does not require %s", FrameworkModulePath)
	}
}

// requireVersion returns the version if entry is a require line for the
// framework module, e.g. "github.com/gombit-dev/gombit v0.5.0".
func requireVersion(entry string) (string, bool) {
	fields := strings.Fields(entry)
	if len(fields) >= 2 && fields[0] == FrameworkModulePath {
		return fields[1], true
	}
	return "", false
}

// applyReplace records the effect of a replace directive on the framework
// module. A version-to-version replace (right-hand side ends in a module
// version) sets ver; a local filesystem replace (no version) sets local.
func applyReplace(entry string, ver *string, local *bool) {
	lhs, rhs, ok := strings.Cut(entry, "=>")
	if !ok {
		return
	}
	if fields := strings.Fields(lhs); len(fields) == 0 || fields[0] != FrameworkModulePath {
		return
	}
	rhsFields := strings.Fields(rhs)
	if len(rhsFields) == 0 {
		*local = true
		return
	}
	if last := rhsFields[len(rhsFields)-1]; semverPattern.MatchString(last) {
		*ver = last
		return
	}
	*local = true
}

// stripComment removes a trailing // comment (e.g. "// indirect") outside of
// any string, which go.mod lines never contain.
func stripComment(line string) string {
	if i := strings.Index(line, "//"); i >= 0 {
		return line[:i]
	}
	return line
}
