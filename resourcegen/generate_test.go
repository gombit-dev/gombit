package resourcegen

import (
	"bytes"
	"context"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/migrations"
	"github.com/gombit-dev/gombit/scaffold"
)

func TestGenerateBookFeaturePackage(t *testing.T) {
	workDir := t.TempDir()
	if err := scaffold.Generate(context.Background(), scaffold.Options{
		Name:     "demo",
		Database: "sqlite",
		WorkDir:  workDir,
		Stdout:   ioDiscard{},
	}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	appDir := filepath.Join(workDir, "demo")

	previousLook := lookPath
	lookPath = func(string) (string, error) { return "", errors.New("atlas missing") }
	t.Cleanup(func() { lookPath = previousLook })

	stdout := new(bytes.Buffer)
	err := Generate(context.Background(), Options{
		WorkDir:   appDir,
		Name:      "Book",
		Fields:    []string{"title:string:required"},
		Stdout:    stdout,
		skipAtlas: true,
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if !strings.Contains(stdout.String(), "create internal/book/book.go") {
		t.Fatalf("stdout = %q, want created model", stdout.String())
	}
	if !strings.Contains(stdout.String(), "gombit db makemigrations") {
		t.Fatalf("stdout = %q, want makemigrations hint", stdout.String())
	}

	modelPath := filepath.Join(appDir, "internal", "book", "book.go")
	modelSrc := readFile(t, modelPath)
	if !strings.Contains(modelSrc, GeneratedBanner) {
		t.Fatal("model missing generated banner")
	}
	if !strings.Contains(modelSrc, "type Book struct") {
		t.Fatal("model missing Book type")
	}
	fset := token.NewFileSet()
	if _, err := parser.ParseFile(fset, modelPath, modelSrc, 0); err != nil {
		t.Fatalf("generated model is not valid Go: %v", err)
	}

	mod := readModulePathMust(t, appDir)
	spec := mod + "/internal/book.Book"
	parsed, err := migrations.ParseModel(spec)
	if err != nil {
		t.Fatalf("ParseModel(%q) error = %v", spec, err)
	}
	if parsed.TypeName != "Book" {
		t.Fatalf("ParseModel type = %q, want Book", parsed.TypeName)
	}

	mainSrc := readFile(t, filepath.Join(appDir, "cmd", "server", "main.go"))
	count, err := CountRegisterCalls([]byte(mainSrc), "book")
	if err != nil {
		t.Fatalf("CountRegisterCalls: %v", err)
	}
	if count != 1 {
		t.Fatalf("book.Register count = %d, want 1\n%s", count, mainSrc)
	}

	handlerPath := filepath.Join(appDir, "internal", "book", "handler.go")
	handlerSrc := readFile(t, handlerPath)
	if !strings.Contains(handlerSrc, `contract.Internal("list books")`) {
		t.Fatalf("handler Internal message = %q, want list books", handlerSrc)
	}
	if !strings.Contains(handlerSrc, `database.MapLoadError(ctx, err, "book not found", "load book")`) {
		t.Fatal("generated get handler does not map load errors via database.MapLoadError")
	}
	if !strings.Contains(handlerSrc, `database.MapPersistError(ctx, err, "resource already exists", "create book")`) {
		t.Fatal("generated create handler does not map persist errors via database.MapPersistError")
	}
	if strings.Count(handlerSrc, `contract.NotFound("book not found")`) != 1 {
		t.Fatal("generated get handler should keep parse-id as not_found and not map First() errors to 404")
	}
	if !strings.Contains(handlerSrc, `query:"page"`) || !strings.Contains(handlerSrc, `query:"per_page"`) {
		t.Fatal("generated list handler missing page/per_page query params")
	}
	if !strings.Contains(handlerSrc, "contract.ClampPage") || !strings.Contains(handlerSrc, "contract.PageOffset") {
		t.Fatal("generated list handler does not clamp page/per_page")
	}
	if !strings.Contains(handlerSrc, ".Limit(") || !strings.Contains(handlerSrc, "Count(&total)") {
		t.Fatal("generated list handler does not LIMIT/OFFSET or count total separately")
	}
	if strings.Contains(handlerSrc, "PerPage: 20, Total: int64(len(items))") {
		t.Fatal("generated list handler still advertises hardcoded per_page=20 from len(items)")
	}
	if _, err := os.Stat(filepath.Join(appDir, "internal", "book", "service.go")); !os.IsNotExist(err) {
		t.Fatal("default generate wrote service.go")
	}
	if _, err := os.Stat(filepath.Join(appDir, "internal", "book", "repo.go")); !os.IsNotExist(err) {
		t.Fatal("default generate wrote repo.go")
	}

	listTS := readFile(t, filepath.Join(appDir, "frontend", "src", "book", "list.tsx"))
	if !strings.Contains(listTS, `from "../api/generated/schema"`) {
		t.Fatal("list.tsx does not import generated OpenAPI types")
	}
	if !strings.Contains(listTS, `from "../api/generated/client"`) {
		t.Fatal("list.tsx does not import the generated client")
	}
	if strings.Contains(strings.ToLower(listTS), "localstorage") {
		t.Fatal("list.tsx uses localStorage")
	}
	if strings.Contains(listTS, "@mui/material") {
		t.Fatal("default make resource must stay headless")
	}
	if strings.Contains(listTS, `to="/">Products</Link>`) {
		t.Fatal("generated Book list.tsx hardcodes a Products home link; AppLayout already exposes that nav")
	}
	if strings.Contains(listTS, "Products") {
		t.Fatalf("generated Book list.tsx still mentions Products:\n%s", listTS)
	}
	if !strings.Contains(listTS, `to="/books/new">New Book</Link>`) {
		t.Fatal("generated Book list.tsx missing New Book link")
	}
	formTS := readFile(t, filepath.Join(appDir, "frontend", "src", "book", "form.tsx"))
	if !strings.Contains(formTS, "applyContractErrors") || !strings.Contains(formTS, "setError") {
		t.Fatal("form.tsx does not map D10 field errors through applyContractErrors")
	}
	if strings.Contains(formTS, "required: true") {
		t.Fatal("form.tsx required rule has no message; submitting an empty field will show no error text")
	}
	if !strings.Contains(formTS, `required: "Title is required"`) {
		t.Fatalf("form.tsx missing a required-field error message:\n%s", formTS)
	}
	resources := readFile(t, filepath.Join(appDir, "frontend", "src", "resources.tsx"))
	if !strings.Contains(resources, "generatedResourceRoutes") || !strings.Contains(resources, "BookListPage") {
		t.Fatal("resources.tsx missing React Router registry for Book")
	}

	// Idempotent re-run does not duplicate Register.
	err = Generate(context.Background(), Options{
		WorkDir:   appDir,
		Name:      "Book",
		Fields:    []string{"title:string:required"},
		Stdout:    ioDiscard{},
		skipAtlas: true,
	})
	if err != nil {
		t.Fatalf("re-run Generate() error = %v", err)
	}
	mainSrc = readFile(t, filepath.Join(appDir, "cmd", "server", "main.go"))
	count, err = CountRegisterCalls([]byte(mainSrc), "book")
	if err != nil {
		t.Fatalf("CountRegisterCalls after re-run: %v", err)
	}
	if count != 1 {
		t.Fatalf("re-run duplicated book.Register: count = %d", count)
	}

	// User edit is refused without --force.
	if err := os.WriteFile(handlerPath, []byte("package book\n\n// user edit\n"), 0o600); err != nil {
		t.Fatalf("edit handler: %v", err)
	}
	err = Generate(context.Background(), Options{
		WorkDir:   appDir,
		Name:      "Book",
		Fields:    []string{"title:string:required"},
		Stdout:    ioDiscard{},
		skipAtlas: true,
	})
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("clobber error = %v, want --force", err)
	}
	got := readFile(t, handlerPath)
	if !strings.Contains(got, "user edit") {
		t.Fatal("user handler.go was overwritten without --force")
	}

	err = Generate(context.Background(), Options{
		WorkDir:   appDir,
		Name:      "Book",
		Fields:    []string{"title:string:required"},
		Force:     true,
		Stdout:    ioDiscard{},
		skipAtlas: true,
	})
	if err != nil {
		t.Fatalf("Generate(--force) error = %v", err)
	}
	got = readFile(t, handlerPath)
	if strings.Contains(got, "user edit") {
		t.Fatal("--force did not replace handler.go")
	}
}

func TestGenerateDryRunAndServiceRepo(t *testing.T) {
	workDir := t.TempDir()
	if err := scaffold.Generate(context.Background(), scaffold.Options{
		Name:     "demo",
		Database: "sqlite",
		WorkDir:  workDir,
		Stdout:   ioDiscard{},
	}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	appDir := filepath.Join(workDir, "demo")

	stdout := new(bytes.Buffer)
	err := Generate(context.Background(), Options{
		WorkDir:   appDir,
		Name:      "Invoice",
		Service:   true,
		Repo:      true,
		DryRun:    true,
		Stdout:    stdout,
		skipAtlas: true,
	})
	if err != nil {
		t.Fatalf("Generate(--dry-run) error = %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "internal/invoice/service.go") || !strings.Contains(out, "internal/invoice/repo.go") {
		t.Fatalf("dry-run stdout = %q, want service and repo", out)
	}
	if _, err := os.Stat(filepath.Join(appDir, "internal", "invoice")); !os.IsNotExist(err) {
		t.Fatal("dry-run wrote invoice package")
	}

	err = Generate(context.Background(), Options{
		WorkDir:   appDir,
		Name:      "Invoice",
		Service:   true,
		Repo:      true,
		Stdout:    ioDiscard{},
		skipAtlas: true,
	})
	if err != nil {
		t.Fatalf("Generate(--service --repo) error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(appDir, "internal", "invoice", "service.go")); err != nil {
		t.Fatalf("missing service.go: %v", err)
	}
	if _, err := os.Stat(filepath.Join(appDir, "internal", "invoice", "repo.go")); err != nil {
		t.Fatalf("missing repo.go: %v", err)
	}
}

func TestGenerateMissingAtlasPrintsHint(t *testing.T) {
	workDir := t.TempDir()
	if err := scaffold.Generate(context.Background(), scaffold.Options{
		Name:     "demo",
		Database: "sqlite",
		WorkDir:  workDir,
		Stdout:   ioDiscard{},
	}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	previousLook := lookPath
	lookPath = func(string) (string, error) { return "", errors.New("atlas missing") }
	t.Cleanup(func() { lookPath = previousLook })

	stdout := new(bytes.Buffer)
	err := Generate(context.Background(), Options{
		WorkDir: filepath.Join(workDir, "demo"),
		Name:    "Book",
		Fields:  []string{"title:string:required"},
		Stdout:  stdout,
	})
	if err != nil {
		t.Fatalf("Generate() error = %v, want missing-atlas hint", err)
	}
	if !strings.Contains(stdout.String(), "atlas not on PATH") {
		t.Fatalf("stdout = %q, want atlas not on PATH hint", stdout.String())
	}
	if !strings.Contains(stdout.String(), "--model ") || !strings.Contains(stdout.String(), "/internal/product.Product") {
		t.Fatalf("stdout = %q, want existing product model in makemigrations hint", stdout.String())
	}
	if !strings.Contains(stdout.String(), "/internal/book.Book") {
		t.Fatalf("stdout = %q, want new book model in makemigrations hint", stdout.String())
	}
}

func TestGenerateSurfacesMakeMigrationsError(t *testing.T) {
	workDir := t.TempDir()
	if err := scaffold.Generate(context.Background(), scaffold.Options{
		Name:     "demo",
		Database: "sqlite",
		WorkDir:  workDir,
		Stdout:   ioDiscard{},
	}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	previousLook := lookPath
	lookPath = func(string) (string, error) { return "/usr/bin/atlas", nil }
	t.Cleanup(func() { lookPath = previousLook })

	previousMake := makeMigrations
	makeMigrations = func(context.Context, migrations.Options) error {
		return errors.New("atlas migrate diff failed")
	}
	t.Cleanup(func() { makeMigrations = previousMake })

	stdout := new(bytes.Buffer)
	err := Generate(context.Background(), Options{
		WorkDir: filepath.Join(workDir, "demo"),
		Name:    "Book",
		Fields:  []string{"title:string:required"},
		Stdout:  stdout,
	})
	if err == nil {
		t.Fatal("Generate() error = nil, want makemigrations failure")
	}
	if !strings.Contains(err.Error(), "makemigrations") || !strings.Contains(err.Error(), "atlas migrate diff failed") {
		t.Fatalf("error = %v, want wrapped atlas failure", err)
	}
	if strings.Contains(stdout.String(), "note:") {
		t.Fatalf("stdout = %q, did not want swallowed atlas note", stdout.String())
	}
}

func TestGeneratePassesAllAutoMigrateModels(t *testing.T) {
	workDir := t.TempDir()
	if err := scaffold.Generate(context.Background(), scaffold.Options{
		Name:     "demo",
		Database: "sqlite",
		WorkDir:  workDir,
		Stdout:   ioDiscard{},
	}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	previousLook := lookPath
	lookPath = func(string) (string, error) { return "/usr/bin/atlas", nil }
	t.Cleanup(func() { lookPath = previousLook })

	var got []migrations.Model
	previousMake := makeMigrations
	makeMigrations = func(_ context.Context, opts migrations.Options) error {
		got = append([]migrations.Model(nil), opts.Models...)
		return nil
	}
	t.Cleanup(func() { makeMigrations = previousMake })

	err := Generate(context.Background(), Options{
		WorkDir: filepath.Join(workDir, "demo"),
		Name:    "Book",
		Fields:  []string{"title:string:required"},
		Stdout:  ioDiscard{},
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	mod := readModulePathMust(t, filepath.Join(workDir, "demo"))
	if !hasCollectedModel(got, "github.com/gombit-dev/gombit/auth", "User") {
		t.Fatalf("MakeMigrations models = %#v, want runtime auth.User", got)
	}
	if !hasCollectedModel(got, "github.com/gombit-dev/gombit/auth", "RefreshToken") {
		t.Fatalf("MakeMigrations models = %#v, want runtime auth.RefreshToken", got)
	}
	if !hasCollectedModel(got, "github.com/gombit-dev/gombit/auth", "Group") {
		t.Fatalf("MakeMigrations models = %#v, want runtime auth.Group", got)
	}
	if !hasCollectedModel(got, "github.com/gombit-dev/gombit/auth", "Permission") {
		t.Fatalf("MakeMigrations models = %#v, want runtime auth.Permission", got)
	}
	if !hasCollectedModel(got, mod+"/internal/product", "Product") {
		t.Fatalf("MakeMigrations models = %#v, want scaffold product", got)
	}
	if !hasCollectedModel(got, mod+"/internal/book", "Book") {
		t.Fatalf("MakeMigrations models = %#v, want generated book", got)
	}
	if len(got) != 6 {
		t.Fatalf("MakeMigrations models = %#v, want all auth models plus product + book", got)
	}
}

func TestGenerateMUIResourcePages(t *testing.T) {
	workDir := t.TempDir()
	if err := scaffold.Generate(context.Background(), scaffold.Options{
		Name:     "demo",
		Database: "sqlite",
		UI:       "mui",
		WorkDir:  workDir,
		Stdout:   ioDiscard{},
	}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	appDir := filepath.Join(workDir, "demo")

	err := Generate(context.Background(), Options{
		WorkDir:   appDir,
		Name:      "Book",
		Fields:    []string{"title:string:required"},
		Stdout:    ioDiscard{},
		skipAtlas: true,
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	listTS := readFile(t, filepath.Join(appDir, "frontend", "src", "book", "list.tsx"))
	for _, want := range []string{`from "@mui/material"`, "TableContainer", "TableHead", `from "../api/generated/schema"`, `from "../api/generated/client"`} {
		if !strings.Contains(listTS, want) {
			t.Fatalf("MUI list.tsx missing %q", want)
		}
	}
	if strings.Contains(strings.ToLower(listTS), "localstorage") {
		t.Fatal("MUI list.tsx uses localStorage")
	}

	formTS := readFile(t, filepath.Join(appDir, "frontend", "src", "book", "form.tsx"))
	for _, want := range []string{`from "@mui/material"`, "TextField", "applyContractErrors", "setError"} {
		if !strings.Contains(formTS, want) {
			t.Fatalf("MUI form.tsx missing %q", want)
		}
	}
	if strings.Contains(formTS, "required: true") {
		t.Fatal("MUI form.tsx required rule has no message; submitting an empty field will show no error text")
	}
	if !strings.Contains(formTS, `required: "Title is required"`) {
		t.Fatalf("MUI form.tsx missing a required-field error message:\n%s", formTS)
	}
}

func TestGenerateUnknownType(t *testing.T) {
	workDir := t.TempDir()
	if err := scaffold.Generate(context.Background(), scaffold.Options{
		Name:     "demo",
		Database: "sqlite",
		WorkDir:  workDir,
		Stdout:   ioDiscard{},
	}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	err := Generate(context.Background(), Options{
		WorkDir: filepath.Join(workDir, "demo"),
		Name:    "Widget",
		Fields:  []string{"amount:blob"},
		Stdout:  ioDiscard{},
	})
	if err == nil || !strings.Contains(err.Error(), "unknown type") {
		t.Fatalf("error = %v, want unknown type", err)
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) {
	return len(p), nil
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	// #nosec G304 -- test fixture path
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func readModulePathMust(t *testing.T, dir string) string {
	t.Helper()
	mod, err := readModulePath(dir)
	if err != nil {
		t.Fatalf("readModulePath: %v", err)
	}
	return mod
}

func TestReadModulePathStripsLineComment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		gomod string
		want  string
	}{
		{
			name:  "trailing comment",
			gomod: "module github.com/example/demo // app\n\ngo 1.25\n",
			want:  "github.com/example/demo",
		},
		{
			name:  "quoted path with comment",
			gomod: "module \"github.com/example/demo\" // app\n",
			want:  "github.com/example/demo",
		},
		{
			name:  "no comment",
			gomod: "module github.com/example/demo\n",
			want:  "github.com/example/demo",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(tt.gomod), 0o600); err != nil {
				t.Fatalf("write go.mod: %v", err)
			}
			got, err := readModulePath(dir)
			if err != nil {
				t.Fatalf("readModulePath() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("readModulePath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGenerateStripsGoModLineCommentFromImports(t *testing.T) {
	workDir := t.TempDir()
	if err := scaffold.Generate(context.Background(), scaffold.Options{
		Name:     "demo",
		Database: "sqlite",
		WorkDir:  workDir,
		Stdout:   ioDiscard{},
	}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	appDir := filepath.Join(workDir, "demo")
	appendGoModModuleComment(t, appDir, "app")

	previousLook := lookPath
	lookPath = func(string) (string, error) { return "", errors.New("atlas missing") }
	t.Cleanup(func() { lookPath = previousLook })

	err := Generate(context.Background(), Options{
		WorkDir:   appDir,
		Name:      "Book",
		Fields:    []string{"title:string:required"},
		Stdout:    ioDiscard{},
		skipAtlas: true,
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	mod := readModulePathMust(t, appDir)
	if strings.Contains(mod, "//") {
		t.Fatalf("readModulePath() = %q, want comment stripped", mod)
	}
	wantImport := `"` + mod + `/internal/book"`
	mainSrc := readFile(t, filepath.Join(appDir, "cmd", "server", "main.go"))
	if !strings.Contains(mainSrc, wantImport) {
		t.Fatalf("cmd/server/main.go missing import %s:\n%s", wantImport, mainSrc)
	}
	if strings.Contains(mainSrc, "// app/") {
		t.Fatalf("cmd/server/main.go kept go.mod comment in an import:\n%s", mainSrc)
	}
	platformSrc := readFile(t, filepath.Join(appDir, "internal", "platform", "database.go"))
	if strings.Contains(platformSrc, "// app/") {
		t.Fatalf("internal/platform/database.go kept go.mod comment in an import:\n%s", platformSrc)
	}
}

func appendGoModModuleComment(t *testing.T, appDir, comment string) {
	t.Helper()
	path := filepath.Join(appDir, "go.mod")
	data := readFile(t, path)
	lines := strings.Split(data, "\n")
	found := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "module ") {
			lines[i] = strings.TrimSpace(line) + " // " + comment
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
}

// TestGenerateFrontendKeepsTypedDefaultPrefix is the #109 contract for
// make-resource pages: gombit.yaml api_prefix is not baked into list/form
// OpenAPI path keys. createAppClient rewrites /api/v1 to the live prefix.
func TestGenerateFrontendKeepsTypedDefaultPrefix(t *testing.T) {
	workDir := t.TempDir()
	if err := scaffold.Generate(context.Background(), scaffold.Options{
		Name:     "demo",
		Database: "sqlite",
		WorkDir:  workDir,
		Stdout:   ioDiscard{},
	}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	appDir := filepath.Join(workDir, "demo")
	yamlPath := filepath.Join(appDir, "gombit.yaml")
	yaml := readFile(t, yamlPath)
	yaml = strings.ReplaceAll(yaml, "api_prefix: /api/v1", "api_prefix: /svc/v2")
	if !strings.Contains(yaml, "api_prefix: /svc/v2") {
		t.Fatal("failed to rewrite gombit.yaml api_prefix")
	}
	if err := os.WriteFile(yamlPath, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write gombit.yaml: %v", err)
	}

	previousLook := lookPath
	lookPath = func(string) (string, error) { return "", errors.New("atlas missing") }
	t.Cleanup(func() { lookPath = previousLook })

	err := Generate(context.Background(), Options{
		WorkDir:   appDir,
		Name:      "Book",
		Fields:    []string{"title:string:required"},
		Stdout:    ioDiscard{},
		skipAtlas: true,
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	listTS := readFile(t, filepath.Join(appDir, "frontend", "src", "book", "list.tsx"))
	if !strings.Contains(listTS, `const listPath = "/api/v1/books" as const`) {
		t.Fatalf("list.tsx must keep typed /api/v1 OpenAPI path, got:\n%s", listTS)
	}
	if strings.Contains(listTS, "/svc/v2") {
		t.Fatal("list.tsx baked live api_prefix /svc/v2; prefix must be runtime-rewritten")
	}
	formTS := readFile(t, filepath.Join(appDir, "frontend", "src", "book", "form.tsx"))
	if !strings.Contains(formTS, `const createPath = "/api/v1/books" as const`) {
		t.Fatalf("form.tsx must keep typed /api/v1 OpenAPI path, got:\n%s", formTS)
	}
	if strings.Contains(formTS, "/svc/v2") {
		t.Fatal("form.tsx baked live api_prefix /svc/v2")
	}
}

func TestGenerateRefusesCollidingHTTPPath(t *testing.T) {
	workDir := t.TempDir()
	if err := scaffold.Generate(context.Background(), scaffold.Options{
		Name:     "demo",
		Database: "sqlite",
		WorkDir:  workDir,
		Stdout:   ioDiscard{},
	}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	appDir := filepath.Join(workDir, "demo")

	previousLook := lookPath
	lookPath = func(string) (string, error) { return "", errors.New("atlas missing") }
	t.Cleanup(func() { lookPath = previousLook })

	err := Generate(context.Background(), Options{
		WorkDir:   appDir,
		Name:      "Bus",
		Fields:    []string{"name:string"},
		Stdout:    ioDiscard{},
		skipAtlas: true,
	})
	if err != nil {
		t.Fatalf("Generate(Bus) error = %v", err)
	}

	err = Generate(context.Background(), Options{
		WorkDir:   appDir,
		Name:      "Buse",
		Fields:    []string{"name:string"},
		Stdout:    ioDiscard{},
		skipAtlas: true,
	})
	if err == nil {
		t.Fatal("Generate(Buse) error = nil, want HTTP path collision")
	}
	if !strings.Contains(err.Error(), "/buses") || !strings.Contains(err.Error(), "bus") {
		t.Fatalf("Generate(Buse) error = %q, want /buses already used by bus", err)
	}
	if _, statErr := os.Stat(filepath.Join(appDir, "internal", "buse")); !os.IsNotExist(statErr) {
		t.Fatal("Generate(Buse) wrote internal/buse after colliding with /buses")
	}

	err = Generate(context.Background(), Options{
		WorkDir:   appDir,
		Name:      "Bus",
		Fields:    []string{"name:string"},
		Stdout:    ioDiscard{},
		skipAtlas: true,
	})
	if err != nil {
		t.Fatalf("idempotent Generate(Bus) error = %v", err)
	}

	err = Generate(context.Background(), Options{
		WorkDir:   appDir,
		Name:      "Widget",
		Fields:    []string{"name:string"},
		Stdout:    ioDiscard{},
		skipAtlas: true,
	})
	if err != nil {
		t.Fatalf("Generate(Widget) error = %v, want distinct HTTP path to succeed", err)
	}
}

func TestGenerateNumberFieldEmptyIsZero(t *testing.T) {
	workDir := t.TempDir()
	if err := scaffold.Generate(context.Background(), scaffold.Options{
		Name:     "demo",
		Database: "sqlite",
		WorkDir:  workDir,
		Stdout:   ioDiscard{},
	}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	appDir := filepath.Join(workDir, "demo")

	previousLook := lookPath
	lookPath = func(string) (string, error) { return "", errors.New("atlas missing") }
	t.Cleanup(func() { lookPath = previousLook })

	err := Generate(context.Background(), Options{
		WorkDir:   appDir,
		Name:      "Widget",
		Fields:    []string{"qty:int"},
		Stdout:    ioDiscard{},
		skipAtlas: true,
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	formTS := readFile(t, filepath.Join(appDir, "frontend", "src", "widget", "form.tsx"))
	if strings.Contains(formTS, "valueAsNumber") {
		t.Fatal("form.tsx uses valueAsNumber; empty number inputs become NaN and JSON.stringify emits null")
	}
	if !strings.Contains(formTS, `setValueAs: (value) => (value === "" ? 0 : Number(value))`) {
		t.Fatalf("form.tsx missing setValueAs empty→0 for qty:\n%s", formTS)
	}
}
