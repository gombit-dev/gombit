package generate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/migrations"
	"github.com/gombit-dev/gombit/resourcegen"
	"gorm.io/gorm"
)

// --- test scaffolding -------------------------------------------------------

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// newAppLayout writes the minimum a gombit app needs for Generate to accept it
// and enumerate one app resource (book): go.mod, cmd/server/main.go, and a
// database.go whose AutoMigrate references testapp/internal/book plus a
// framework model (which must be filtered out as not an app resource).
func newAppLayout(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module testapp\n\ngo 1.23\n")
	writeFile(t, filepath.Join(dir, "cmd", "server", "main.go"), "package main\n\nfunc main() {}\n")
	writeFile(t, filepath.Join(dir, "internal", "platform", "database.go"),
		"package platform\n\n"+
			"import (\n"+
			"\t\"github.com/gombit-dev/gombit/auth\"\n"+
			"\t\"testapp/internal/book\"\n"+
			")\n\n"+
			"func AutoMigrate(db anyDB) error {\n"+
			"\treturn db.AutoMigrate(&auth.User{}, &book.Book{})\n"+
			"}\n")
	// Mark book as a model-first resource so discovery targets it.
	writeFile(t, filepath.Join(dir, "internal", "book", resourcegen.ResourceMarkerFile), "")
	return dir
}

// fakeRunner stands in for `go run`: it records that it ran and writes canned
// artifact JSON to stdout, so the parent's plan/check/apply logic is tested
// without compiling or executing a loader.
type fakeRunner struct {
	out []byte
	err error
	ran bool
	dir string
}

func (f *fakeRunner) Run(ctx context.Context, dir, name string, args []string, stdout, stderr io.Writer) error {
	f.ran = true
	f.dir = dir
	if f.err != nil {
		return f.err
	}
	_, err := stdout.Write(f.out)
	return err
}

func artifactsJSON(t *testing.T, arts []resourcegen.GeneratedArtifact) []byte {
	t.Helper()
	b, err := json.Marshal(arts)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// bookArtifacts is canned loader output. Content is valid Go with a `package
// book` clause (matching the directory), since Generate now parses each existing
// file's package clause before writing.
func bookArtifacts() []resourcegen.GeneratedArtifact {
	return []resourcegen.GeneratedArtifact{
		{Path: "internal/book/dto.gen.go", Content: []byte("package book\n\n// dto v1\n"), Ownership: resourcegen.GeneratorOwned},
		{Path: "internal/book/handler.gen.go", Content: []byte("package book\n\n// handler v1\n"), Ownership: resourcegen.GeneratorOwned},
		{Path: "internal/book/hooks.go", Content: []byte("package book\n\n// hooks v1\n"), Ownership: resourcegen.SeedOnce},
	}
}

func runGenerate(t *testing.T, dir string, check, dryRun bool, runner commandRunner) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Generate(context.Background(), Options{
		WorkDir: dir, Check: check, DryRun: dryRun,
		Stdout: &out, Stderr: io.Discard, runner: runner,
	})
	return out.String(), err
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) // #nosec G304 -- test file under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// --- --check ----------------------------------------------------------------

func TestCheckPassesWhenGeneratedFilesMatch(t *testing.T) {
	dir := newAppLayout(t)
	arts := bookArtifacts()
	for _, a := range arts {
		writeFile(t, filepath.Join(dir, filepath.FromSlash(a.Path)), string(a.Content))
	}
	runner := &fakeRunner{out: artifactsJSON(t, arts)}
	if _, err := runGenerate(t, dir, true, false, runner); err != nil {
		t.Fatalf("check should pass when committed files match: %v", err)
	}
	if !runner.ran {
		t.Fatal("loader should have run")
	}
}

func TestCheckFailsWhenGeneratorOwnedDiffers(t *testing.T) {
	dir := newAppLayout(t)
	arts := bookArtifacts()
	for _, a := range arts {
		writeFile(t, filepath.Join(dir, filepath.FromSlash(a.Path)), string(a.Content))
	}
	// A stale committed handler.gen.go.
	writeFile(t, filepath.Join(dir, "internal", "book", "handler.gen.go"), "package book\n\n// handler OLD\n")
	_, err := runGenerate(t, dir, true, false, &fakeRunner{out: artifactsJSON(t, arts)})
	if err == nil {
		t.Fatal("check must fail on a stale generator-owned file")
	}
	if !strings.Contains(err.Error(), "handler.gen.go") {
		t.Fatalf("error should name the stale file, got: %v", err)
	}
	if !errors.Is(err, ErrStale) {
		t.Fatalf("error should wrap ErrStale (gombit db check tells staleness from failure by it), got: %v", err)
	}
}

func TestCheckFailsWhenGeneratorOwnedMissing(t *testing.T) {
	dir := newAppLayout(t)
	arts := bookArtifacts()
	// Only write the DTO; handler.gen.go is missing.
	writeFile(t, filepath.Join(dir, "internal", "book", "dto.gen.go"), "package book\n\n// dto v1\n")
	_, err := runGenerate(t, dir, true, false, &fakeRunner{out: artifactsJSON(t, arts)})
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("check must fail (missing) when a generator-owned file is absent, got: %v", err)
	}
}

// --check must ignore the human-owned hooks file entirely: a hooks file that
// differs from (or is absent versus) what the loader would seed is NOT drift.
func TestCheckIgnoresSeedOnceHooks(t *testing.T) {
	dir := newAppLayout(t)
	arts := bookArtifacts()
	writeFile(t, filepath.Join(dir, "internal", "book", "dto.gen.go"), "package book\n\n// dto v1\n")
	writeFile(t, filepath.Join(dir, "internal", "book", "handler.gen.go"), "package book\n\n// handler v1\n")
	// Hooks present but heavily customized — must not count as drift.
	writeFile(t, filepath.Join(dir, "internal", "book", "hooks.go"), "package book\n\n// hooks HAND EDITED\n")
	if _, err := runGenerate(t, dir, true, false, &fakeRunner{out: artifactsJSON(t, arts)}); err != nil {
		t.Fatalf("check must ignore the human-owned hooks file: %v", err)
	}
}

// --- write ------------------------------------------------------------------

func TestApplyWritesGeneratedAndSeedsHooks(t *testing.T) {
	dir := newAppLayout(t)
	arts := bookArtifacts()
	if _, err := runGenerate(t, dir, false, false, &fakeRunner{out: artifactsJSON(t, arts)}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := read(t, filepath.Join(dir, "internal", "book", "dto.gen.go")); got != "package book\n\n// dto v1\n" {
		t.Fatalf("dto not written: %q", got)
	}
	if got := read(t, filepath.Join(dir, "internal", "book", "handler.gen.go")); got != "package book\n\n// handler v1\n" {
		t.Fatalf("handler not written: %q", got)
	}
	if got := read(t, filepath.Join(dir, "internal", "book", "hooks.go")); got != "package book\n\n// hooks v1\n" {
		t.Fatalf("hooks not seeded: %q", got)
	}
}

// A seed-once file that already exists is never overwritten, even when its
// content differs from the default the generator would seed.
func TestApplyNeverOverwritesExistingHooks(t *testing.T) {
	dir := newAppLayout(t)
	hooksPath := filepath.Join(dir, "internal", "book", "hooks.go")
	writeFile(t, hooksPath, "package book\n\n// hooks HAND EDITED\n")
	if _, err := runGenerate(t, dir, false, false, &fakeRunner{out: artifactsJSON(t, bookArtifacts())}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := read(t, hooksPath); got != "package book\n\n// hooks HAND EDITED\n" {
		t.Fatalf("seed-once hooks must not be overwritten, got: %q", got)
	}
	// Generator-owned files ARE (re)written.
	if got := read(t, filepath.Join(dir, "internal", "book", "dto.gen.go")); got != "package book\n\n// dto v1\n" {
		t.Fatalf("generator-owned dto should be written: %q", got)
	}
}

// A stale generator-owned file is overwritten with the fresh content (no --force
// needed: the generator owns it).
func TestApplyOverwritesStaleGeneratorOwned(t *testing.T) {
	dir := newAppLayout(t)
	writeFile(t, filepath.Join(dir, "internal", "book", "dto.gen.go"), "package book\n\n// dto OLD\n")
	if _, err := runGenerate(t, dir, false, false, &fakeRunner{out: artifactsJSON(t, bookArtifacts())}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := read(t, filepath.Join(dir, "internal", "book", "dto.gen.go")); got != "package book\n\n// dto v1\n" {
		t.Fatalf("stale generator-owned file must be overwritten: %q", got)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	dir := newAppLayout(t)
	out, err := runGenerate(t, dir, false, true, &fakeRunner{out: artifactsJSON(t, bookArtifacts())})
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "internal", "book", "dto.gen.go")); !os.IsNotExist(err) {
		t.Fatal("dry-run must not write files")
	}
	if !strings.Contains(out, "would write internal/book/dto.gen.go") {
		t.Fatalf("dry-run should report the planned write, got: %s", out)
	}
}

// --- fail closed on the legacy layout --------------------------------------

func TestFailsClosedOnLegacyHandler(t *testing.T) {
	dir := newAppLayout(t)
	writeFile(t, filepath.Join(dir, "internal", "book", "handler.go"), "package book\n\n// legacy human-owned handler\n")
	runner := &fakeRunner{out: artifactsJSON(t, bookArtifacts())}
	_, err := runGenerate(t, dir, false, false, runner)
	if err == nil || !strings.Contains(err.Error(), "legacy handler layout") {
		t.Fatalf("must fail closed on the legacy handler layout, got: %v", err)
	}
	if runner.ran {
		t.Fatal("must fail BEFORE running the loader (no half-migration)")
	}
	// And nothing was written.
	if _, statErr := os.Stat(filepath.Join(dir, "internal", "book", "dto.gen.go")); !os.IsNotExist(statErr) {
		t.Fatal("nothing must be written on the legacy path")
	}
}

// A resource directory whose files declare a package name different from the
// directory basename must fail closed BEFORE the loader runs or anything is
// written — otherwise generate would write `package book` files next to a
// `package books` model and leave the app not compiling while reporting success.
func TestFailsClosedOnPackageMismatch(t *testing.T) {
	dir := newAppLayout(t)
	// Directory is "book" but the model declares package "books".
	writeFile(t, filepath.Join(dir, "internal", "book", "book.go"), "package books\n\ntype Book struct{}\n")
	runner := &fakeRunner{out: artifactsJSON(t, bookArtifacts())}
	_, err := runGenerate(t, dir, false, false, runner)
	if err == nil || !strings.Contains(err.Error(), "declares package") {
		t.Fatalf("must fail closed on a package/dir mismatch, got: %v", err)
	}
	if runner.ran {
		t.Fatal("must fail BEFORE running the loader")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "internal", "book", "dto.gen.go")); !os.IsNotExist(statErr) {
		t.Fatal("nothing must be written on the mismatch path")
	}
}

// --- resource discovery (marker-based) --------------------------------------

// A no-op when nothing is marked: an app with no resource marker yields nothing
// to do and never runs the loader, even if it has app-internal models.
func TestNoMarkedResourcesDoesNothing(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module testapp\n\ngo 1.23\n")
	writeFile(t, filepath.Join(dir, "cmd", "server", "main.go"), "package main\n\nfunc main() {}\n")
	writeFile(t, filepath.Join(dir, "internal", "platform", "database.go"),
		"package platform\n\nimport \"github.com/gombit-dev/gombit/auth\"\n\n"+
			"func AutoMigrate(db anyDB) error {\n\treturn db.AutoMigrate(&auth.User{})\n}\n")
	runner := &fakeRunner{out: artifactsJSON(t, bookArtifacts())}
	out, err := runGenerate(t, dir, false, false, runner)
	if err != nil {
		t.Fatalf("no marked resources should be a no-op, got: %v", err)
	}
	if runner.ran {
		t.Fatal("loader must not run when there are no marked resources")
	}
	if !strings.Contains(out, "no model-first resources") {
		t.Fatalf("expected a no-op message, got: %s", out)
	}
}

// An UNMARKED package (a persisted model in AutoMigrate with no resource marker —
// a join table, an audit-log model) is not a resource and is skipped: AutoMigrate
// membership alone never makes a model a generated-CRUD resource.
func TestUnmarkedPackageIsSkipped(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module testapp\n\ngo 1.23\n")
	writeFile(t, filepath.Join(dir, "cmd", "server", "main.go"), "package main\n\nfunc main() {}\n")
	writeFile(t, filepath.Join(dir, "internal", "platform", "database.go"),
		"package platform\n\nimport \"testapp/internal/audit\"\n\n"+
			"func AutoMigrate(db anyDB) error {\n\treturn db.AutoMigrate(&audit.Entry{})\n}\n")
	// audit is persisted but NOT marked as a resource.
	writeFile(t, filepath.Join(dir, "internal", "audit", "entry.go"), "package audit\n\ntype Entry struct{}\n")
	runner := &fakeRunner{out: artifactsJSON(t, bookArtifacts())}
	if _, err := runGenerate(t, dir, false, false, runner); err != nil {
		t.Fatalf("unmarked package should be skipped, got: %v", err)
	}
	if runner.ran {
		t.Fatal("loader must not run for an unmarked package")
	}
}

// A marked package with no AutoMigrate model fails closed: the resource has no
// persisted model to generate from.
func TestMarkedPackageWithNoModelFailsClosed(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module testapp\n\ngo 1.23\n")
	writeFile(t, filepath.Join(dir, "cmd", "server", "main.go"), "package main\n\nfunc main() {}\n")
	writeFile(t, filepath.Join(dir, "internal", "platform", "database.go"),
		"package platform\n\nimport \"github.com/gombit-dev/gombit/auth\"\n\n"+
			"func AutoMigrate(db anyDB) error {\n\treturn db.AutoMigrate(&auth.User{})\n}\n")
	writeFile(t, filepath.Join(dir, "internal", "book", resourcegen.ResourceMarkerFile), "")
	_, err := runGenerate(t, dir, false, false, &fakeRunner{out: artifactsJSON(t, bookArtifacts())})
	if err == nil || !strings.Contains(err.Error(), "no model") {
		t.Fatalf("a marked package with no AutoMigrate model must fail closed, got: %v", err)
	}
}

// A marked package registering more than one AutoMigrate model fails closed:
// generated files are package-level, so the resource target is ambiguous.
func TestMarkedPackageWithMultipleModelsFailsClosed(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module testapp\n\ngo 1.23\n")
	writeFile(t, filepath.Join(dir, "cmd", "server", "main.go"), "package main\n\nfunc main() {}\n")
	writeFile(t, filepath.Join(dir, "internal", "platform", "database.go"),
		"package platform\n\nimport \"testapp/internal/book\"\n\n"+
			"func AutoMigrate(db anyDB) error {\n\treturn db.AutoMigrate(&book.Book{}, &book.Tag{})\n}\n")
	writeFile(t, filepath.Join(dir, "internal", "book", resourcegen.ResourceMarkerFile), "")
	_, err := runGenerate(t, dir, false, false, &fakeRunner{out: artifactsJSON(t, bookArtifacts())})
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("a marked package with multiple models must fail closed, got: %v", err)
	}
}

func TestRejectsNonAppDirectory(t *testing.T) {
	dir := t.TempDir() // no go.mod / app layout
	if _, err := runGenerate(t, dir, false, false, &fakeRunner{}); err == nil {
		t.Fatal("must reject a non-gombit directory")
	}
}

// --- loaderSource -----------------------------------------------------------

// The generated loader must be valid Go that imports resourcegen and each model
// package and calls RenderResource per model. It is executed for real in
// TestGenerateProgramModeEndToEnd; here we assert it parses and wires correctly.
func TestLoaderSourceParsesAndWires(t *testing.T) {
	models := []migrations.Model{
		{ImportPath: "testapp/internal/book", TypeName: "Book"},
		{ImportPath: "testapp/internal/author", TypeName: "Author"},
	}
	src := loaderSource(models)
	if _, err := parser.ParseFile(token.NewFileSet(), "main.go", src, parser.AllErrors); err != nil {
		t.Fatalf("loader source does not parse: %v\n%s", err, src)
	}
	for _, want := range []string{
		`"github.com/gombit-dev/gombit/resourcegen"`,
		`model0 "testapp/internal/book"`,
		`model1 "testapp/internal/author"`,
		`resourcegen.RenderResource(&model0.Book{}, "book")`,
		`resourcegen.RenderResource(&model1.Author{}, "author")`,
		"json.NewEncoder(os.Stdout).Encode(all)",
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("loader source missing %q:\n%s", want, src)
		}
	}
}

// --- Program Mode end to end ------------------------------------------------

// Book is the compile-run model for the end-to-end test; package-level so its
// Go type name is literally "Book", matching the temp app's model.go.
type Book struct {
	gorm.Model
	Title    string `gorm:"not null"`
	TenantID uint   `gorm:"not null" gombit:"read,server"`
}

// TestGenerateProgramModeEndToEnd builds a real temp app whose book package is
// already in the model-first layout (model + committed .gen.go + hooks, generated
// by RenderResource itself), then runs the REAL Program-Mode loader (go run inside
// the app) to prove: (1) --check passes when the committed files match the model,
// (2) --check fails after the model changes, and (3) a write refreshes them.
func TestGenerateProgramModeEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles and runs a loader in a temp module; skipped in -short")
	}
	arts, err := resourcegen.RenderResource(&Book{}, "book")
	if err != nil {
		t.Fatalf("RenderResource: %v", err)
	}

	dir := t.TempDir()
	root := moduleRoot(t)
	writeFile(t, filepath.Join(dir, "go.mod"),
		"module bookapp\n\ngo 1.23\n\nrequire github.com/gombit-dev/gombit v0.0.0\n\nreplace github.com/gombit-dev/gombit => "+root+"\n")
	writeFile(t, filepath.Join(dir, "cmd", "server", "main.go"), "package main\n\nfunc main() {}\n")
	writeFile(t, filepath.Join(dir, "internal", "platform", "database.go"),
		"package platform\n\nimport \"bookapp/internal/book\"\n\n"+
			"func AutoMigrate(db anyDB) error {\n\treturn db.AutoMigrate(&book.Book{})\n}\n")
	writeFile(t, filepath.Join(dir, "internal", "book", "book.go"),
		"package book\n\nimport \"gorm.io/gorm\"\n\ntype Book struct {\n\tgorm.Model\n\tTitle    string `gorm:\"not null\"`\n\tTenantID uint   `gorm:\"not null\" gombit:\"read,server\"`\n}\n")
	// Mark book as a model-first resource so generate discovers it.
	writeFile(t, filepath.Join(dir, "internal", "book", resourcegen.ResourceMarkerFile), "")
	// Commit exactly what RenderResource produced, so --check starts clean.
	for _, a := range arts {
		writeFile(t, filepath.Join(dir, filepath.FromSlash(a.Path)), string(a.Content))
	}

	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}

	// (1) Fresh: --check passes against the real model.
	if _, err := runGenerate(t, dir, true, false, execRunner{}); err != nil {
		t.Fatalf("check should pass on a freshly generated app: %v", err)
	}

	// (2) Change the model: --check must now report drift.
	writeFile(t, filepath.Join(dir, "internal", "book", "book.go"),
		"package book\n\nimport \"gorm.io/gorm\"\n\ntype Book struct {\n\tgorm.Model\n\tTitle    string `gorm:\"not null\"`\n\tSubtitle string `gorm:\"not null\"`\n\tTenantID uint   `gorm:\"not null\" gombit:\"read,server\"`\n}\n")
	if _, err := runGenerate(t, dir, true, false, execRunner{}); err == nil {
		t.Fatal("check must fail after the model gains a field")
	}

	// (3) Regenerate (write): --check passes again.
	if _, err := runGenerate(t, dir, false, false, execRunner{}); err != nil {
		t.Fatalf("write regenerate: %v", err)
	}
	if _, err := runGenerate(t, dir, true, false, execRunner{}); err != nil {
		t.Fatalf("check should pass after regenerating: %v", err)
	}
	// The refreshed DTO reflects the new column.
	if got := read(t, filepath.Join(dir, "internal", "book", "dto.gen.go")); !strings.Contains(got, "Subtitle") {
		t.Fatalf("regenerated dto should include the new Subtitle column:\n%s", got)
	}
}

// moduleRoot is this repo's module root, for the temp app's replace directive.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root %s: %v", root, err)
	}
	return root
}

// TestStaleReportsStaleAndMissingFiles: Stale lists, by path, what Check
// fails on (a generator-owned file that differs, one that is missing),
// never a seed-once file, and how many resources there are; it writes
// nothing.
func TestStaleReportsStaleAndMissingFiles(t *testing.T) {
	dir := newAppLayout(t)
	arts := bookArtifacts()
	writeFile(t, filepath.Join(dir, "internal", "book", "dto.gen.go"), "package book\n\n// stale dto\n")
	writeFile(t, filepath.Join(dir, "internal", "book", "hooks.go"), "package book\n\n// my own hooks\n")
	stale, resources, err := Stale(context.Background(), Options{WorkDir: dir, Stdout: io.Discard, Stderr: io.Discard, runner: &fakeRunner{out: artifactsJSON(t, arts)}})
	if err != nil {
		t.Fatal(err)
	}
	if resources != 1 || len(stale) != 2 || stale[0] != (StaleFile{Path: "internal/book/dto.gen.go"}) || stale[1] != (StaleFile{Path: "internal/book/handler.gen.go", Missing: true}) {
		t.Fatalf("Stale = %+v, %d", stale, resources)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "internal", "book", "dto.gen.go")); string(got) != "package book\n\n// stale dto\n" { // #nosec G304 -- under t.TempDir()
		t.Fatal("Stale wrote a file")
	}
	// No resources: nothing rendered.
	empty := t.TempDir()
	writeFile(t, filepath.Join(empty, "go.mod"), "module testapp\n\ngo 1.23\n")
	writeFile(t, filepath.Join(empty, "cmd", "server", "main.go"), "package main\n\nfunc main() {}\n")
	writeFile(t, filepath.Join(empty, "internal", "platform", "database.go"), "package platform\n")
	runner := &fakeRunner{}
	if stale, resources, err := Stale(context.Background(), Options{WorkDir: empty, Stdout: io.Discard, Stderr: io.Discard, runner: runner}); err != nil || resources != 0 || len(stale) != 0 || runner.ran {
		t.Fatalf("no resources: %+v, %d, %v, ran %v", stale, resources, err, runner.ran)
	}
}

// TestUpdateWritesOnlyStaleFiles: Update renders once and writes the stale
// generator-owned file only: a current one is left as it is (its
// modification time too), and an existing hooks file is never written.
func TestUpdateWritesOnlyStaleFiles(t *testing.T) {
	dir := newAppLayout(t)
	arts := bookArtifacts()
	for _, a := range arts {
		writeFile(t, filepath.Join(dir, filepath.FromSlash(a.Path)), string(a.Content))
	}
	handler := filepath.Join(dir, "internal", "book", "handler.gen.go")
	dto := filepath.Join(dir, "internal", "book", "dto.gen.go")
	hooks := filepath.Join(dir, "internal", "book", "hooks.go")
	writeFile(t, dto, "package book\n\n// stale dto\n")
	writeFile(t, hooks, "package book\n\n// my hooks\n")
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(handler, old, old); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{out: artifactsJSON(t, arts)}
	written, resources, err := Update(context.Background(), Options{WorkDir: dir, Stdout: io.Discard, Stderr: io.Discard, runner: runner})
	if err != nil || resources != 1 || len(written) != 1 || written[0].Path != "internal/book/dto.gen.go" {
		t.Fatalf("Update = %+v, %d, %v", written, resources, err)
	}
	if info, err := os.Stat(handler); err != nil || !info.ModTime().Equal(old) {
		t.Fatalf("the current handler.gen.go was rewritten (%v)", err)
	}
	if got, _ := os.ReadFile(hooks); string(got) != "package book\n\n// my hooks\n" { // #nosec G304 -- under t.TempDir()
		t.Fatal("hooks.go was written")
	}
	for _, a := range arts {
		if a.Path == "internal/book/dto.gen.go" {
			if got, _ := os.ReadFile(dto); string(got) != string(a.Content) { // #nosec G304 -- under t.TempDir()
				t.Fatal("dto.gen.go was not regenerated")
			}
		}
	}
}

// TestUpdateReportsWhatAFailedWriteLeft: a write failure returns the stale
// files written before it, with the write error, which is not a
// LoaderError; a loader failure is one.
func TestUpdateReportsWhatAFailedWriteLeft(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs unix permissions, as a user they bind")
	}
	dir := newAppLayout(t)
	arts := append(bookArtifacts()[:1], resourcegen.GeneratedArtifact{Path: "internal/locked/x.gen.go", Content: []byte("package locked\n"), Ownership: resourcegen.GeneratorOwned})
	locked := filepath.Join(dir, "internal", "locked")
	if err := os.MkdirAll(locked, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o500); err != nil { // #nosec G302 -- a test directory made read-only
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) }) // #nosec G302 -- restored for cleanup
	written, _, err := Update(context.Background(), Options{WorkDir: dir, Stdout: io.Discard, Stderr: io.Discard, runner: &fakeRunner{out: artifactsJSON(t, arts)}})
	var loaderErr *LoaderError
	if err == nil || errors.As(err, &loaderErr) || len(written) != 1 || written[0].Path != "internal/book/dto.gen.go" {
		t.Fatalf("Update = %+v, %v; want dto.gen.go written, then the write error", written, err)
	}
	_, _, err = Stale(context.Background(), Options{WorkDir: dir, Stdout: io.Discard, Stderr: io.Discard, runner: &fakeRunner{err: errors.New("models do not compile")}})
	if !errors.As(err, &loaderErr) || !strings.Contains(err.Error(), "models do not compile") {
		t.Fatalf("a loader failure = %v; want a LoaderError", err)
	}
}

// TestUpdateNeverSeeds: a seed-once file that is absent (a resource that
// declares its hooks in another file) is not written, so --write never
// redeclares them; with nothing stale, nothing is written at all.
func TestUpdateNeverSeeds(t *testing.T) {
	dir := newAppLayout(t)
	arts := bookArtifacts()
	for _, a := range arts[:2] {
		writeFile(t, filepath.Join(dir, filepath.FromSlash(a.Path)), string(a.Content))
	}
	written, _, err := Update(context.Background(), Options{WorkDir: dir, Stdout: io.Discard, Stderr: io.Discard, runner: &fakeRunner{out: artifactsJSON(t, arts)}})
	if err != nil || len(written) != 0 {
		t.Fatalf("Update = %+v, %v; want nothing written", written, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "internal", "book", "hooks.go")); !os.IsNotExist(err) {
		t.Fatalf("hooks.go was seeded (%v)", err)
	}
}
