package migrations

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/config"
)

// planRunner fakes the go loader and Atlas: the loader prints a fixed schema,
// `schema inspect` returns fixed HCL for the loader's schema file and for the
// migration directory, and `migrate diff` writes a migration file.
type planRunner struct {
	t         *testing.T
	inspected []string
	diffed    bool
}

const (
	fakeDesiredHCL = "# desired\n"
	fakeCurrentHCL = "# current\n"
)

func (r *planRunner) Run(_ context.Context, _ string, name string, args []string, stdout io.Writer, _ io.Writer) error {
	if name == "go" {
		_, _ = io.WriteString(stdout, "-- fake gorm schema\n")
		return nil
	}
	if len(args) >= 4 && args[0] == "schema" && args[1] == "inspect" {
		url := args[3]
		r.inspected = append(r.inspected, url)
		if strings.HasSuffix(url, "schema.sql") {
			_, _ = io.WriteString(stdout, fakeDesiredHCL)
		} else {
			_, _ = io.WriteString(stdout, fakeCurrentHCL)
		}
		return nil
	}
	if len(args) >= 3 && args[0] == "migrate" && args[1] == "diff" {
		r.diffed = true
		atlasHCL := (&recordingRunner{t: r.t, args: args}).readConfig()
		dir := parseMigrationDir(r.t, atlasHCL)
		return os.WriteFile(filepath.Join(dir, "20260102000000_"+args[2]+".sql"), []byte("-- fake\n"), 0o600)
	}
	return errors.New("planRunner: unexpected command " + name + " " + strings.Join(args, " "))
}

func seedMigrationDir(t *testing.T, dir string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "20260101000000_init.sql"), "-- init\n")
	if err := SaveRegistry(dir, []Model{{ImportPath: "example.com/app/internal/product", TypeName: "Product"}}); err != nil {
		t.Fatal(err)
	}
}

func TestMakeMigrationsGateRefusalWritesNothing(t *testing.T) {
	migrationDir := t.TempDir()
	seedMigrationDir(t, migrationDir)
	runner := &planRunner{t: t}
	forget := []Model{{ImportPath: "example.com/app/internal/product", TypeName: "Product"}}
	var got Inspection
	var gotName string
	refusal := errors.New("refused")
	err := MakeMigrations(context.Background(), Options{
		WorkDir:      t.TempDir(),
		Name:         "reshape_products",
		Driver:       config.DatabaseDriverSQLite,
		MigrationDir: migrationDir,
		AtlasBinary:  "atlas-test",
		Models:       []Model{{ImportPath: "example.com/app/internal/order", TypeName: "Order"}},
		ForgetModels: forget,
		Gate: func(_ context.Context, name string, in Inspection) ([]string, error) {
			gotName, got = name, in
			return nil, refusal
		},
		Stdout: io.Discard,
		Stderr: io.Discard,
		runner: runner,
	})
	if !errors.Is(err, refusal) {
		t.Fatalf("MakeMigrations() error = %v, want the gate's refusal", err)
	}
	if runner.diffed {
		t.Fatal("atlas migrate diff ran after the gate refused")
	}
	if gotName != "reshape_products" || string(got.Current) != fakeCurrentHCL || string(got.Desired) != fakeDesiredHCL || got.Driver != config.DatabaseDriverSQLite {
		t.Fatalf("gate got name %q and %+v, want both inspected states", gotName, got)
	}
	if len(got.ForgetModels) != 1 || got.ForgetModels[0] != forget[0] {
		t.Fatalf("gate got ForgetModels %v, want %v", got.ForgetModels, forget)
	}
	// Order is new; Product is already registered, so it is not repeated.
	if len(got.NewModels) != 1 || got.NewModels[0].TypeName != "Order" {
		t.Fatalf("gate got NewModels %v, want only the unregistered Order", got.NewModels)
	}
	files, _ := filepath.Glob(filepath.Join(migrationDir, "*.sql"))
	if len(files) != 1 {
		t.Fatalf("migration files = %v, want only the seeded one", files)
	}
	registered, err := LoadRegistry(migrationDir)
	if err != nil || len(registered) != 1 || registered[0].TypeName != "Product" {
		t.Fatalf("registry = %v (%v), want it unchanged after a refusal", registered, err)
	}
}

func TestMakeMigrationsGatePassWritesMigration(t *testing.T) {
	migrationDir := t.TempDir()
	seedMigrationDir(t, migrationDir)
	runner := &planRunner{t: t}
	err := MakeMigrations(context.Background(), Options{
		WorkDir:      t.TempDir(),
		Name:         "add_orders",
		Driver:       config.DatabaseDriverSQLite,
		MigrationDir: migrationDir,
		AtlasBinary:  "atlas-test",
		Gate:         func(context.Context, string, Inspection) ([]string, error) { return nil, nil },
		Stdout:       io.Discard,
		Stderr:       io.Discard,
		runner:       runner,
	})
	if err != nil {
		t.Fatalf("MakeMigrations() error = %v", err)
	}
	if !runner.diffed || len(runner.inspected) != 2 {
		t.Fatalf("diffed = %v, inspected = %v; want the gate's two inspections, then the diff", runner.diffed, runner.inspected)
	}
}

func TestMakeMigrationsGateSkippedBeforeFirstMigrationAndWhenNil(t *testing.T) {
	cases := []struct {
		name string
		seed bool
		gate Gate
	}{
		{name: "first migration", seed: false, gate: func(context.Context, string, Inspection) ([]string, error) {
			return nil, errors.New("gate must not run")
		}},
		{name: "nil gate", seed: true, gate: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			migrationDir := t.TempDir()
			if tc.seed {
				seedMigrationDir(t, migrationDir)
			}
			runner := &planRunner{t: t}
			err := MakeMigrations(context.Background(), Options{
				WorkDir:      t.TempDir(),
				Name:         "next",
				Driver:       config.DatabaseDriverSQLite,
				MigrationDir: migrationDir,
				AtlasBinary:  "atlas-test",
				Models:       []Model{{ImportPath: "example.com/app/internal/product", TypeName: "Product"}},
				Gate:         tc.gate,
				Stdout:       io.Discard,
				Stderr:       io.Discard,
				runner:       runner,
			})
			if err != nil {
				t.Fatalf("MakeMigrations() error = %v", err)
			}
			if len(runner.inspected) != 0 {
				t.Fatalf("inspected %v, want no inspection", runner.inspected)
			}
		})
	}
}

func TestInspectWithoutWriting(t *testing.T) {
	migrationDir := t.TempDir()
	seedMigrationDir(t, migrationDir)
	runner := &planRunner{t: t}
	in, err := Inspect(context.Background(), InspectOptions{
		WorkDir:      t.TempDir(),
		Driver:       config.DatabaseDriverSQLite,
		MigrationDir: migrationDir,
		AtlasBinary:  "atlas-test",
		runner:       runner,
	})
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if runner.diffed {
		t.Fatal("Inspect ran atlas migrate diff")
	}
	if string(in.Current) != fakeCurrentHCL || string(in.Desired) != fakeDesiredHCL {
		t.Fatalf("Inspect() = %+v, want both states", in)
	}
}

func TestInspectEmptyMigrationDirHasNoCurrentState(t *testing.T) {
	runner := &planRunner{t: t}
	missing := filepath.Join(t.TempDir(), "missing")
	in, err := Inspect(context.Background(), InspectOptions{
		WorkDir:      t.TempDir(),
		Driver:       config.DatabaseDriverSQLite,
		MigrationDir: missing,
		AtlasBinary:  "atlas-test",
		Models:       []Model{{ImportPath: "example.com/app/internal/product", TypeName: "Product"}},
		runner:       runner,
	})
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if len(in.Current) != 0 || len(runner.inspected) != 1 {
		t.Fatalf("Inspect() current = %q after inspecting %v, want only the models' schema", in.Current, runner.inspected)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("Inspect created the migration directory: %v", err)
	}
}

// TestLoaderSidePackagesStayOffAtlasSQL guards the split between this package
// and migrations/schemaplan: generated apps compile migrations (through
// resourcegen and generate) in read-only module mode, and their go.sum does
// not carry the Atlas SQL libraries.
func TestLoaderSidePackagesStayOffAtlasSQL(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/gombit-dev/gombit/migrations", "github.com/gombit-dev/gombit/resourcegen", "github.com/gombit-dev/gombit/generate").Output() // #nosec G204 -- fixed arguments
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	for _, pkg := range strings.Fields(string(out)) {
		if strings.HasPrefix(pkg, "ariga.io/atlas/sql/") || strings.HasPrefix(pkg, "github.com/gombit-dev/gombit/migrations/schemaplan") {
			t.Fatalf("%s is a dependency of migrations/resourcegen/generate; keep it in migrations/schemaplan", pkg)
		}
	}
}
