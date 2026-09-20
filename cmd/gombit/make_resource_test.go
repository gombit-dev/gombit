package main

import (
	"bytes"
	"context"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/migrations"
	"github.com/gombit-dev/gombit/resourcegen"
)

func TestRunMakeResourceBookCompiles(t *testing.T) {
	workDir := t.TempDir()
	chdir(t, workDir)

	err := run(context.Background(), []string{"new", "demo", "--database", "sqlite", "--skip-tidy"}, ioDiscard{}, ioDiscard{})
	if err != nil {
		t.Fatalf("gombit new: %v", err)
	}
	dest := filepath.Join(workDir, "demo")
	appendReplace(t, dest)

	chdir(t, dest)
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	err = run(context.Background(), []string{"make", "resource", "Book", "title:string:required"}, stdout, stderr)
	if err != nil {
		t.Fatalf("make resource: %v; stderr=%q stdout=%q", err, stderr.String(), stdout.String())
	}

	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dest
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}
	build := exec.Command("go", "build", "./...")
	build.Dir = dest
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	mainSrc := readFileString(t, filepath.Join(dest, "cmd", "server", "main.go"))
	count, err := resourcegen.CountRegisterCalls([]byte(mainSrc), "book")
	if err != nil {
		t.Fatalf("CountRegisterCalls: %v", err)
	}
	if count != 1 {
		t.Fatalf("book.Register count = %d, want 1\n%s", count, mainSrc)
	}

	fset := token.NewFileSet()
	modelPath := filepath.Join(dest, "internal", "book", "book.go")
	if _, err := parser.ParseFile(fset, modelPath, nil, 0); err != nil {
		t.Fatalf("generated model parse: %v", err)
	}
	mod := modulePathFromGoMod(t, dest)
	if _, err := migrations.ParseModel(mod + "/internal/book.Book"); err != nil {
		t.Fatalf("ParseModel: %v", err)
	}

	err = run(context.Background(), []string{"make", "resource", "Book", "title:string:required"}, ioDiscard{}, ioDiscard{})
	if err != nil {
		t.Fatalf("re-run make resource: %v", err)
	}
	mainSrc = readFileString(t, filepath.Join(dest, "cmd", "server", "main.go"))
	count, err = resourcegen.CountRegisterCalls([]byte(mainSrc), "book")
	if err != nil {
		t.Fatalf("CountRegisterCalls re-run: %v", err)
	}
	if count != 1 {
		t.Fatalf("re-run duplicated Register: %d", count)
	}

	handlerPath := filepath.Join(dest, "internal", "book", "handler.go")
	if err := os.WriteFile(handlerPath, []byte("package book\n\n// edited by user\n"), 0o600); err != nil {
		t.Fatalf("edit handler: %v", err)
	}
	err = run(context.Background(), []string{"make", "resource", "Book", "title:string:required"}, ioDiscard{}, ioDiscard{})
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("clobber error = %v, want --force", err)
	}
	if !strings.Contains(readFileString(t, handlerPath), "edited by user") {
		t.Fatal("user handler.go was overwritten")
	}

	err = run(context.Background(), []string{"make", "resource", "Book", "title:string:required", "--force"}, ioDiscard{}, ioDiscard{})
	if err != nil {
		t.Fatalf("make resource --force: %v", err)
	}

	dryStdout := new(bytes.Buffer)
	before, err := os.ReadDir(filepath.Join(dest, "internal"))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	err = run(context.Background(), []string{"make", "resource", "Invoice", "--service", "--repo", "--dry-run"}, dryStdout, ioDiscard{})
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if !strings.Contains(dryStdout.String(), "service.go") || !strings.Contains(dryStdout.String(), "repo.go") {
		t.Fatalf("dry-run stdout = %q", dryStdout.String())
	}
	if _, err := os.Stat(filepath.Join(dest, "internal", "invoice")); !os.IsNotExist(err) {
		t.Fatal("dry-run wrote invoice")
	}
	after, err := os.ReadDir(filepath.Join(dest, "internal"))
	if err != nil {
		t.Fatalf("readdir after dry-run: %v", err)
	}
	if len(after) != len(before) {
		t.Fatal("dry-run changed internal/")
	}

	err = run(context.Background(), []string{"make", "resource", "Invoice", "--service", "--repo"}, ioDiscard{}, ioDiscard{})
	if err != nil {
		t.Fatalf("service/repo: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "internal", "invoice", "service.go")); err != nil {
		t.Fatalf("missing service.go: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "internal", "invoice", "repo.go")); err != nil {
		t.Fatalf("missing repo.go: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "internal", "book", "service.go")); !os.IsNotExist(err) {
		t.Fatal("default Book generate wrote service.go")
	}

	build = exec.Command("go", "build", "./...")
	build.Dir = dest
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build after invoice: %v\n%s", err, out)
	}
}

func TestRunMakeResourceDryRunOnFreshApp(t *testing.T) {
	workDir := t.TempDir()
	chdir(t, workDir)
	if err := run(context.Background(), []string{"new", "demo", "--database", "sqlite", "--skip-tidy"}, ioDiscard{}, ioDiscard{}); err != nil {
		t.Fatalf("new: %v", err)
	}
	chdir(t, filepath.Join(workDir, "demo"))
	stdout := new(bytes.Buffer)
	err := run(context.Background(), []string{"make", "resource", "Widget", "name:string:required", "price:int", "--dry-run"}, stdout, ioDiscard{})
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "demo", "internal", "widget")); !os.IsNotExist(err) {
		t.Fatal("dry-run wrote widget package")
	}
	if !strings.Contains(stdout.String(), "internal/widget/widget.go") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestRunMakeHelp(t *testing.T) {
	stdout := new(bytes.Buffer)
	err := run(context.Background(), []string{"make", "--help"}, stdout, ioDiscard{})
	if err != nil {
		t.Fatalf("make --help: %v", err)
	}
	if !strings.Contains(stdout.String(), "resource") {
		t.Fatalf("make help missing resource:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "command") {
		t.Fatalf("make help missing command:\n%s", stdout.String())
	}
}

func TestRunRejectsUnknownMakeSubcommand(t *testing.T) {
	stderr := new(bytes.Buffer)
	err := run(context.Background(), []string{"make", "unknown"}, ioDiscard{}, stderr)
	if err == nil {
		t.Fatal("error = nil, want unknown subcommand")
	}
	if !strings.Contains(err.Error(), "unknown subcommand") {
		t.Fatalf("error = %q", err)
	}
	if !strings.Contains(stderr.String(), "resource") {
		t.Fatalf("make usage = %q, want resource", stderr.String())
	}
	if !strings.Contains(stderr.String(), "command") {
		t.Fatalf("make usage = %q, want command", stderr.String())
	}
}

func appendReplace(t *testing.T, dest string) {
	t.Helper()
	goModPath := filepath.Join(dest, "go.mod")
	// #nosec G304 -- test temp go.mod
	mod, err := os.OpenFile(goModPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open go.mod: %v", err)
	}
	if _, err := mod.WriteString("\nreplace github.com/gombit-dev/gombit => " + cmdModuleRoot(t) + "\n"); err != nil {
		_ = mod.Close()
		t.Fatalf("write replace: %v", err)
	}
	if err := mod.Close(); err != nil {
		t.Fatalf("close go.mod: %v", err)
	}
}

func modulePathFromGoMod(t *testing.T, dir string) string {
	t.Helper()
	for _, line := range strings.Split(readFileString(t, filepath.Join(dir, "go.mod")), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module "))
		}
	}
	t.Fatal("go.mod missing module path")
	return ""
}
