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
	// make resource's phase-2 preflight runs go in a read-only module mode (it must
	// not mutate go.mod/go.sum), so the module has to be tidy first — a fresh
	// --skip-tidy app is not.
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dest
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}

	chdir(t, dest)
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	err = run(context.Background(), []string{"make", "resource", "Book", "title:string:required", "--skip-migrations"}, stdout, stderr)
	if err != nil {
		t.Fatalf("make resource: %v; stderr=%q stdout=%q", err, stderr.String(), stdout.String())
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

	err = run(context.Background(), []string{"make", "resource", "Book", "title:string:required", "--skip-migrations"}, ioDiscard{}, ioDiscard{})
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

	// The model is human-owned (seed-once): a valid local edit survives a re-run
	// (no error, no clobber), and generation still succeeds from the edited model.
	edited := readFileString(t, modelPath) + "\n// edited by user\n"
	if err := os.WriteFile(modelPath, []byte(edited), 0o600); err != nil {
		t.Fatalf("edit model: %v", err)
	}
	err = run(context.Background(), []string{"make", "resource", "Book", "title:string:required", "--skip-migrations"}, ioDiscard{}, ioDiscard{})
	if err != nil {
		t.Fatalf("re-run over an edited model must succeed (seed-once), got: %v", err)
	}
	if !strings.Contains(readFileString(t, modelPath), "edited by user") {
		t.Fatal("re-run clobbered the human-owned model without --force")
	}

	// --force re-scaffolds the model from the CLI spec, discarding the local edit.
	err = run(context.Background(), []string{"make", "resource", "Book", "title:string:required", "--force", "--skip-migrations"}, ioDiscard{}, ioDiscard{})
	if err != nil {
		t.Fatalf("make resource --force: %v", err)
	}
	if strings.Contains(readFileString(t, modelPath), "edited by user") {
		t.Fatal("--force did not re-scaffold the model")
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

	err = run(context.Background(), []string{"make", "resource", "Invoice", "--service", "--repo", "--skip-migrations"}, ioDiscard{}, ioDiscard{})
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
	dest := filepath.Join(workDir, "demo")
	appendReplace(t, dest)
	// The overlay-backed dry-run runs Program Mode (go run) to validate the pending
	// model, so the framework must resolve; tidy first so that go run does not have
	// to touch go.mod/go.sum (keeping "dry-run writes nothing" honest).
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dest
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}
	chdir(t, dest)
	// A dry-run must not mutate module metadata either — the preflight runs go in a
	// read-only module mode, so go.mod/go.sum are untouched.
	goModBefore := readFileString(t, filepath.Join(dest, "go.mod"))
	goSumBefore := readFileString(t, filepath.Join(dest, "go.sum"))
	stdout := new(bytes.Buffer)
	err := run(context.Background(), []string{"make", "resource", "Widget", "name:string:required", "price:int", "--dry-run"}, stdout, ioDiscard{})
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "internal", "widget")); !os.IsNotExist(err) {
		t.Fatal("dry-run wrote widget package")
	}
	if readFileString(t, filepath.Join(dest, "go.mod")) != goModBefore {
		t.Fatal("dry-run mutated go.mod")
	}
	if readFileString(t, filepath.Join(dest, "go.sum")) != goSumBefore {
		t.Fatal("dry-run mutated go.sum")
	}
	out := stdout.String()
	// The dry-run derives its preview from the real plan: the scaffold plus the exact
	// generator-owned files gombit generate produces.
	for _, want := range []string{
		"internal/widget/widget.go",
		"internal/widget/dto.gen.go",
		"internal/widget/handler.gen.go",
		"internal/widget/hooks.go",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run stdout = %q, want %q", out, want)
		}
	}
}

// A --force re-scaffold that changes the model must not commit if a preserved
// human-owned hook no longer compiles against the regenerated model. The phase-2
// preflight compiles the whole committed package (new model + new *.gen.go + the
// kept hook), so the mismatch fails closed before any file changes.
func TestRunMakeResourceForceValidatesPreservedHooks(t *testing.T) {
	workDir := t.TempDir()
	chdir(t, workDir)
	if err := run(context.Background(), []string{"new", "demo", "--database", "sqlite", "--skip-tidy"}, ioDiscard{}, ioDiscard{}); err != nil {
		t.Fatalf("new: %v", err)
	}
	dest := filepath.Join(workDir, "demo")
	appendReplace(t, dest)
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dest
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}
	chdir(t, dest)

	if err := run(context.Background(), []string{"make", "resource", "Book", "title:string:required", "--skip-migrations"}, ioDiscard{}, ioDiscard{}); err != nil {
		t.Fatalf("make resource Book: %v", err)
	}
	// Customize the seed-once hook to read a model field.
	hooksPath := filepath.Join(dest, "internal", "book", "hooks.go")
	hooks := readFileString(t, hooksPath)
	customized := strings.Replace(hooks, "\treturn nil", "\t_ = row.Title\n\treturn nil", 1)
	if customized == hooks {
		t.Fatalf("could not inject hook customization into:\n%s", hooks)
	}
	if err := os.WriteFile(hooksPath, []byte(customized), 0o600); err != nil {
		t.Fatalf("write customized hook: %v", err)
	}
	modelPath := filepath.Join(dest, "internal", "book", "book.go")
	modelBefore := readFileString(t, modelPath)

	// --force re-scaffold that drops Title (uses Name instead). The kept hook still
	// reads row.Title, so the preflight's final-tree compile must fail.
	stderr := new(bytes.Buffer)
	err := run(context.Background(), []string{"make", "resource", "Book", "name:string:required", "--force", "--skip-migrations"}, ioDiscard{}, stderr)
	if err == nil {
		t.Fatal("make resource --force must fail when a preserved hook no longer compiles against the regenerated model")
	}
	if got := readFileString(t, modelPath); got != modelBefore {
		t.Fatalf("a refused preflight must not rewrite the model, got:\n%s", got)
	}
	if !strings.Contains(readFileString(t, hooksPath), "row.Title") {
		t.Fatal("the customized hook must be left intact")
	}
	build := exec.Command("go", "build", "./...")
	build.Dir = dest
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("app must remain buildable after the refused --force: %v\n%s", err, out)
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

func TestRunMakeResourceFailsClosedWithoutAtlas(t *testing.T) {
	workDir := t.TempDir()
	chdir(t, workDir)
	if err := run(context.Background(), []string{"new", "demo", "--database", "sqlite", "--skip-tidy"}, ioDiscard{}, ioDiscard{}); err != nil {
		t.Fatalf("gombit new: %v", err)
	}
	dest := filepath.Join(workDir, "demo")
	chdir(t, dest)

	// No atlas resolvable: make resource must fail before writing anything, so the
	// committed tree never depends on whether Atlas happened to be installed (#300).
	// The atlas check is the first thing RunE does, before any go toolchain use, so
	// an empty PATH still surfaces the Atlas-required error rather than a go error.
	t.Setenv("PATH", "")
	stderr := new(bytes.Buffer)
	err := run(context.Background(), []string{"make", "resource", "Book", "title:string:required"}, ioDiscard{}, stderr)
	if err == nil {
		t.Fatal("make resource without atlas: error = nil, want an Atlas-required failure")
	}
	if !strings.Contains(err.Error(), "Atlas is required") || !strings.Contains(err.Error(), "--skip-migrations") {
		t.Fatalf("error = %v, want it to require Atlas and point at --skip-migrations", err)
	}
	// Atomic failure: nothing scaffolded.
	if _, statErr := os.Stat(filepath.Join(dest, "internal", "book")); !os.IsNotExist(statErr) {
		t.Fatalf("internal/book must not be created on a fail-closed run; stat err = %v", statErr)
	}
}
