package migrations

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/config"
)

func TestParseModel(t *testing.T) {
	tests := []struct {
		name    string
		spec    string
		want    Model
		wantErr bool
	}{
		{
			name: "valid",
			spec: "github.com/example/app/internal/product.Product",
			want: Model{
				ImportPath: "github.com/example/app/internal/product",
				TypeName:   "Product",
			},
		},
		{name: "empty", spec: "", wantErr: true},
		{name: "missing type", spec: "github.com/example/app/internal/product.", wantErr: true},
		{name: "missing import", spec: ".Product", wantErr: true},
		{name: "bad type", spec: "github.com/example/app/internal/product.123Product", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseModel(tt.spec)
			if tt.wantErr {
				if err == nil {
					t.Fatal("ParseModel() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseModel() error = %v, want nil", err)
			}
			if got != tt.want {
				t.Fatalf("ParseModel() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestMakeMigrationsRunsAtlasDiffForSupportedDrivers(t *testing.T) {
	tests := []struct {
		driver config.DatabaseDriver
		devURL string
	}{
		{driver: config.DatabaseDriverSQLite, devURL: "sqlite://file?mode=memory&_fk=1"},
		{driver: config.DatabaseDriverPostgres, devURL: "docker://postgres/15/dev?search_path=public"},
		{driver: config.DatabaseDriverMySQL, devURL: "docker://mysql/8/dev"},
	}

	for _, tt := range tests {
		t.Run(string(tt.driver), func(t *testing.T) {
			workDir := t.TempDir()
			runner := &recordingRunner{t: t}
			stdout := new(bytes.Buffer)

			err := MakeMigrations(context.Background(), Options{
				WorkDir:      workDir,
				Name:         "create_products",
				Driver:       tt.driver,
				MigrationDir: "database/migrations",
				AtlasBinary:  "atlas-test",
				Models: []Model{
					{ImportPath: "github.com/example/app/internal/product", TypeName: "Product"},
					{ImportPath: "github.com/example/app/internal/account", TypeName: "Account"},
				},
				Stdout: stdout,
				runner: runner,
			})
			if err != nil {
				t.Fatalf("MakeMigrations() error = %v, want nil", err)
			}

			wantArgs := []string{"migrate", "diff", "create_products", "--env", "gombit", "--config"}
			if len(runner.args) != len(wantArgs)+1 {
				t.Fatalf("atlas args = %v, want prefix %v plus config URL", runner.args, wantArgs)
			}
			for i, want := range wantArgs {
				if runner.args[i] != want {
					t.Fatalf("atlas args[%d] = %q, want %q (all args: %v)", i, runner.args[i], want, runner.args)
				}
			}
			if runner.dir != workDir {
				t.Fatalf("atlas dir = %q, want %q", runner.dir, workDir)
			}
			if runner.name != "atlas-test" {
				t.Fatalf("atlas name = %q, want atlas-test", runner.name)
			}
			wantGoArgs := []string{"run", "-mod=mod"}
			if len(runner.goArgs) != len(wantGoArgs)+1 {
				t.Fatalf("go args = %v, want prefix %v plus loader path", runner.goArgs, wantGoArgs)
			}
			for i, want := range wantGoArgs {
				if runner.goArgs[i] != want {
					t.Fatalf("go args[%d] = %q, want %q (all args: %v)", i, runner.goArgs[i], want, runner.goArgs)
				}
			}
			if !strings.HasPrefix(runner.goArgs[len(runner.goArgs)-1], "./.gombit/") {
				t.Fatalf("go loader arg = %q, want ./.gombit path", runner.goArgs[len(runner.goArgs)-1])
			}

			atlasHCL := runner.atlasHCL
			for _, want := range []string{
				`env "gombit"`,
				`src = "file://`,
				tt.devURL,
				"file://" + filepath.ToSlash(filepath.Join(workDir, "database/migrations")),
			} {
				if !strings.Contains(atlasHCL, want) {
					t.Fatalf("atlas.hcl = %q, want it to contain %q", atlasHCL, want)
				}
			}
			if strings.Contains(atlasHCL, "external_schema") {
				t.Fatalf("atlas.hcl = %q, want Community-compatible file schema source", atlasHCL)
			}

			loader := runner.loader
			for _, want := range []string{
				`"ariga.io/atlas-provider-gorm/gormschema"`,
				`model0 "github.com/example/app/internal/product"`,
				`model1 "github.com/example/app/internal/account"`,
				`gormschema.New("` + string(tt.driver) + `").Load(`,
				"&model0.Product{}",
				"&model1.Account{}",
			} {
				if !strings.Contains(loader, want) {
					t.Fatalf("loader = %q, want it to contain %q", loader, want)
				}
			}

			migrationFile := filepath.Join(workDir, "database/migrations", "20260101000000_create_products.sql")
			if _, err := os.Stat(migrationFile); err != nil {
				t.Fatalf("expected fake atlas migration file: %v", err)
			}
			if !strings.Contains(stdout.String(), "created migration") {
				t.Fatalf("stdout = %q, want fake atlas output", stdout.String())
			}
		})
	}
}

func TestMakeMigrationsRejectsUntrackedForgetModel(t *testing.T) {
	workDir := t.TempDir()
	runner := &recordingRunner{t: t}

	err := MakeMigrations(context.Background(), Options{
		WorkDir:      workDir,
		Name:         "drop_widget",
		Driver:       config.DatabaseDriverSQLite,
		MigrationDir: "database/migrations",
		AtlasBinary:  "atlas-test",
		Models: []Model{
			{ImportPath: "github.com/example/app/internal/product", TypeName: "Product"},
		},
		ForgetModels: []Model{
			{ImportPath: "github.com/example/app/internal/widget", TypeName: "Widget"},
		},
		Stdout: io.Discard,
		runner: runner,
	})
	if err == nil {
		t.Fatal("MakeMigrations() error = nil, want an untracked --forget-model error")
	}
	if !strings.Contains(err.Error(), "not tracked") {
		t.Fatalf("error = %v, want it to say the model is not tracked", err)
	}
	// A silent no-op is the exact bug (#300): the run must not reach Atlas nor
	// leave a migration directory behind as a side effect.
	if runner.name != "" || runner.goName != "" {
		t.Fatalf("nothing should run for an untracked forget; ran name=%q goName=%q", runner.name, runner.goName)
	}
	if _, statErr := os.Stat(filepath.Join(workDir, "database/migrations")); !os.IsNotExist(statErr) {
		t.Fatalf("migration dir must not be created on a rejected run; stat err = %v", statErr)
	}
}

func TestMakeMigrationsForgetsTrackedModel(t *testing.T) {
	workDir := t.TempDir()
	migrationDir := filepath.Join(workDir, "database/migrations")
	if err := os.MkdirAll(migrationDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := SaveRegistry(migrationDir, []Model{
		{ImportPath: "github.com/example/app/internal/product", TypeName: "Product"},
		{ImportPath: "github.com/example/app/internal/widget", TypeName: "Widget"},
	}); err != nil {
		t.Fatal(err)
	}

	runner := &recordingRunner{t: t}
	err := MakeMigrations(context.Background(), Options{
		WorkDir:      workDir,
		Name:         "drop_widget",
		Driver:       config.DatabaseDriverSQLite,
		MigrationDir: "database/migrations",
		AtlasBinary:  "atlas-test",
		ForgetModels: []Model{
			{ImportPath: "github.com/example/app/internal/widget", TypeName: "Widget"},
		},
		Stdout: io.Discard,
		runner: runner,
	})
	if err != nil {
		t.Fatalf("MakeMigrations() error = %v, want nil for a tracked forget", err)
	}
	got, err := LoadRegistry(migrationDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].TypeName != "Product" {
		t.Fatalf("registry after forget = %#v, want only Product", got)
	}
	// The desired-state loader the diff runs on must drop the forgotten model.
	if strings.Contains(runner.loader, "Widget") {
		t.Fatalf("loader must not reference the forgotten Widget: %q", runner.loader)
	}
}

func TestMakeMigrationsGeneratedLoaderUsesRealGormschema(t *testing.T) {
	workDir := projectRoot(t)
	migrationDir := t.TempDir()
	runner := &gormschemaRunner{t: t}

	err := MakeMigrations(context.Background(), Options{
		WorkDir:      workDir,
		Name:         "create_products",
		Driver:       config.DatabaseDriverSQLite,
		MigrationDir: migrationDir,
		AtlasBinary:  "atlas-test",
		Models: []Model{
			{ImportPath: "github.com/gombit-dev/gombit/migrations/testmodels", TypeName: "Product"},
		},
		runner: runner,
	})
	if err != nil {
		t.Fatalf("MakeMigrations() error = %v, want nil", err)
	}

	for _, want := range []string{
		"CREATE TABLE `products`",
		"`name` text",
		"`price` integer",
		"CREATE INDEX `idx_products_deleted_at`",
	} {
		if !strings.Contains(runner.schema, want) {
			t.Fatalf("generated schema = %q, want it to contain %q", runner.schema, want)
		}
	}

	migrationFile := filepath.Join(migrationDir, "20260101000000_create_products.sql")
	// #nosec G304 -- migrationFile is built from t.TempDir and a fixed filename.
	data, err := os.ReadFile(migrationFile)
	if err != nil {
		t.Fatalf("read generated migration file: %v", err)
	}
	if !strings.Contains(string(data), "CREATE TABLE `products`") {
		t.Fatalf("migration file = %q, want real gormschema output", string(data))
	}
}

func TestMakeMigrationsRunsAtlasCLISQLiteWhenAvailable(t *testing.T) {
	atlasBin := os.Getenv("ATLAS_BINARY")
	if atlasBin == "" {
		var err error
		atlasBin, err = exec.LookPath("atlas")
		if err != nil {
			t.Skip("Atlas CLI not found; set ATLAS_BINARY to run the real SQLite makemigrations smoke test")
		}
	}

	migrationDir := t.TempDir()
	err := MakeMigrations(context.Background(), Options{
		WorkDir:      projectRoot(t),
		Name:         "create_products",
		Driver:       config.DatabaseDriverSQLite,
		MigrationDir: migrationDir,
		AtlasBinary:  atlasBin,
		Models: []Model{
			{ImportPath: "github.com/gombit-dev/gombit/migrations/testmodels", TypeName: "Product"},
		},
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	if err != nil {
		t.Fatalf("MakeMigrations() error = %v, want nil", err)
	}

	files, err := filepath.Glob(filepath.Join(migrationDir, "*.sql"))
	if err != nil {
		t.Fatalf("glob migration files: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("migration files = %v, want one SQL migration", files)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("read migration file: %v", err)
	}
	for _, want := range []string{"CREATE TABLE", "products", "name", "price"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("migration file = %q, want it to contain %q", string(data), want)
		}
	}
}

// TestMakeMigrationsSecondModelDoesNotDropTheFirst is the regression test for
// #97: a migration that names one new model must not propose dropping the
// table an earlier migration created for a model this invocation doesn't
// repeat.
func TestMakeMigrationsSecondModelDoesNotDropTheFirst(t *testing.T) {
	atlasBin := os.Getenv("ATLAS_BINARY")
	if atlasBin == "" {
		var err error
		atlasBin, err = exec.LookPath("atlas")
		if err != nil {
			t.Skip("Atlas CLI not found; set ATLAS_BINARY to run the real SQLite makemigrations smoke test")
		}
	}

	migrationDir := t.TempDir()
	ctx := context.Background()
	root := projectRoot(t)

	// Migration 1 names only Product, exactly like a single-feature app.
	err := MakeMigrations(ctx, Options{
		WorkDir:      root,
		Name:         "create_products",
		Driver:       config.DatabaseDriverSQLite,
		MigrationDir: migrationDir,
		AtlasBinary:  atlasBin,
		Models: []Model{
			{ImportPath: "github.com/gombit-dev/gombit/migrations/testmodels", TypeName: "Product"},
		},
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	if err != nil {
		t.Fatalf("MakeMigrations() [1] error = %v, want nil", err)
	}

	registered, err := LoadRegistry(migrationDir)
	if err != nil {
		t.Fatalf("LoadRegistry() after migration 1: %v", err)
	}
	if len(registered) != 1 || registered[0].TypeName != "Product" {
		t.Fatalf("registry after migration 1 = %#v, want just Product", registered)
	}

	// Migration 2 names only Account — a second feature added later, the way
	// the tutorial's own chapter 3 teaches. It must NOT drop products.
	err = MakeMigrations(ctx, Options{
		WorkDir:      root,
		Name:         "create_accounts",
		Driver:       config.DatabaseDriverSQLite,
		MigrationDir: migrationDir,
		AtlasBinary:  atlasBin,
		Models: []Model{
			{ImportPath: "github.com/gombit-dev/gombit/migrations/testmodels", TypeName: "Account"},
		},
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	if err != nil {
		t.Fatalf("MakeMigrations() [2] error = %v, want nil", err)
	}

	files, err := filepath.Glob(filepath.Join(migrationDir, "*.sql"))
	if err != nil {
		t.Fatalf("glob migration files: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("migration files = %v, want two SQL migrations", files)
	}

	var secondMigration string
	for _, f := range files {
		if strings.Contains(f, "create_accounts") {
			secondMigration = f
		}
	}
	if secondMigration == "" {
		t.Fatalf("migration files = %v, want one named create_accounts", files)
	}
	// #nosec G304 -- secondMigration comes from filepath.Glob over migrationDir
	data, err := os.ReadFile(secondMigration)
	if err != nil {
		t.Fatalf("read migration file: %v", err)
	}
	content := string(data)
	if strings.Contains(strings.ToUpper(content), "DROP TABLE") {
		t.Fatalf("migration 2 = %q, must not drop the products table from migration 1 (#97)", content)
	}
	if !strings.Contains(content, "accounts") {
		t.Fatalf("migration 2 = %q, want it to create the accounts table", content)
	}

	registered, err = LoadRegistry(migrationDir)
	if err != nil {
		t.Fatalf("LoadRegistry() after migration 2: %v", err)
	}
	if len(registered) != 2 {
		t.Fatalf("registry after migration 2 = %#v, want both Product and Account", registered)
	}
}

// TestMakeMigrationsFullListAfterIncrementalCallsIsNoop is the regression
// test for #96: gombit make resource always diffs the *entire* AutoMigrate
// model list (resourcegen.CollectAutoMigrateModels), not just the resource
// being generated. Before the bootstrap migration (seeded by gombit new)
// and the model registry (#97) existed together, a full-list call like that
// would "discover" AutoMigrate-created tables the registry had never heard
// of and try to CREATE them again, and applying that migration failed with
// "table already exists" because AutoMigrate had already created them live.
//
// With bootstrap migration + registry both in place, a later call that
// names every currently-registered model (exactly what resourcegen does)
// must be a no-op: nothing left to create, so Atlas writes no new
// migration file at all.
func TestMakeMigrationsFullListAfterIncrementalCallsIsNoop(t *testing.T) {
	atlasBin := os.Getenv("ATLAS_BINARY")
	if atlasBin == "" {
		var err error
		atlasBin, err = exec.LookPath("atlas")
		if err != nil {
			t.Skip("Atlas CLI not found; set ATLAS_BINARY to run the real SQLite makemigrations smoke test")
		}
	}

	migrationDir := t.TempDir()
	ctx := context.Background()
	root := projectRoot(t)
	const testmodelsPkg = "github.com/gombit-dev/gombit/migrations/testmodels"

	// "bootstrap": Product only, the way gombit new would seed it.
	err := MakeMigrations(ctx, Options{
		WorkDir:      root,
		Name:         "bootstrap",
		Driver:       config.DatabaseDriverSQLite,
		MigrationDir: migrationDir,
		AtlasBinary:  atlasBin,
		Models:       []Model{{ImportPath: testmodelsPkg, TypeName: "Product"}},
		Stdout:       io.Discard,
		Stderr:       io.Discard,
	})
	if err != nil {
		t.Fatalf("MakeMigrations() [bootstrap] error = %v, want nil", err)
	}

	// chapter 3's own pattern: a single new model, not repeating Product.
	err = MakeMigrations(ctx, Options{
		WorkDir:      root,
		Name:         "create_accounts",
		Driver:       config.DatabaseDriverSQLite,
		MigrationDir: migrationDir,
		AtlasBinary:  atlasBin,
		Models:       []Model{{ImportPath: testmodelsPkg, TypeName: "Account"}},
		Stdout:       io.Discard,
		Stderr:       io.Discard,
	})
	if err != nil {
		t.Fatalf("MakeMigrations() [create_accounts] error = %v, want nil", err)
	}

	before, err := filepath.Glob(filepath.Join(migrationDir, "*.sql"))
	if err != nil {
		t.Fatalf("glob migration files before: %v", err)
	}
	if len(before) != 2 {
		t.Fatalf("migration files before = %v, want two", before)
	}

	// resourcegen's own call shape: every currently-registered model, not
	// just the one being added — both are already tracked, so this must not
	// try to CREATE anything, since AutoMigrate would already have made
	// both tables live and Atlas now knows about them too.
	stdout := new(bytes.Buffer)
	err = MakeMigrations(ctx, Options{
		WorkDir:      root,
		Name:         "create_accounts", // resourcegen names it after the new resource, same as chapter 4
		Driver:       config.DatabaseDriverSQLite,
		MigrationDir: migrationDir,
		AtlasBinary:  atlasBin,
		Models: []Model{
			{ImportPath: testmodelsPkg, TypeName: "Product"},
			{ImportPath: testmodelsPkg, TypeName: "Account"},
		},
		Stdout: stdout,
		Stderr: io.Discard,
	})
	if err != nil {
		t.Fatalf("MakeMigrations() [full list] error = %v, want nil (this is #96 if it fails)", err)
	}

	after, err := filepath.Glob(filepath.Join(migrationDir, "*.sql"))
	if err != nil {
		t.Fatalf("glob migration files after: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("migration files after full-list call = %v, want no new file (got %d, had %d): %s",
			after, len(after), len(before), stdout.String())
	}
}

func TestMakeMigrationsValidatesOptions(t *testing.T) {
	tests := []struct {
		name string
		opts Options
	}{
		{name: "missing name", opts: Options{Driver: config.DatabaseDriverSQLite, Models: []Model{{ImportPath: "example.com/app/product", TypeName: "Product"}}}},
		{name: "name with spaces", opts: Options{Name: "create products", Driver: config.DatabaseDriverSQLite, Models: []Model{{ImportPath: "example.com/app/product", TypeName: "Product"}}}},
		{name: "name starting with hyphen", opts: Options{Name: "-create_products", Driver: config.DatabaseDriverSQLite, Models: []Model{{ImportPath: "example.com/app/product", TypeName: "Product"}}}},
		{name: "unsupported driver", opts: Options{Name: "x", Driver: "oracle", Models: []Model{{ImportPath: "example.com/app/product", TypeName: "Product"}}}},
		{name: "missing models", opts: Options{Name: "x", Driver: config.DatabaseDriverSQLite}},
		{name: "invalid model", opts: Options{Name: "x", Driver: config.DatabaseDriverSQLite, Models: []Model{{ImportPath: "example.com/app/product", TypeName: "123"}}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := tt.opts
			// "missing models" is only rejected once the persisted registry
			// is consulted and found empty too — give it a real, isolated
			// WorkDir regardless, so no case can touch the package directory.
			opts.WorkDir = t.TempDir()
			if err := MakeMigrations(context.Background(), opts); err == nil {
				t.Fatal("MakeMigrations() error = nil, want error")
			}
		})
	}
}

// TestMakeMigrationsNoModelsDoesNotCreateMigrationDir guards against a
// regression where rejecting "nothing to migrate" would still leave behind
// an empty database/migrations/ as a side effect of checking.
func TestMakeMigrationsNoModelsDoesNotCreateMigrationDir(t *testing.T) {
	workDir := t.TempDir()
	err := MakeMigrations(context.Background(), Options{
		WorkDir: workDir,
		Name:    "create_products",
		Driver:  config.DatabaseDriverSQLite,
	})
	if err == nil {
		t.Fatal("MakeMigrations() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "no models to migrate") {
		t.Fatalf("error = %q, want it to explain there's nothing to migrate", err)
	}
	if _, statErr := os.Stat(filepath.Join(workDir, "database", "migrations")); !os.IsNotExist(statErr) {
		t.Fatalf("database/migrations was created despite MakeMigrations() failing: stat err = %v", statErr)
	}
}

type recordingRunner struct {
	t      *testing.T
	dir    string
	name   string
	args   []string
	goDir  string
	goName string
	goArgs []string

	atlasHCL string
	loader   string
	schema   string
}

func (r *recordingRunner) Run(_ context.Context, dir string, name string, args []string, stdout io.Writer, stderr io.Writer) error {
	if name == "go" {
		r.goDir = dir
		r.goName = name
		r.goArgs = append([]string(nil), args...)
		r.loader = r.readLoaderFromGoArgs(dir, args)
		_, _ = stdout.Write([]byte("-- fake gorm schema\n"))
		return nil
	}

	r.dir = dir
	r.name = name
	r.args = append([]string(nil), args...)
	r.atlasHCL = r.readConfig()
	r.schema = r.readSchema(r.atlasHCL)
	migrationDir := parseMigrationDir(r.t, r.atlasHCL)
	if err := os.MkdirAll(migrationDir, 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(migrationDir, "20260101000000_create_products.sql"), []byte("-- fake atlas migration\n"), 0o600); err != nil {
		return err
	}
	_, _ = stdout.Write([]byte("created migration\n"))
	return nil
}

func (r *recordingRunner) readConfig() string {
	r.t.Helper()
	if len(r.args) == 0 {
		r.t.Fatal("atlas args are empty")
	}
	configURL := r.args[len(r.args)-1]
	const prefix = "file://"
	if !strings.HasPrefix(configURL, prefix) {
		r.t.Fatalf("config arg = %q, want file URL", configURL)
	}
	data, err := os.ReadFile(strings.TrimPrefix(configURL, prefix))
	if err != nil {
		r.t.Fatalf("read atlas config: %v", err)
	}
	return string(data)
}

func (r *recordingRunner) readLoaderFromGoArgs(dir string, args []string) string {
	r.t.Helper()

	if len(args) < 1 {
		r.t.Fatal("go args are empty")
	}
	loaderRel := strings.TrimPrefix(args[len(args)-1], "./")
	// #nosec G304 -- loaderRel is generated by MakeMigrations under the test work dir.
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(loaderRel), "main.go"))
	if err != nil {
		r.t.Fatalf("read loader: %v", err)
	}
	return string(data)
}

func (r *recordingRunner) readSchema(atlasHCL string) string {
	r.t.Helper()

	data, err := os.ReadFile(parseSchemaPath(r.t, atlasHCL))
	if err != nil {
		r.t.Fatalf("read schema: %v", err)
	}
	return string(data)
}

type gormschemaRunner struct {
	t      *testing.T
	schema string
}

func (r *gormschemaRunner) Run(ctx context.Context, dir string, _ string, args []string, stdout io.Writer, _ io.Writer) error {
	if len(args) > 0 && args[0] == "run" {
		// #nosec G204 -- args are generated by MakeMigrations for the temporary loader.
		cmd := exec.CommandContext(ctx, "go", args...)
		cmd.Dir = dir
		var commandStderr bytes.Buffer
		cmd.Stderr = &commandStderr
		data, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("run generated loader: %w: %s", err, commandStderr.String())
		}
		r.schema = string(data)
		_, _ = stdout.Write(data)
		return nil
	}

	atlasHCL := (&recordingRunner{t: r.t, dir: dir, args: args}).readConfig()
	data, err := os.ReadFile(parseSchemaPath(r.t, atlasHCL))
	if err != nil {
		return fmt.Errorf("read generated schema: %w", err)
	}
	migrationDir := parseMigrationDir(r.t, atlasHCL)
	if err := os.MkdirAll(migrationDir, 0o750); err != nil {
		return err
	}
	// #nosec G703 -- migrationDir is parsed from the Atlas config generated by this test run.
	if err := os.WriteFile(filepath.Join(migrationDir, "20260101000000_create_products.sql"), data, 0o600); err != nil {
		return err
	}
	_, _ = stdout.Write([]byte("created migration\n"))
	return nil
}

func parseSchemaPath(t *testing.T, atlasHCL string) string {
	t.Helper()

	const prefix = `  src = "file://`
	start := strings.Index(atlasHCL, prefix)
	if start < 0 {
		t.Fatalf("atlas.hcl = %q, want schema path", atlasHCL)
	}
	start += len(prefix)
	end := strings.Index(atlasHCL[start:], `"`)
	if end < 0 {
		t.Fatalf("atlas.hcl = %q, want quoted schema path", atlasHCL)
	}
	return filepath.FromSlash(atlasHCL[start : start+end])
}

func parseMigrationDir(t *testing.T, atlasHCL string) string {
	t.Helper()

	const prefix = `    dir = "file://`
	start := strings.Index(atlasHCL, prefix)
	if start < 0 {
		t.Fatalf("atlas.hcl = %q, want migration dir", atlasHCL)
	}
	start += len(prefix)
	end := strings.Index(atlasHCL[start:], `"`)
	if end < 0 {
		t.Fatalf("atlas.hcl = %q, want quoted migration dir", atlasHCL)
	}
	return filepath.FromSlash(atlasHCL[start : start+end])
}

func projectRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	return filepath.Dir(dir)
}
