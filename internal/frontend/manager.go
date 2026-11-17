// Package frontend selects the package manager used by a generated frontend.
package frontend

import (
	"errors"
	"os"
	"path/filepath"
)

// ErrNoPackageManager means neither supported frontend package manager is on PATH.
var ErrNoPackageManager = errors.New("npm and pnpm not found")

// DetectPackageManager selects pnpm for a frontend with a pnpm lockfile, and
// otherwise prefers npm. pnpm remains a fallback when npm is unavailable.
func DetectPackageManager(frontendDir string, lookPath func(string) (string, error)) (name, path string, err error) {
	if _, statErr := os.Stat(filepath.Join(frontendDir, "pnpm-lock.yaml")); statErr == nil {
		if path, err := lookPath("pnpm"); err == nil {
			return "pnpm", path, nil
		}
	}
	if path, err := lookPath("npm"); err == nil {
		return "npm", path, nil
	}
	if path, err := lookPath("pnpm"); err == nil {
		return "pnpm", path, nil
	}
	return "", "", ErrNoPackageManager
}
