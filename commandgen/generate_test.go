package commandgen

import (
	"bytes"
	"context"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/resourcegen"
	"github.com/gombit-dev/gombit/scaffold"
)

func TestGenerateGreetCommand(t *testing.T) {
	appDir := scaffoldApp(t)

	stdout := new(bytes.Buffer)
	err := Generate(context.Background(), Options{
		WorkDir: appDir,
		Name:    "greet",
		Stdout:  stdout,
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if !strings.Contains(stdout.String(), "create internal/commands/greet.go") {
		t.Fatalf("stdout = %q, want created greet.go", stdout.String())
	}
	if !strings.Contains(stdout.String(), "internal/commands/commands.go") {
		t.Fatalf("stdout = %q, want commands.go", stdout.String())
	}
	if !strings.Contains(stdout.String(), "cmd/gombit/main.go") {
		t.Fatalf("stdout = %q, want cmd/gombit/main.go", stdout.String())
	}

	greetPath := filepath.Join(appDir, "internal", "commands", "greet.go")
	greetSrc := readFile(t, greetPath)
	if !strings.Contains(greetSrc, GeneratedBanner) {
		t.Fatal("greet.go missing generated banner")
	}
	if !strings.Contains(greetSrc, "func NewGreetCommand()") {
		t.Fatal("greet.go missing NewGreetCommand")
	}
	if strings.Contains(greetSrc, "regexp") {
		t.Fatal("generated command imported regexp")
	}
	fset := token.NewFileSet()
	if _, err := parser.ParseFile(fset, greetPath, greetSrc, 0); err != nil {
		t.Fatalf("generated greet.go is not valid Go: %v", err)
	}

	commandsSrc := readFile(t, filepath.Join(appDir, "internal", "commands", "commands.go"))
	count, err := CountConstructorCalls([]byte(commandsSrc), "NewGreetCommand")
	if err != nil {
		t.Fatalf("CountConstructorCalls: %v", err)
	}
	if count != 1 {
		t.Fatalf("NewGreetCommand count = %d, want 1\n%s", count, commandsSrc)
	}
	if !strings.Contains(commandsSrc, "func RegisterCommands") {
		t.Fatal("commands.go missing RegisterCommands")
	}
	if !strings.Contains(commandsSrc, "cli.AddCommand") {
		t.Fatal("commands.go does not use cli.AddCommand")
	}

	mainSrc := readFile(t, filepath.Join(appDir, "cmd", "gombit", "main.go"))
	regCount, err := CountRegisterCalls([]byte(mainSrc), "commands")
	if err != nil {
		t.Fatalf("CountRegisterCalls: %v", err)
	}
	if regCount != 1 {
		t.Fatalf("commands.RegisterCommands count = %d, want 1\n%s", regCount, mainSrc)
	}
	if !strings.Contains(mainSrc, "product.RegisterCommands") {
		t.Fatal("cmd/gombit lost product.RegisterCommands")
	}

	stdout.Reset()
	err = Generate(context.Background(), Options{
		WorkDir: appDir,
		Name:    "greet",
		Stdout:  stdout,
	})
	if err != nil {
		t.Fatalf("re-run Generate() error = %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("idempotent re-run stdout = %q, want empty", stdout.String())
	}
	commandsSrc = readFile(t, filepath.Join(appDir, "internal", "commands", "commands.go"))
	count, err = CountConstructorCalls([]byte(commandsSrc), "NewGreetCommand")
	if err != nil {
		t.Fatalf("CountConstructorCalls re-run: %v", err)
	}
	if count != 1 {
		t.Fatalf("re-run duplicated NewGreetCommand: %d", count)
	}
	mainSrc = readFile(t, filepath.Join(appDir, "cmd", "gombit", "main.go"))
	regCount, err = CountRegisterCalls([]byte(mainSrc), "commands")
	if err != nil {
		t.Fatalf("CountRegisterCalls re-run: %v", err)
	}
	if regCount != 1 {
		t.Fatalf("re-run duplicated commands.RegisterCommands: %d", regCount)
	}

	if err := os.WriteFile(greetPath, []byte("package commands\n\n// edited by user\n"), 0o600); err != nil {
		t.Fatalf("edit greet.go: %v", err)
	}
	err = Generate(context.Background(), Options{WorkDir: appDir, Name: "greet"})
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("clobber error = %v, want --force", err)
	}
	if !strings.Contains(readFile(t, greetPath), "edited by user") {
		t.Fatal("user greet.go was overwritten")
	}

	err = Generate(context.Background(), Options{WorkDir: appDir, Name: "greet", Force: true})
	if err != nil {
		t.Fatalf("Generate(--force) error = %v", err)
	}
}

func TestGenerateDryRunWritesNothing(t *testing.T) {
	appDir := scaffoldApp(t)
	before := snapshotInternal(t, appDir)
	stdout := new(bytes.Buffer)
	err := Generate(context.Background(), Options{
		WorkDir: appDir,
		Name:    "hello",
		DryRun:  true,
		Stdout:  stdout,
	})
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if !strings.Contains(stdout.String(), "internal/commands/hello.go") {
		t.Fatalf("stdout = %q, want hello.go", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(appDir, "internal", "commands")); !os.IsNotExist(err) {
		t.Fatal("dry-run wrote internal/commands")
	}
	after := snapshotInternal(t, appDir)
	if after != before {
		t.Fatal("dry-run changed internal/")
	}
}

func TestGeneratePackageFlag(t *testing.T) {
	appDir := scaffoldApp(t)
	err := Generate(context.Background(), Options{
		WorkDir: appDir,
		Name:    "hello",
		Package: "hello",
	})
	if err != nil {
		t.Fatalf("Generate(--package hello): %v", err)
	}
	if _, err := os.Stat(filepath.Join(appDir, "internal", "hello", "hello.go")); err != nil {
		t.Fatalf("missing hello package command: %v", err)
	}
	mainSrc := readFile(t, filepath.Join(appDir, "cmd", "gombit", "main.go"))
	count, err := CountRegisterCalls([]byte(mainSrc), "hello")
	if err != nil {
		t.Fatalf("CountRegisterCalls: %v", err)
	}
	if count != 1 {
		t.Fatalf("hello.RegisterCommands count = %d, want 1\n%s", count, mainSrc)
	}
	helloCommands := readFile(t, filepath.Join(appDir, "internal", "hello", "commands.go"))
	if !strings.Contains(helloCommands, "func RegisterCommands") {
		t.Fatal("internal/hello/commands.go missing RegisterCommands")
	}
}

func TestGenerateCommandInExistingResourcePackage(t *testing.T) {
	appDir := scaffoldApp(t)
	err := resourcegen.Generate(context.Background(), resourcegen.Options{
		WorkDir:  appDir,
		Name:     "Book",
		Fields:   []string{"title:string:required"},
		Stdout:   ioDiscard{},
		Stderr:   ioDiscard{},
		AtlasBin: "gombit-atlas-not-installed",
	})
	if err != nil {
		t.Fatalf("make resource Book: %v", err)
	}
	routesSrc := readFile(t, filepath.Join(appDir, "internal", "book", "routes.go"))
	if !strings.Contains(routesSrc, "func Register(app *framework.App)") {
		t.Fatal("book routes.go lost Register")
	}

	err = Generate(context.Background(), Options{
		WorkDir: appDir,
		Name:    "greet",
		Package: "book",
	})
	if err != nil {
		t.Fatalf("make command greet --package book: %v", err)
	}

	commandsSrc := readFile(t, filepath.Join(appDir, "internal", "book", "commands.go"))
	if !strings.Contains(commandsSrc, "func RegisterCommands(root *cli.Command)") {
		t.Fatalf("book commands.go missing RegisterCommands:\n%s", commandsSrc)
	}
	if strings.Contains(commandsSrc, "func Register(root") {
		t.Fatal("book commands.go declared colliding Register")
	}
	ctor, err := CountConstructorCalls([]byte(commandsSrc), "NewGreetCommand")
	if err != nil {
		t.Fatalf("CountConstructorCalls: %v", err)
	}
	if ctor != 1 {
		t.Fatalf("NewGreetCommand count = %d, want 1\n%s", ctor, commandsSrc)
	}

	routesSrc = readFile(t, filepath.Join(appDir, "internal", "book", "routes.go"))
	if !strings.Contains(routesSrc, "func Register(app *framework.App)") {
		t.Fatal("book routes.go Register was overwritten")
	}

	mainSrc := readFile(t, filepath.Join(appDir, "cmd", "gombit", "main.go"))
	count, err := CountRegisterCalls([]byte(mainSrc), "book")
	if err != nil {
		t.Fatalf("CountRegisterCalls: %v", err)
	}
	if count != 1 {
		t.Fatalf("book.RegisterCommands count = %d, want 1\n%s", count, mainSrc)
	}
	if strings.Contains(mainSrc, "book.Register(root)") {
		t.Fatalf("cmd/gombit called colliding book.Register:\n%s", mainSrc)
	}

	bookDir := filepath.Join(appDir, "internal", "book")
	entries, err := os.ReadDir(bookDir)
	if err != nil {
		t.Fatalf("readdir book: %v", err)
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		path := filepath.Join(bookDir, entry.Name())
		if _, err := parser.ParseFile(fset, path, nil, 0); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
	}
}

func TestGenerateRefusesFrameworkLayout(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), ".."))
	err := Generate(context.Background(), Options{
		WorkDir: root,
		Name:    "greet",
		DryRun:  true,
	})
	if err == nil || !strings.Contains(err.Error(), "not a gombit application") {
		t.Fatalf("error = %v, want refused framework layout", err)
	}
}

func TestGenerateRefusesExecuteOnlyGombitMain(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module github.com/example/demo\n\ngo 1.22\n")
	writeFile(t, filepath.Join(dir, "gombit.yaml"), "name: demo\n")
	writeFile(t, filepath.Join(dir, "cmd", "server", "main.go"), "package main\n\nfunc main() {}\n")
	writeFile(t, filepath.Join(dir, "cmd", "gombit", "main.go"), fixtureFrameworkMain)
	err := Generate(context.Background(), Options{
		WorkDir: dir,
		Name:    "greet",
		DryRun:  true,
	})
	if err == nil || !strings.Contains(err.Error(), "registration point") {
		t.Fatalf("error = %v, want registration point (cli.Execute is not an anchor)", err)
	}
}

func TestGenerateRejectsReservedNames(t *testing.T) {
	tests := []struct {
		name string
		pkg  string
		want string
	}{
		{name: "new", want: "collides"},
		{name: "make", want: "collides"},
		{name: "build", want: "collides"},
		{name: "version", want: "collides"},
		{name: "createsuperuser", want: "collides"},
		{name: "platform", pkg: "platform", want: "reserved"},
		{name: "greet", pkg: "product", want: "reserved"},
		{name: "greet", pkg: "web", want: "reserved"},
		{name: "commands", want: "reserved file"},
	}
	for _, tt := range tests {
		t.Run(tt.name+"/"+tt.pkg, func(t *testing.T) {
			_, err := parseCommandName(tt.name, tt.pkg)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestPackageDoesNotImportRegexp(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		src := readFile(t, filepath.Join(dir, entry.Name()))
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, entry.Name(), src, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		for _, imp := range parsed.Imports {
			if imp.Path != nil && strings.Trim(imp.Path.Value, `"`) == "regexp" {
				t.Fatalf("%s imports regexp; generators must use go/ast", entry.Name())
			}
		}
	}
}

func scaffoldApp(t *testing.T) string {
	t.Helper()
	workDir := t.TempDir()
	if err := scaffold.Generate(context.Background(), scaffold.Options{
		Name:     "demo",
		Database: "sqlite",
		WorkDir:  workDir,
		Stdout:   ioDiscard{},
	}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	return filepath.Join(workDir, "demo")
}

func snapshotInternal(t *testing.T, appDir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(filepath.Join(appDir, "internal"), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(appDir, path)
		if relErr != nil {
			return relErr
		}
		b.WriteString(filepath.ToSlash(rel))
		if !info.IsDir() {
			b.WriteByte(':')
			b.WriteString(readFile(t, path))
		}
		b.WriteByte('\n')
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal: %v", err)
	}
	return b.String()
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- test fixture path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestReadModulePathStripsLineComment(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module github.com/example/demo // app\n"), 0o600)
	if err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	got, err := readModulePath(dir)
	if err != nil {
		t.Fatalf("readModulePath() error = %v", err)
	}
	if got != "github.com/example/demo" {
		t.Fatalf("readModulePath() = %q, want github.com/example/demo", got)
	}
}

func TestGenerateStripsGoModLineCommentFromImports(t *testing.T) {
	appDir := scaffoldApp(t)
	path := filepath.Join(appDir, "go.mod")
	data := readFile(t, path)
	lines := strings.Split(data, "\n")
	found := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "module ") {
			lines[i] = strings.TrimSpace(line) + " // app"
			found = true
			break
		}
	}
	if !found {
		t.Fatal("go.mod missing module line")
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	err := Generate(context.Background(), Options{
		WorkDir: appDir,
		Name:    "greet",
		Stdout:  ioDiscard{},
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	mod, err := readModulePath(appDir)
	if err != nil {
		t.Fatalf("readModulePath: %v", err)
	}
	if strings.Contains(mod, "//") {
		t.Fatalf("readModulePath() = %q, want comment stripped", mod)
	}
	wantImport := `"` + mod + `/internal/commands"`
	mainSrc := readFile(t, filepath.Join(appDir, "cmd", "gombit", "main.go"))
	if !strings.Contains(mainSrc, wantImport) {
		t.Fatalf("cmd/gombit/main.go missing import %s:\n%s", wantImport, mainSrc)
	}
	if strings.Contains(mainSrc, "// app/") {
		t.Fatalf("cmd/gombit/main.go kept go.mod comment in an import:\n%s", mainSrc)
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) {
	return len(p), nil
}
