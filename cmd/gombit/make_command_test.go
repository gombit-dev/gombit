package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/commandgen"
)

func TestRunMakeCommandGreetRunnable(t *testing.T) {
	workDir := t.TempDir()
	chdir(t, workDir)

	if err := run(context.Background(), []string{"new", "demo", "--database", "sqlite", "--skip-tidy"}, ioDiscard{}, ioDiscard{}); err != nil {
		t.Fatalf("gombit new: %v", err)
	}
	dest := filepath.Join(workDir, "demo")
	appendReplace(t, dest)
	chdir(t, dest)

	stdout := new(bytes.Buffer)
	err := run(context.Background(), []string{"make", "command", "greet"}, stdout, ioDiscard{})
	if err != nil {
		t.Fatalf("make command: %v; stdout=%q", err, stdout.String())
	}
	if !strings.Contains(stdout.String(), "internal/commands/greet.go") {
		t.Fatalf("stdout = %q, want greet.go", stdout.String())
	}

	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dest
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}

	greet := exec.Command("go", "run", "./cmd/gombit", "greet")
	greet.Dir = dest
	out, err := greet.CombinedOutput()
	if err != nil {
		t.Fatalf("go run ./cmd/gombit greet: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "greet: ok") {
		t.Fatalf("greet output = %q, want greet: ok", out)
	}

	help := exec.Command("go", "run", "./cmd/gombit", "--help")
	help.Dir = dest
	out, err = help.CombinedOutput()
	if err != nil {
		t.Fatalf("go run ./cmd/gombit --help: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "greet") {
		t.Fatalf("app --help missing greet:\n%s", out)
	}

	mainSrc := readFileString(t, filepath.Join(dest, "cmd", "gombit", "main.go"))
	count, err := commandgen.CountRegisterCalls([]byte(mainSrc), "commands")
	if err != nil {
		t.Fatalf("CountRegisterCalls: %v", err)
	}
	if count != 1 {
		t.Fatalf("commands.RegisterCommands count = %d, want 1\n%s", count, mainSrc)
	}

	err = run(context.Background(), []string{"make", "command", "greet"}, ioDiscard{}, ioDiscard{})
	if err != nil {
		t.Fatalf("re-run make command: %v", err)
	}
	mainSrc = readFileString(t, filepath.Join(dest, "cmd", "gombit", "main.go"))
	count, err = commandgen.CountRegisterCalls([]byte(mainSrc), "commands")
	if err != nil {
		t.Fatalf("CountRegisterCalls re-run: %v", err)
	}
	if count != 1 {
		t.Fatalf("re-run duplicated RegisterCommands: %d", count)
	}

	dryStdout := new(bytes.Buffer)
	err = run(context.Background(), []string{"make", "command", "hello", "--dry-run"}, dryStdout, ioDiscard{})
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if !strings.Contains(dryStdout.String(), "hello.go") {
		t.Fatalf("dry-run stdout = %q", dryStdout.String())
	}
	if _, err := os.Stat(filepath.Join(dest, "internal", "commands", "hello.go")); !os.IsNotExist(err) {
		t.Fatal("dry-run wrote hello.go")
	}
}

func TestRunMakeCommandInResourcePackageCompiles(t *testing.T) {
	workDir := t.TempDir()
	chdir(t, workDir)

	if err := run(context.Background(), []string{"new", "demo", "--database", "sqlite", "--skip-tidy"}, ioDiscard{}, ioDiscard{}); err != nil {
		t.Fatalf("gombit new: %v", err)
	}
	dest := filepath.Join(workDir, "demo")
	appendReplace(t, dest)
	chdir(t, dest)

	stdout := new(bytes.Buffer)
	err := run(context.Background(), []string{"make", "resource", "Book", "title:string:required"}, stdout, ioDiscard{})
	if err != nil {
		t.Fatalf("make resource Book: %v; stdout=%q", err, stdout.String())
	}

	stdout.Reset()
	err = run(context.Background(), []string{"make", "command", "greet", "--package", "book"}, stdout, ioDiscard{})
	if err != nil {
		t.Fatalf("make command greet --package book: %v; stdout=%q", err, stdout.String())
	}
	if !strings.Contains(stdout.String(), "internal/book/commands.go") {
		t.Fatalf("stdout = %q, want internal/book/commands.go", stdout.String())
	}
	if strings.Contains(stdout.String(), "register.go") {
		t.Fatalf("stdout = %q, did not expect register.go", stdout.String())
	}

	commandsSrc := readFileString(t, filepath.Join(dest, "internal", "book", "commands.go"))
	if !strings.Contains(commandsSrc, "func RegisterCommands") {
		t.Fatal("book commands.go missing RegisterCommands")
	}
	routesSrc := readFileString(t, filepath.Join(dest, "internal", "book", "routes.go"))
	if !strings.Contains(routesSrc, "func Register(app") {
		t.Fatal("book routes.go lost Register")
	}
	mainSrc := readFileString(t, filepath.Join(dest, "cmd", "gombit", "main.go"))
	count, err := commandgen.CountRegisterCalls([]byte(mainSrc), "book")
	if err != nil {
		t.Fatalf("CountRegisterCalls: %v", err)
	}
	if count != 1 {
		t.Fatalf("book.RegisterCommands count = %d, want 1\n%s", count, mainSrc)
	}

	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dest
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}
	build := exec.Command("go", "build", "./...")
	build.Dir = dest
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build after make resource Book + make command --package book: %v\n%s", err, out)
	}

	greet := exec.Command("go", "run", "./cmd/gombit", "greet")
	greet.Dir = dest
	out, err := greet.CombinedOutput()
	if err != nil {
		t.Fatalf("go run ./cmd/gombit greet: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "greet: ok") {
		t.Fatalf("greet output = %q, want greet: ok", out)
	}
}

func TestRunMakeCommandRefusesFrameworkLayout(t *testing.T) {
	workDir := t.TempDir()
	chdir(t, workDir)
	if err := os.MkdirAll(filepath.Join(workDir, "cmd", "gombit"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "go.mod"), []byte("module github.com/example/framework\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	mainSrc := "package main\n\nimport (\n\t\"context\"\n\t\"io\"\n\n\t\"github.com/gombit-dev/gombit/cli\"\n)\n\nfunc run(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) error {\n\treturn cli.Execute(ctx, args, stdout, stderr)\n}\n"
	if err := os.WriteFile(filepath.Join(workDir, "cmd", "gombit", "main.go"), []byte(mainSrc), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}

	err := run(context.Background(), []string{"make", "command", "greet"}, ioDiscard{}, ioDiscard{})
	if err == nil || !strings.Contains(err.Error(), "not a gombit application") {
		t.Fatalf("error = %v, want refused framework layout", err)
	}
	if _, statErr := os.Stat(filepath.Join(workDir, "internal")); !os.IsNotExist(statErr) {
		t.Fatal("refused generate still wrote internal/")
	}
}

func TestRunMakeCommandHelp(t *testing.T) {
	stdout := new(bytes.Buffer)
	err := run(context.Background(), []string{"make", "command", "--help"}, stdout, ioDiscard{})
	if err != nil {
		t.Fatalf("make command --help: %v", err)
	}
	got := stdout.String()
	if !strings.Contains(got, "--dry-run") || !strings.Contains(got, "--force") {
		t.Fatalf("make command help missing flags:\n%s", got)
	}
	if !strings.Contains(got, "--package") {
		t.Fatalf("make command help missing --package:\n%s", got)
	}
}
