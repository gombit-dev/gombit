package build

import (
	"fmt"
	"os"
	"path/filepath"
)

type frontendPlan struct {
	Manager string
	Path    string
	Args    []string
}

func planFrontendBuild(workDir string, lookPath LookPathFunc) (frontendPlan, error) {
	manager, path, err := detectPackageManager(workDir, lookPath)
	if err != nil {
		return frontendPlan{}, err
	}
	return frontendPlan{
		Manager: manager,
		Path:    path,
		Args:    []string{"run", "build"},
	}, nil
}

func detectPackageManager(workDir string, lookPath LookPathFunc) (name, path string, err error) {
	frontendDir := filepath.Join(workDir, frontendDirRel)
	if _, statErr := os.Stat(filepath.Join(frontendDir, "pnpm-lock.yaml")); statErr == nil {
		if path, err := lookPath("pnpm"); err == nil {
			return "pnpm", path, nil
		}
	}
	// Without a pnpm-lock.yaml this is not a pnpm project. pnpm 10+ refuses
	// to run unapproved dependency build scripts (esbuild's postinstall), so
	// prefer npm and use pnpm only when npm is absent.
	if path, err := lookPath("npm"); err == nil {
		return "npm", path, nil
	}
	if path, err := lookPath("pnpm"); err == nil {
		return "pnpm", path, nil
	}
	return "", "", fmt.Errorf("build: npm and pnpm not found; install Node.js to build the Vite frontend")
}

func frontendDepsInstalled(frontendDir string) bool {
	_, err := os.Stat(filepath.Join(frontendDir, "node_modules"))
	return err == nil
}
