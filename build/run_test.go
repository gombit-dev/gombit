package build

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRunRequiresEmbed(t *testing.T) {
	err := Run(context.Background(), Options{WorkDir: t.TempDir(), Embed: false})
	if err == nil {
		t.Fatal("Run() error = nil, want --embed required")
	}
	if !strings.Contains(err.Error(), "--embed") || !strings.Contains(err.Error(), "split") {
		t.Fatalf("Run() error = %q, want split default / --embed", err)
	}
}

func TestRunMissingFrontendPackageJSON(t *testing.T) {
	workDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workDir, "cmd", "server"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(workDir, "internal", "web", "embed.go"), "package web\n")
	err := Run(context.Background(), Options{WorkDir: workDir, Embed: true})
	if err == nil {
		t.Fatal("Run() error = nil, want missing frontend/package.json")
	}
	if !strings.Contains(err.Error(), "frontend/package.json") {
		t.Fatalf("Run() error = %q, want frontend/package.json", err)
	}
	if !strings.Contains(err.Error(), "backend-only") {
		t.Fatalf("Run() error = %q, want backend-only hint", err)
	}
}

func TestRunMissingServer(t *testing.T) {
	workDir := t.TempDir()
	writeFile(t, filepath.Join(workDir, "frontend", "package.json"), `{"name":"frontend"}`)
	writeFile(t, filepath.Join(workDir, "internal", "web", "embed.go"), "package web\n")
	err := Run(context.Background(), Options{WorkDir: workDir, Embed: true})
	if err == nil {
		t.Fatal("Run() error = nil, want missing cmd/server")
	}
	if !strings.Contains(err.Error(), "cmd/server") {
		t.Fatalf("Run() error = %q, want cmd/server", err)
	}
}

func TestRunMissingEmbedHook(t *testing.T) {
	workDir := t.TempDir()
	writeFile(t, filepath.Join(workDir, "frontend", "package.json"), `{"name":"frontend"}`)
	if err := os.MkdirAll(filepath.Join(workDir, "cmd", "server"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	err := Run(context.Background(), Options{WorkDir: workDir, Embed: true})
	if err == nil {
		t.Fatal("Run() error = nil, want missing embed.go")
	}
	if !strings.Contains(err.Error(), "embed.go") {
		t.Fatalf("Run() error = %q, want embed.go", err)
	}
}

func TestRunDryRunPrintsPlanWithoutWriting(t *testing.T) {
	workDir := writeBuildApp(t)
	stdout := new(bytes.Buffer)
	err := Run(context.Background(), Options{
		WorkDir: workDir,
		Embed:   true,
		DryRun:  true,
		Out:     "bin/server",
		Stdout:  stdout,
		LookPath: func(file string) (string, error) {
			switch file {
			case "go", "npm":
				return file, nil
			default:
				return "", errors.New("not found")
			}
		},
		Command: func(name string, args ...string) *exec.Cmd {
			t.Fatalf("dry-run must not exec %s %v", name, args)
			return exec.Command("false")
		},
	})
	if err != nil {
		t.Fatalf("Run(dry-run) error = %v", err)
	}
	got := stdout.String()
	for _, want := range []string{"would run:", "npm install", "npm run build", "would copy:", "frontend/dist", "internal/web/static", "would compile:", "bin/server"} {
		if !strings.Contains(got, want) {
			t.Fatalf("dry-run stdout missing %q:\n%s", want, got)
		}
	}
	if _, err := os.Stat(filepath.Join(workDir, "frontend", "dist", "index.html")); !os.IsNotExist(err) {
		t.Fatal("dry-run wrote frontend/dist")
	}
	if _, err := os.Stat(filepath.Join(workDir, "bin", "server")); !os.IsNotExist(err) {
		t.Fatal("dry-run wrote bin/server")
	}
}

func TestRunDefaultOutGetsExeForWindowsTarget(t *testing.T) {
	t.Setenv("GOOS", "windows")
	workDir := writeBuildApp(t)
	writeFile(t, filepath.Join(workDir, "frontend", "node_modules", ".keep"), "")
	stdout := new(bytes.Buffer)
	err := Run(context.Background(), Options{
		WorkDir: workDir,
		Embed:   true,
		DryRun:  true,
		Stdout:  stdout,
		LookPath: func(file string) (string, error) {
			switch file {
			case "go", "npm":
				return file, nil
			default:
				return "", errors.New("not found")
			}
		},
		Command: func(name string, args ...string) *exec.Cmd {
			t.Fatalf("dry-run must not exec %s %v", name, args)
			return exec.Command("false")
		},
	})
	if err != nil {
		t.Fatalf("Run(dry-run) error = %v", err)
	}
	if got := stdout.String(); !strings.Contains(got, "bin/server.exe") {
		t.Fatalf("GOOS=windows default output did not compile to bin/server.exe:\n%s", got)
	}
}

func TestRunEmbedCopiesAndCompilesWithStubs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh stubs")
	}
	workDir := writeBuildApp(t)
	writeFile(t, filepath.Join(workDir, "frontend", "node_modules", ".keep"), "")
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)

	err := Run(context.Background(), Options{
		WorkDir: workDir,
		Embed:   true,
		Out:     "bin/server",
		Stdout:  stdout,
		Stderr:  stderr,
		LookPath: func(file string) (string, error) {
			switch file {
			case "go", "npm":
				return "/usr/bin/" + file, nil
			default:
				return "", errors.New("not found")
			}
		},
		Command: func(name string, args ...string) *exec.Cmd {
			joined := strings.Join(args, " ")
			switch {
			case strings.Contains(joined, "run build"):
				return exec.Command("sh", "-c", "mkdir -p dist/assets && printf '<html>built</html>' > dist/index.html && printf 'js' > dist/assets/app.js")
			case len(args) >= 3 && args[0] == "build" && args[1] == "-o":
				return exec.Command("sh", "-c", "mkdir -p bin && printf fake-binary > bin/server")
			default:
				return exec.Command("sh", "-c", "exit 1")
			}
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}

	if got := readFile(t, filepath.Join(workDir, "internal", "web", "static", "index.html")); got != "<html>built</html>" {
		t.Fatalf("collectstatic index.html = %q", got)
	}
	if got := readFile(t, filepath.Join(workDir, "internal", "web", "static", "assets", "app.js")); got != "js" {
		t.Fatalf("collectstatic app.js = %q", got)
	}
	if _, err := os.Stat(filepath.Join(workDir, "internal", "web", "embed.go")); err != nil {
		t.Fatalf("embed.go missing after build: %v", err)
	}
	if got := readFile(t, filepath.Join(workDir, "bin", "server")); got != "fake-binary" {
		t.Fatalf("compiled binary = %q", got)
	}
	out := stdout.String()
	for _, want := range []string{"create frontend/dist", "copy frontend/dist -> internal/web/static", "compile bin/server"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q:\n%s", want, out)
		}
	}
}

func TestRunFailsWhenViteOmitsIndex(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh stubs")
	}
	workDir := writeBuildApp(t)
	writeFile(t, filepath.Join(workDir, "frontend", "node_modules", ".keep"), "")
	err := Run(context.Background(), Options{
		WorkDir: workDir,
		Embed:   true,
		LookPath: func(file string) (string, error) {
			switch file {
			case "go", "npm":
				return file, nil
			default:
				return "", errors.New("not found")
			}
		},
		Command: func(name string, args ...string) *exec.Cmd {
			if strings.Contains(strings.Join(args, " "), "run build") {
				return exec.Command("sh", "-c", "mkdir -p dist && printf 'no-index' > dist/app.js")
			}
			return exec.Command("sh", "-c", "exit 0")
		},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want missing index.html")
	}
	if !strings.Contains(err.Error(), "index.html") {
		t.Fatalf("Run() error = %q, want index.html", err)
	}
}

func TestPlanFrontendPrefersPnpmLockfile(t *testing.T) {
	workDir := t.TempDir()
	writeFile(t, filepath.Join(workDir, "frontend", "pnpm-lock.yaml"), "lockfileVersion: 9\n")
	plan, err := planFrontendBuild(workDir, func(file string) (string, error) {
		if file == "pnpm" {
			return "/usr/bin/pnpm", nil
		}
		return "", errors.New("not found")
	})
	if err != nil {
		t.Fatalf("planFrontendBuild() error = %v", err)
	}
	if plan.Manager != "pnpm" {
		t.Fatalf("manager = %q, want pnpm", plan.Manager)
	}
	if strings.Join(plan.Args, " ") != "run build" {
		t.Fatalf("args = %v, want run build", plan.Args)
	}
}

func TestRunDryRunOmitsInstallWhenNodeModulesPresent(t *testing.T) {
	workDir := writeBuildApp(t)
	writeFile(t, filepath.Join(workDir, "frontend", "node_modules", ".keep"), "")
	stdout := new(bytes.Buffer)
	err := Run(context.Background(), Options{
		WorkDir: workDir,
		Embed:   true,
		DryRun:  true,
		Out:     "bin/server",
		Stdout:  stdout,
		LookPath: func(file string) (string, error) {
			switch file {
			case "go", "npm":
				return file, nil
			default:
				return "", errors.New("not found")
			}
		},
		Command: func(name string, args ...string) *exec.Cmd {
			t.Fatalf("dry-run must not exec %s %v", name, args)
			return exec.Command("false")
		},
	})
	if err != nil {
		t.Fatalf("Run(dry-run) error = %v", err)
	}
	got := stdout.String()
	if strings.Contains(got, "install") {
		t.Fatalf("dry-run with node_modules mentioned install:\n%s", got)
	}
	if !strings.Contains(got, "npm run build") {
		t.Fatalf("dry-run stdout missing npm run build:\n%s", got)
	}
}

func writeBuildApp(t *testing.T) string {
	t.Helper()
	workDir := t.TempDir()
	writeFile(t, filepath.Join(workDir, "frontend", "package.json"), `{"name":"frontend","scripts":{"build":"vite build"}}`)
	writeFile(t, filepath.Join(workDir, "internal", "web", "embed.go"), "package web\n")
	writeFile(t, filepath.Join(workDir, "internal", "web", "static", keepName), "")
	if err := os.MkdirAll(filepath.Join(workDir, "cmd", "server"), 0o750); err != nil {
		t.Fatalf("mkdir cmd/server: %v", err)
	}
	return workDir
}
