package goldentest

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// schemaStub is written only into a temp copy so `tsc --noEmit` can check
// make-resource list/form pages. Those files import ../api/generated/schema
// paths that exist after `gombit client generate` / `gombit dev`, not in the
// resource generator output. It is never committed in testdata/golden.
const schemaStub = `export type paths = {
  [path: string]: {
    parameters?: {
      query?: never;
      header?: never;
      path?: { id?: string };
      cookie?: never;
    };
    get: {
      parameters?: {
        query?: never;
        header?: never;
        path?: { id?: string };
        cookie?: never;
      };
      responses: {
        200: {
          headers: { [name: string]: unknown };
          content: {
            "application/json": { data?: Array<{ id?: unknown; name?: unknown; price?: unknown } & Record<string, unknown>> | null };
          };
        };
      };
    };
    post: {
      requestBody: {
        content: {
          "application/json": { [key: string]: unknown };
        };
      };
      responses: {
        200: {
          headers: { [name: string]: unknown };
          content: {
            "application/json": { data?: Record<string, unknown> };
          };
        };
      };
    };
  };
};
`

func compileBackend(t *testing.T, appDir string) {
	t.Helper()
	copyDir := filepath.Join(t.TempDir(), "compile")
	copyTree(t, appDir, copyDir)
	appendLocalReplace(t, copyDir)

	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = copyDir
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}
	build := exec.Command("go", "build", "./...")
	build.Dir = copyDir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./...: %v\n%s", err, out)
	}
}

func appendLocalReplace(t *testing.T, appDir string) {
	t.Helper()
	goModPath := filepath.Join(appDir, "go.mod")
	// #nosec G304 -- temp copy of generated go.mod
	mod, err := os.OpenFile(goModPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open go.mod: %v", err)
	}
	_, writeErr := mod.WriteString("\nreplace " + gombitModule + " => " + moduleRoot(t) + "\n")
	if closeErr := mod.Close(); closeErr != nil {
		t.Fatalf("close go.mod: %v", closeErr)
	}
	if writeErr != nil {
		t.Fatalf("write replace: %v", writeErr)
	}
}

func typecheckFrontend(t *testing.T, appDir string, stubGeneratedSchema bool) {
	t.Helper()
	if !hasNode() {
		t.Skip("npx/npm not available")
	}

	copyDir := filepath.Join(t.TempDir(), "frontend-check")
	copyTree(t, appDir, copyDir)
	frontend := filepath.Join(copyDir, "frontend")
	if _, err := os.Stat(filepath.Join(frontend, "package.json")); err != nil {
		t.Fatalf("generated frontend/package.json: %v", err)
	}
	if stubGeneratedSchema {
		stubDir := filepath.Join(frontend, "src", "api", "generated")
		if err := os.MkdirAll(stubDir, 0o750); err != nil {
			t.Fatalf("mkdir schema stub: %v", err)
		}
		stubPath := filepath.Join(stubDir, "schema.ts")
		if err := os.WriteFile(stubPath, []byte(schemaStub), 0o600); err != nil {
			t.Fatalf("write schema stub: %v", err)
		}
	}

	install := exec.Command("npm", "install", "--no-fund", "--no-audit", "--ignore-scripts")
	install.Dir = frontend
	install.Env = append(os.Environ(), "npm_config_update_notifier=false")
	if out, err := install.CombinedOutput(); err != nil {
		t.Fatalf("npm install: %v\n%s", err, out)
	}
	//nolint:gosec // frontend is a generated tree; tsc args are fixed
	tsc := exec.Command("npx", "--no-install", "tsc", "--noEmit")
	tsc.Dir = frontend
	if out, err := tsc.CombinedOutput(); err != nil {
		t.Fatalf("npx tsc --noEmit: %v\n%s", err, out)
	}
	if !stubGeneratedSchema {
		//nolint:gosec // frontend is a generated tree; vite args are fixed
		build := exec.Command("npx", "--no-install", "vite", "build")
		build.Dir = frontend
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("npx vite build: %v\n%s", err, out)
		}
	}
}

func hasNode() bool {
	if _, err := exec.LookPath("npx"); err != nil {
		return false
	}
	if _, err := exec.LookPath("npm"); err != nil {
		return false
	}
	return true
}

func requireNode(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("npx openapi-typescript path handling differs on Windows")
	}
	if !hasNode() {
		t.Skip("npx/npm not available")
	}
}
