package appcontract

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FrameworkModulePath is the module a Gombit application requires.
const FrameworkModulePath = "github.com/gombit-dev/gombit"

// ErrFrameworkVersionUnresolved is returned when go.mod names the framework but
// its version cannot be reported as a resolvable module version — a local
// replace directive (a framework dev checkout) being the common case. A host
// cannot pin such a build, so this is surfaced rather than guessed.
var ErrFrameworkVersionUnresolved = errors.New("appcontract: framework version is unresolved (replaced or local)")

// FrameworkVersion reads the version of the Gombit framework the app in workDir
// builds against, straight from its go.mod require directive. It deliberately
// parses the *declared* dependency rather than running the go toolchain, so it
// is offline and deterministic.
//
// A replace directive targeting the framework module makes the version
// unresolvable for a host; that returns ErrFrameworkVersionUnresolved. A go.mod
// that does not require the framework at all is an error — it is not a Gombit
// app.
func FrameworkVersion(workDir string) (string, error) {
	path := filepath.Join(workDir, "go.mod")
	data, err := os.ReadFile(path) // #nosec G304 -- go.mod at a caller-provided project dir
	if err != nil {
		return "", fmt.Errorf("appcontract: read %s: %w", path, err)
	}
	return frameworkVersionFromModfile(string(data))
}

// frameworkVersionFromModfile extracts the framework require version from go.mod
// content. Split out for testing without touching the filesystem.
func frameworkVersionFromModfile(content string) (string, error) {
	version := ""
	replaced := false

	inRequireBlock := false
	inReplaceBlock := false

	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := stripComment(scanner.Text())
		trimmed := strings.TrimSpace(line)
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
				version = v
			}
		case inReplaceBlock:
			if trimmed == ")" {
				inReplaceBlock = false
				continue
			}
			if isFrameworkReplace(trimmed) {
				replaced = true
			}
		case strings.HasPrefix(trimmed, "require ("):
			inRequireBlock = true
		case strings.HasPrefix(trimmed, "replace ("):
			inReplaceBlock = true
		case strings.HasPrefix(trimmed, "require "):
			if v, ok := requireVersion(strings.TrimSpace(strings.TrimPrefix(trimmed, "require"))); ok {
				version = v
			}
		case strings.HasPrefix(trimmed, "replace "):
			if isFrameworkReplace(strings.TrimSpace(strings.TrimPrefix(trimmed, "replace"))) {
				replaced = true
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("appcontract: scan go.mod: %w", err)
	}

	if replaced {
		return "", ErrFrameworkVersionUnresolved
	}
	if version == "" {
		return "", fmt.Errorf("appcontract: go.mod does not require %s", FrameworkModulePath)
	}
	return version, nil
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

// isFrameworkReplace reports whether entry replaces the framework module, e.g.
// "github.com/gombit-dev/gombit => ../gombit".
func isFrameworkReplace(entry string) bool {
	fields := strings.Fields(entry)
	return len(fields) >= 1 && fields[0] == FrameworkModulePath
}

// stripComment removes a trailing // comment (e.g. "// indirect") outside of
// any string, which go.mod lines never contain.
func stripComment(line string) string {
	if i := strings.Index(line, "//"); i >= 0 {
		return line[:i]
	}
	return line
}
