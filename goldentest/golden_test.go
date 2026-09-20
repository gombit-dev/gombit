package goldentest

import (
	"bytes"
	"context"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/gombit-dev/gombit/client"
	"github.com/gombit-dev/gombit/commandgen"
	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/resourcegen"
	"github.com/gombit-dev/gombit/scaffold"
)

var update = flag.Bool("update", false, "regenerate testdata/golden trees")

func TestNewGolden(t *testing.T) {
	appDir := scaffoldDemo(t)
	got := snapshotTree(t, appDir)
	assertNoReplace(t, got)
	assertGoFormatted(t, got)
	assertFrontendInvariants(t, got)
	checkOrUpdateGolden(t, "new", got)

	t.Run("compile", func(t *testing.T) {
		compileBackend(t, appDir)
	})
	t.Run("typecheck", func(t *testing.T) {
		typecheckFrontend(t, appDir, false)
	})
	t.Run("idempotent", func(t *testing.T) {
		if err := scaffold.Generate(context.Background(), scaffold.Options{
			Name:     fixtureName,
			Module:   fixtureModule,
			Database: "sqlite",
			Cache:    "memory",
			Auth:     "jwt",
			UI:       "minimal",
			WorkDir:  filepath.Dir(appDir),
			Force:    true,
			Stdout:   io.Discard,
			IsTTY:    func() bool { return false },
		}); err != nil {
			t.Fatalf("idempotent gombit new --force: %v", err)
		}
		second := snapshotTree(t, appDir)
		if !treesEqual(got, second) {
			t.Fatalf("gombit new --force changed files:\n%s", treeDiffSummary(second, got))
		}
	})
}

func TestNewGoldenCookieAuth(t *testing.T) {
	appDir := scaffoldCookieDemo(t)
	got := snapshotTree(t, appDir)
	assertNoReplace(t, got)
	assertGoFormatted(t, got)
	assertCookieFrontendInvariants(t, got)
	checkOrUpdateGolden(t, "new-cookie", got)

	t.Run("compile", func(t *testing.T) {
		compileBackend(t, appDir)
	})
	t.Run("typecheck", func(t *testing.T) {
		typecheckFrontend(t, appDir, false)
	})
	t.Run("idempotent", func(t *testing.T) {
		if err := scaffold.Generate(context.Background(), scaffold.Options{
			Name:     fixtureName,
			Module:   fixtureModule,
			Database: "sqlite",
			Cache:    "memory",
			Auth:     "cookie",
			UI:       "minimal",
			WorkDir:  filepath.Dir(appDir),
			Force:    true,
			Stdout:   io.Discard,
			IsTTY:    func() bool { return false },
		}); err != nil {
			t.Fatalf("idempotent gombit new --auth cookie --force: %v", err)
		}
		second := snapshotTree(t, appDir)
		if !treesEqual(got, second) {
			t.Fatalf("gombit new --auth cookie --force changed files:\n%s", treeDiffSummary(second, got))
		}
	})
}

func TestNewGoldenMUI(t *testing.T) {
	appDir := scaffoldMUIDemo(t)
	got := snapshotTree(t, appDir)
	assertNoReplace(t, got)
	assertGoFormatted(t, got)
	assertFrontendInvariants(t, got)
	assertMUIFrontendInvariants(t, got)
	checkOrUpdateGolden(t, "new-mui", got)

	t.Run("compile", func(t *testing.T) {
		compileBackend(t, appDir)
	})
	t.Run("typecheck", func(t *testing.T) {
		typecheckFrontend(t, appDir, false)
	})
	t.Run("idempotent", func(t *testing.T) {
		if err := scaffold.Generate(context.Background(), scaffold.Options{
			Name:     fixtureName,
			Module:   fixtureModule,
			Database: "sqlite",
			Cache:    "memory",
			Auth:     "jwt",
			UI:       "mui",
			WorkDir:  filepath.Dir(appDir),
			Force:    true,
			Stdout:   io.Discard,
			IsTTY:    func() bool { return false },
		}); err != nil {
			t.Fatalf("idempotent gombit new --ui mui --force: %v", err)
		}
		second := snapshotTree(t, appDir)
		if !treesEqual(got, second) {
			t.Fatalf("gombit new --ui mui --force changed files:\n%s", treeDiffSummary(second, got))
		}
	})
}

func TestMakeResourceGolden(t *testing.T) {
	appDir := scaffoldDemo(t)
	stdout := new(bytes.Buffer)
	if err := resourcegen.Generate(context.Background(), resourcegen.Options{
		WorkDir:  appDir,
		Name:     fixtureBook,
		Fields:   []string{fixtureFields},
		AtlasBin: missingAtlas,
		Stdout:   stdout,
		Stderr:   io.Discard,
	}); err != nil {
		t.Fatalf("gombit make resource: %v\nstdout=%s", err, stdout.String())
	}

	got := snapshotTree(t, appDir)
	assertNoReplace(t, got)
	assertGoFormatted(t, got)
	assertFrontendInvariants(t, got)
	checkOrUpdateGolden(t, "make-resource", got)

	mainSrc := got["cmd/server/main.go"]
	count, err := resourcegen.CountRegisterCalls(mainSrc, "book")
	if err != nil {
		t.Fatalf("CountRegisterCalls: %v", err)
	}
	if count != 1 {
		t.Fatalf("book.Register count = %d, want 1\n%s", count, mainSrc)
	}

	t.Run("compile", func(t *testing.T) {
		compileBackend(t, appDir)
	})
	t.Run("typecheck", func(t *testing.T) {
		typecheckFrontend(t, appDir, true)
	})
	t.Run("idempotent", func(t *testing.T) {
		if err := resourcegen.Generate(context.Background(), resourcegen.Options{
			WorkDir:  appDir,
			Name:     fixtureBook,
			Fields:   []string{fixtureFields},
			AtlasBin: missingAtlas,
			Stdout:   io.Discard,
			Stderr:   io.Discard,
		}); err != nil {
			t.Fatalf("idempotent make resource: %v", err)
		}
		second := snapshotTree(t, appDir)
		if !treesEqual(got, second) {
			t.Fatalf("second make resource changed files:\n%s", treeDiffSummary(second, got))
		}
		count, err := resourcegen.CountRegisterCalls(second["cmd/server/main.go"], "book")
		if err != nil {
			t.Fatalf("CountRegisterCalls after re-run: %v", err)
		}
		if count != 1 {
			t.Fatalf("re-run duplicated book.Register: count = %d", count)
		}
	})
}

// TestMakeResourceScalarTypesCompiles exercises the #222 scalar grammar
// (decimal/time/enum) end to end: a generated resource using all three must
// compile against the framework (including types.Decimal). No golden tree is
// committed — the point is that the generated Go builds and the DTO matches the
// model for the newly supported types.
func TestMakeResourceScalarTypesCompiles(t *testing.T) {
	appDir := scaffoldDemo(t)
	stdout := new(bytes.Buffer)
	if err := resourcegen.Generate(context.Background(), resourcegen.Options{
		WorkDir: appDir,
		Name:    "Rental",
		Fields: []string{
			"price:decimal:required",
			"deposit:decimal(10,2)",
			"starts_at:time:required",
			"status:enum(requested,confirmed,active,returned,cancelled)",
		},
		AtlasBin: missingAtlas,
		Stdout:   stdout,
		Stderr:   io.Discard,
	}); err != nil {
		t.Fatalf("gombit make resource (scalars): %v\nstdout=%s", err, stdout.String())
	}
	t.Run("compile", func(t *testing.T) {
		compileBackend(t, appDir)
	})
}

// TestMakeResourceRelationsCompiles exercises the #222(b) relation grammar end
// to end: it generates the target models, then a resource with belongs_to /
// has_many / many_to_many fields, and compiles the app (the generated model
// imports the target feature-packages and references their types).
func TestMakeResourceRelationsCompiles(t *testing.T) {
	appDir := scaffoldDemo(t)
	gen := func(name string, fields ...string) {
		t.Helper()
		stdout := new(bytes.Buffer)
		if err := resourcegen.Generate(context.Background(), resourcegen.Options{
			WorkDir:  appDir,
			Name:     name,
			Fields:   fields,
			AtlasBin: missingAtlas,
			Stdout:   stdout,
			Stderr:   io.Discard,
		}); err != nil {
			t.Fatalf("gombit make resource %s: %v\nstdout=%s", name, err, stdout.String())
		}
	}
	// Target models first, so the relation imports resolve.
	gen("Engine", "name:string:required")
	gen("Warehouse", "name:string:required")
	// The has_many child carries the back-reference FK as a plain column (no
	// import, so no cycle): rental_id -> RentalID, which GORM's has_many uses.
	gen("Part", "name:string:required", "rental_id:uint")
	gen("Rental",
		"price:decimal:required",
		"engine:belongs_to:Engine",
		"parts:has_many:Part",
		"warehouses:many_to_many:Warehouse",
	)
	t.Run("compile", func(t *testing.T) {
		compileBackend(t, appDir)
	})
}

// TestMakeResourceListQueryCompiles exercises the #260 declared list-query
// grammar end to end: a resource declaring searchable / sortable / filterable
// fields plus a belongs_to (filterable by default) must generate a handler that
// compiles against the framework's database list-query helpers.
func TestMakeResourceListQueryCompiles(t *testing.T) {
	appDir := scaffoldDemo(t)
	gen := func(name string, fields ...string) {
		t.Helper()
		stdout := new(bytes.Buffer)
		if err := resourcegen.Generate(context.Background(), resourcegen.Options{
			WorkDir:  appDir,
			Name:     name,
			Fields:   fields,
			AtlasBin: missingAtlas,
			Stdout:   stdout,
			Stderr:   io.Discard,
		}); err != nil {
			t.Fatalf("gombit make resource %s: %v\nstdout=%s", name, err, stdout.String())
		}
	}
	gen("Author", "name:string:required")
	gen("Article",
		"title:string:required,searchable,sortable",
		"body:text:searchable",
		"views:int:filterable,sortable",
		"published:bool:filterable",
		"status:enum(draft,published):filterable,sortable",
		"author:belongs_to:Author",
	)
	t.Run("compile", func(t *testing.T) {
		compileBackend(t, appDir)
	})
}

// TestMakeResourceAggregatesCompiles exercises the #272 aggregatable grammar end
// to end: a resource declaring aggregatable numeric fields (int + decimal), mixed
// with filter/search/sort, must generate a list handler that compiles against the
// framework's database.Aggregate / contract.ListMeta surface.
func TestMakeResourceAggregatesCompiles(t *testing.T) {
	appDir := scaffoldDemo(t)
	gen := func(name string, fields ...string) {
		t.Helper()
		stdout := new(bytes.Buffer)
		if err := resourcegen.Generate(context.Background(), resourcegen.Options{
			WorkDir:  appDir,
			Name:     name,
			Fields:   fields,
			AtlasBin: missingAtlas,
			Stdout:   stdout,
			Stderr:   io.Discard,
		}); err != nil {
			t.Fatalf("gombit make resource %s: %v\nstdout=%s", name, err, stdout.String())
		}
	}
	gen("Customer", "name:string:required")
	gen("Invoice",
		"total:decimal:required,aggregatable",
		"quantity:int:aggregatable,filterable,sortable",
		"status:enum(draft,paid):filterable",
		"customer:belongs_to:Customer",
	)
	t.Run("compile", func(t *testing.T) {
		compileBackend(t, appDir)
	})
}

func TestMakeCommandGolden(t *testing.T) {
	appDir := scaffoldDemo(t)
	stdout := new(bytes.Buffer)
	if err := commandgen.Generate(context.Background(), commandgen.Options{
		WorkDir: appDir,
		Name:    "greet",
		Stdout:  stdout,
	}); err != nil {
		t.Fatalf("gombit make command: %v\nstdout=%s", err, stdout.String())
	}

	got := snapshotTree(t, appDir)
	assertNoReplace(t, got)
	assertGoFormatted(t, got)
	checkOrUpdateGolden(t, "make-command", got)

	mainSrc := got["cmd/gombit/main.go"]
	count, err := commandgen.CountRegisterCalls(mainSrc, "commands")
	if err != nil {
		t.Fatalf("CountRegisterCalls: %v", err)
	}
	if count != 1 {
		t.Fatalf("commands.RegisterCommands count = %d, want 1\n%s", count, mainSrc)
	}
	registerSrc := got["internal/commands/commands.go"]
	ctor, err := commandgen.CountConstructorCalls(registerSrc, "NewGreetCommand")
	if err != nil {
		t.Fatalf("CountConstructorCalls: %v", err)
	}
	if ctor != 1 {
		t.Fatalf("NewGreetCommand count = %d, want 1\n%s", ctor, registerSrc)
	}
	if !bytes.Contains(registerSrc, []byte("func RegisterCommands")) {
		t.Fatal("internal/commands/commands.go missing RegisterCommands")
	}

	t.Run("compile", func(t *testing.T) {
		compileBackend(t, appDir)
	})
	t.Run("idempotent", func(t *testing.T) {
		if err := commandgen.Generate(context.Background(), commandgen.Options{
			WorkDir: appDir,
			Name:    "greet",
			Stdout:  io.Discard,
		}); err != nil {
			t.Fatalf("idempotent make command: %v", err)
		}
		second := snapshotTree(t, appDir)
		if !treesEqual(got, second) {
			t.Fatalf("second make command changed files:\n%s", treeDiffSummary(second, got))
		}
		count, err := commandgen.CountRegisterCalls(second["cmd/gombit/main.go"], "commands")
		if err != nil {
			t.Fatalf("CountRegisterCalls after re-run: %v", err)
		}
		if count != 1 {
			t.Fatalf("re-run duplicated commands.RegisterCommands: count = %d", count)
		}
	})
}

func TestClientGenerateGolden(t *testing.T) {
	requireNode(t)

	workDir := t.TempDir()
	specPath := writeSampleSpec(t, workDir)
	outDir := filepath.Join(workDir, "frontend", "src", "api", "generated")
	if err := client.Generate(context.Background(), client.Options{
		WorkDir:  workDir,
		SpecPath: specPath,
		OutDir:   outDir,
		Stdout:   io.Discard,
		Stderr:   io.Discard,
	}); err != nil {
		t.Fatalf("gombit client generate: %v", err)
	}

	got := snapshotTree(t, outDir)
	checkOrUpdateGolden(t, "client", got)

	t.Run("typecheck", func(t *testing.T) {
		typecheckClient(t, workDir, outDir)
	})
	t.Run("idempotent", func(t *testing.T) {
		if err := client.Generate(context.Background(), client.Options{
			WorkDir:  workDir,
			SpecPath: specPath,
			OutDir:   outDir,
			Stdout:   io.Discard,
			Stderr:   io.Discard,
		}); err != nil {
			t.Fatalf("idempotent client generate: %v", err)
		}
		second := snapshotTree(t, outDir)
		if !treesEqual(got, second) {
			t.Fatalf("second client generate changed files:\n%s", treeDiffSummary(second, got))
		}
	})
}

func checkOrUpdateGolden(t *testing.T, name string, got fileMap) {
	t.Helper()
	if *update {
		writeGolden(t, name, got)
		t.Logf("updated golden %s (%d files)", name, len(got))
		return
	}
	want := loadGolden(t, name)
	compareTrees(t, got, want)
}

func scaffoldDemo(t *testing.T) string {
	t.Helper()
	workDir := t.TempDir()
	err := scaffold.Generate(context.Background(), scaffold.Options{
		Name:     fixtureName,
		Module:   fixtureModule,
		Database: "sqlite",
		Cache:    "memory",
		Auth:     "jwt",
		UI:       "minimal",
		WorkDir:  workDir,
		Stdout:   io.Discard,
		Stderr:   io.Discard,
		IsTTY:    func() bool { return false },
	})
	if err != nil {
		t.Fatalf("gombit new: %v", err)
	}
	return filepath.Join(workDir, fixtureName)
}

func scaffoldCookieDemo(t *testing.T) string {
	t.Helper()
	workDir := t.TempDir()
	err := scaffold.Generate(context.Background(), scaffold.Options{
		Name:     fixtureName,
		Module:   fixtureModule,
		Database: "sqlite",
		Cache:    "memory",
		Auth:     "cookie",
		UI:       "minimal",
		WorkDir:  workDir,
		Stdout:   io.Discard,
		Stderr:   io.Discard,
		IsTTY:    func() bool { return false },
	})
	if err != nil {
		t.Fatalf("gombit new --auth cookie: %v", err)
	}
	return filepath.Join(workDir, fixtureName)
}

func scaffoldMUIDemo(t *testing.T) string {
	t.Helper()
	workDir := t.TempDir()
	err := scaffold.Generate(context.Background(), scaffold.Options{
		Name:     fixtureName,
		Module:   fixtureModule,
		Database: "sqlite",
		Cache:    "memory",
		Auth:     "jwt",
		UI:       "mui",
		WorkDir:  workDir,
		Stdout:   io.Discard,
		Stderr:   io.Discard,
		IsTTY:    func() bool { return false },
	})
	if err != nil {
		t.Fatalf("gombit new --ui mui: %v", err)
	}
	return filepath.Join(workDir, fixtureName)
}

func writeSampleSpec(t *testing.T, workDir string) string {
	t.Helper()
	app, err := client.SampleApp()
	if err != nil {
		t.Fatalf("SampleApp() error = %v", err)
	}
	path := filepath.Join(workDir, "openapi.json")
	if err := contract.WriteOpenAPI(path, app.API()); err != nil {
		t.Fatalf("WriteOpenAPI: %v", err)
	}
	return path
}

func typecheckClient(t *testing.T, workDir, outDir string) {
	t.Helper()
	relOut, err := filepath.Rel(workDir, outDir)
	if err != nil {
		t.Fatalf("rel: %v", err)
	}
	relOut = filepath.ToSlash(relOut)
	writeFile(t, filepath.Join(workDir, "package.json"), `{
  "name": "gombit-golden-client",
  "private": true,
  "type": "module",
  "dependencies": {
    "openapi-fetch": "0.17.0"
  },
  "devDependencies": {
    "typescript": "5.9.3"
  }
}
`)
	writeFile(t, filepath.Join(workDir, "tsconfig.json"), `{
  "compilerOptions": {
    "target": "ES2022",
    "module": "ES2022",
    "moduleResolution": "bundler",
    "strict": true,
    "skipLibCheck": true,
    "noEmit": true
  },
  "include": ["`+relOut+`/**/*.ts"]
}
`)
	install := exec.Command("npm", "install", "--no-fund", "--no-audit", "--ignore-scripts")
	install.Dir = workDir
	install.Env = append(os.Environ(), "npm_config_update_notifier=false")
	if out, err := install.CombinedOutput(); err != nil {
		t.Fatalf("npm install: %v\n%s", err, out)
	}
	//nolint:gosec // workDir is t.TempDir; tsc args are fixed
	tsc := exec.Command("npx", "--no-install", "tsc", "--noEmit", "-p", workDir)
	tsc.Dir = workDir
	if out, err := tsc.CombinedOutput(); err != nil {
		t.Fatalf("npx tsc --noEmit: %v\n%s", err, out)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
