package dev

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	reloadHint = "air and watchexec not found; running go run ./cmd/server without reload. Install air (https://github.com/air-verse/air) or watchexec for backend reload."
)

type backendPlan struct {
	Name string
	Path string
	Args []string
	Hint string
}

type frontendPlan struct {
	Manager string
	Path    string
	Args    []string
}

func planBackend(workDir string, lookPath LookPathFunc) (backendPlan, error) {
	if path, err := lookPath("air"); err == nil {
		args := []string{}
		if _, statErr := os.Stat(filepath.Join(workDir, ".air.toml")); statErr == nil {
			args = []string{"-c", ".air.toml"}
		}
		return backendPlan{Name: "air", Path: path, Args: args}, nil
	}
	if path, err := lookPath("watchexec"); err == nil {
		return backendPlan{
			Name: "watchexec",
			Path: path,
			Args: []string{"-r", "-e", "go", "--", "go", "run", "./cmd/server"},
		}, nil
	}
	goBin, err := lookPath("go")
	if err != nil {
		return backendPlan{}, fmt.Errorf("dev: go not found in PATH")
	}
	return backendPlan{
		Name: "go",
		Path: goBin,
		Args: []string{"run", "./cmd/server"},
		Hint: reloadHint,
	}, nil
}

func planFrontend(workDir string, lookPath LookPathFunc, host string, port int) (frontendPlan, error) {
	manager, path, err := detectPackageManager(workDir, lookPath)
	if err != nil {
		return frontendPlan{}, err
	}
	args := []string{"run", "dev", "--", "--host", host, "--port", fmt.Sprintf("%d", port)}
	return frontendPlan{Manager: manager, Path: path, Args: args}, nil
}

func detectPackageManager(workDir string, lookPath LookPathFunc) (name, path string, err error) {
	frontendDir := filepath.Join(workDir, "frontend")
	if _, statErr := os.Stat(filepath.Join(frontendDir, "pnpm-lock.yaml")); statErr == nil {
		if path, err := lookPath("pnpm"); err == nil {
			return "pnpm", path, nil
		}
	}
	// Without a pnpm-lock.yaml, prefer npm. pnpm 10+ refuses to run a
	// dependency's build script (esbuild's postinstall) unless the project
	// approves it, and a pnpm-lock.yaml written by a failed install does not
	// mean pnpm will work. Use pnpm only when npm is absent.
	if path, err := lookPath("npm"); err == nil {
		return "npm", path, nil
	}
	if path, err := lookPath("pnpm"); err == nil {
		return "pnpm", path, nil
	}
	return "", "", fmt.Errorf("dev: npm and pnpm not found; install Node.js to run the Vite frontend")
}
