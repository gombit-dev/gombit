package build

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gombit-dev/gombit/internal/frontend"
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
	name, path, err = frontend.DetectPackageManager(frontendDir, lookPath)
	if err != nil {
		return "", "", fmt.Errorf("build: npm and pnpm not found; install Node.js to build the Vite frontend")
	}
	return name, path, nil
}

func frontendDepsInstalled(frontendDir string) bool {
	_, err := os.Stat(filepath.Join(frontendDir, "node_modules"))
	return err == nil
}
