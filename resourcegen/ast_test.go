package resourcegen

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/migrations"
)

const fixtureMain = `package main

import (
	"context"
	"log"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/framework"

	"github.com/example/demo/internal/platform"
	"github.com/example/demo/internal/product"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	app, err := framework.New(framework.WithConfig(cfg))
	if err != nil {
		log.Fatal(err)
	}
	product.Register(app)
	if err := framework.Run(app); err != nil {
		log.Fatal(err)
	}
}
`

const fixturePlatform = `package platform

import (
	"github.com/gombit-dev/gombit/database"

	"github.com/example/demo/internal/product"
)

func AutoMigrate(db *database.DB) error {
	return db.AutoMigrate(&product.Product{})
}
`

const fixtureMainIfRunOnly = `package main

import (
	"log"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/framework"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	app, err := framework.New(framework.WithConfig(cfg))
	if err != nil {
		log.Fatal(err)
	}
	if err := framework.Run(app); err != nil {
		log.Fatal(err)
	}
}
`

const fixtureMainNestedIfRun = `package main

import (
	"log"

	"github.com/gombit-dev/gombit/framework"
)

func main() {
	app, err := framework.New()
	if err != nil {
		log.Fatal(err)
	}
	if app != nil {
		if err := framework.Run(app); err != nil {
			log.Fatal(err)
		}
	}
}
`

func TestAddImportAndRegisterIdempotent(t *testing.T) {
	t.Parallel()

	once, err := AddImportAndRegister([]byte(fixtureMain), "github.com/example/demo/internal/book", "book")
	if err != nil {
		t.Fatalf("AddImportAndRegister() error = %v", err)
	}
	count, err := CountRegisterCalls(once, "book")
	if err != nil {
		t.Fatalf("CountRegisterCalls: %v", err)
	}
	if count != 1 {
		t.Fatalf("book.Register count = %d, want 1\n%s", count, once)
	}
	if !strings.Contains(string(once), "github.com/example/demo/internal/book") {
		t.Fatalf("missing import:\n%s", once)
	}
	if countMust(t, once, "product") != 1 {
		t.Fatalf("product.Register was disturbed:\n%s", once)
	}

	twice, err := AddImportAndRegister(once, "github.com/example/demo/internal/book", "book")
	if err != nil {
		t.Fatalf("second AddImportAndRegister() error = %v", err)
	}
	count, err = CountRegisterCalls(twice, "book")
	if err != nil {
		t.Fatalf("CountRegisterCalls second: %v", err)
	}
	if count != 1 {
		t.Fatalf("second run duplicated Register: count = %d\n%s", count, twice)
	}
}

func TestAddImportAndRegisterBeforeIfInitRun(t *testing.T) {
	t.Parallel()

	once, err := AddImportAndRegister([]byte(fixtureMainIfRunOnly), "github.com/example/demo/internal/book", "book")
	if err != nil {
		t.Fatalf("AddImportAndRegister() error = %v", err)
	}
	if countMust(t, once, "book") != 1 {
		t.Fatalf("book.Register count = %d, want 1\n%s", countMust(t, once, "book"), once)
	}
	src := string(once)
	if !strings.Contains(src, "github.com/example/demo/internal/book") {
		t.Fatalf("missing import:\n%s", src)
	}
	registerIdx := strings.Index(src, "book.Register(app)")
	runIdx := strings.Index(src, "framework.Run(app)")
	if registerIdx < 0 || runIdx < 0 || registerIdx > runIdx {
		t.Fatalf("Register should appear before framework.Run:\n%s", src)
	}

	twice, err := AddImportAndRegister(once, "github.com/example/demo/internal/book", "book")
	if err != nil {
		t.Fatalf("second AddImportAndRegister() error = %v", err)
	}
	if countMust(t, twice, "book") != 1 {
		t.Fatalf("second run duplicated Register: count = %d\n%s", countMust(t, twice, "book"), twice)
	}
}

func TestAddImportAndRegisterNestedIfInitRun(t *testing.T) {
	t.Parallel()

	got, err := AddImportAndRegister([]byte(fixtureMainNestedIfRun), "github.com/example/demo/internal/book", "book")
	if err != nil {
		t.Fatalf("AddImportAndRegister() error = %v", err)
	}
	if countMust(t, got, "book") != 1 {
		t.Fatalf("book.Register count = %d, want 1\n%s", countMust(t, got, "book"), got)
	}
	src := string(got)
	registerIdx := strings.Index(src, "book.Register(app)")
	runIdx := strings.Index(src, "framework.Run(app)")
	if registerIdx < 0 || runIdx < 0 || registerIdx > runIdx {
		t.Fatalf("Register should appear before nested framework.Run:\n%s", src)
	}
}

func TestCollectAutoMigrateModelsIncludesExistingAndNew(t *testing.T) {
	t.Parallel()

	onlyProduct, err := CollectAutoMigrateModels([]byte(fixturePlatform))
	if err != nil {
		t.Fatalf("CollectAutoMigrateModels() error = %v", err)
	}
	if !hasCollectedModel(onlyProduct, "github.com/example/demo/internal/product", "Product") {
		t.Fatalf("models = %#v, want product.Product", onlyProduct)
	}
	if hasCollectedModel(onlyProduct, "github.com/example/demo/internal/book", "Book") {
		t.Fatalf("models = %#v, did not want book.Book before AddAutoMigrateModel", onlyProduct)
	}

	updated, err := AddAutoMigrateModel([]byte(fixturePlatform), "github.com/example/demo/internal/book", "book", "Book")
	if err != nil {
		t.Fatalf("AddAutoMigrateModel() error = %v", err)
	}
	got, err := CollectAutoMigrateModels(updated)
	if err != nil {
		t.Fatalf("CollectAutoMigrateModels(updated) error = %v", err)
	}
	if !hasCollectedModel(got, "github.com/example/demo/internal/product", "Product") {
		t.Fatalf("models = %#v, want product.Product", got)
	}
	if !hasCollectedModel(got, "github.com/example/demo/internal/book", "Book") {
		t.Fatalf("models = %#v, want book.Book", got)
	}
	if len(got) != 2 {
		t.Fatalf("models = %#v, want exactly product + book", got)
	}
}

func TestCollectAutoMigrateModelsNamedImport(t *testing.T) {
	t.Parallel()

	src := []byte(`package platform

import (
	"github.com/gombit-dev/gombit/database"

	productmodel "github.com/example/demo/internal/product"
	"github.com/example/demo/internal/book"
)

func AutoMigrate(db *database.DB) error {
	return db.AutoMigrate(&productmodel.Product{}, &book.Book{})
}
`)
	got, err := CollectAutoMigrateModels(src)
	if err != nil {
		t.Fatalf("CollectAutoMigrateModels() error = %v", err)
	}
	if !hasCollectedModel(got, "github.com/example/demo/internal/product", "Product") {
		t.Fatalf("models = %#v, want aliased product.Product", got)
	}
	if !hasCollectedModel(got, "github.com/example/demo/internal/book", "Book") {
		t.Fatalf("models = %#v, want book.Book", got)
	}
}

func hasCollectedModel(models []migrations.Model, importPath, typeName string) bool {
	for _, model := range models {
		if model.ImportPath == importPath && model.TypeName == typeName {
			return true
		}
	}
	return false
}

func TestAddAutoMigrateModelIdempotent(t *testing.T) {
	t.Parallel()

	once, err := AddAutoMigrateModel([]byte(fixturePlatform), "github.com/example/demo/internal/book", "book", "Book")
	if err != nil {
		t.Fatalf("AddAutoMigrateModel() error = %v", err)
	}
	if !strings.Contains(string(once), "&book.Book{}") {
		t.Fatalf("missing book model:\n%s", once)
	}
	if !strings.Contains(string(once), "&product.Product{}") {
		t.Fatalf("lost product model:\n%s", once)
	}

	twice, err := AddAutoMigrateModel(once, "github.com/example/demo/internal/book", "book", "Book")
	if err != nil {
		t.Fatalf("second AddAutoMigrateModel() error = %v", err)
	}
	if strings.Count(string(twice), "&book.Book{}") != 1 {
		t.Fatalf("duplicated AutoMigrate arg:\n%s", twice)
	}
}

func TestGoEditPathUsesParserASTNotRegexp(t *testing.T) {
	t.Parallel()

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	astPath := filepath.Join(filepath.Dir(thisFile), "ast.go")
	// #nosec G304 -- package source next to this test
	src, err := os.ReadFile(astPath)
	if err != nil {
		t.Fatalf("read ast.go: %v", err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, astPath, src, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse ast.go: %v", err)
	}
	imports := map[string]bool{}
	for _, imp := range file.Imports {
		if imp.Path != nil {
			imports[strings.Trim(imp.Path.Value, `"`)] = true
		}
	}
	for _, want := range []string{"go/ast", "go/parser", "go/format"} {
		if !imports[want] {
			t.Fatalf("ast.go missing import %q", want)
		}
	}
	if imports["regexp"] {
		t.Fatal("ast.go must not import regexp")
	}
}

func countMust(t *testing.T, src []byte, pkg string) int {
	t.Helper()
	count, err := CountRegisterCalls(src, pkg)
	if err != nil {
		t.Fatalf("CountRegisterCalls(%s): %v", pkg, err)
	}
	return count
}

// TestAutoMigrateModelsWithoutAutoMigrateCall: a platform database.go with
// no AutoMigrate call (the schema comes from migrations alone) is reported
// as not ok, like a missing file, so the AutoMigrate-based checks skip it.
// make resource still needs the call to register its model, so
// CollectAutoMigrateModels keeps refusing such a file.
func TestAutoMigrateModelsWithoutAutoMigrateCall(t *testing.T) {
	src := "package platform\n\nimport \"github.com/gombit-dev/gombit/database\"\n\nfunc Open() (*database.DB, error) { return nil, nil }\n"
	workDir := t.TempDir()
	path := filepath.Join(workDir, filepath.FromSlash(platformDBRel))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	models, ok, err := AutoMigrateModels(workDir)
	if err != nil || ok || models != nil {
		t.Fatalf("AutoMigrateModels() = %v, %v, %v; want nil, false, nil", models, ok, err)
	}
	if _, err := CollectAutoMigrateModels([]byte(src)); err == nil || !strings.Contains(err.Error(), "no AutoMigrate call in internal/platform/database.go") {
		t.Fatalf("CollectAutoMigrateModels() error = %v, want no AutoMigrate call", err)
	}
}

// TestAutoMigrateModelsReportsAnUnreadableFile: only a file with no
// AutoMigrate call is skipped. One that does not parse, or names a model
// whose package it does not import, is an error, so db check reports it and
// makemigrations --forget-model refuses rather than dropping a table the app
// may still create.
func TestAutoMigrateModelsReportsAnUnreadableFile(t *testing.T) {
	for name, src := range map[string]string{
		"syntax error": "package platform\n\nfunc AutoMigrate(db *database.DB) error {\n<<<<<<< HEAD\n",
		"unresolved model": "package platform\n\nimport \"github.com/gombit-dev/gombit/database\"\n\n" +
			"func AutoMigrate(db *database.DB) error { return db.AutoMigrate(&missing.Widget{}) }\n",
	} {
		t.Run(name, func(t *testing.T) {
			workDir := t.TempDir()
			path := filepath.Join(workDir, filepath.FromSlash(platformDBRel))
			if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, ok, err := AutoMigrateModels(workDir); err == nil || ok {
				t.Fatalf("AutoMigrateModels() = ok %v, err %v; want an error", ok, err)
			}
		})
	}
}
