package scaffold

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/migrations"
)

// bootstrapModels lists the GORM models internal/platform/database.go.tmpl
// registers for AutoMigrate: the framework's own auth and storage-claims
// tables plus the product/ example, in the same order the template writes
// them.
//
// AutoMigrate runs at every app startup (app.OnStart), creating these tables
// directly through GORM. Without a migration on disk for them from the
// start, Atlas's tracked history never learns they exist, so the first later
// diff that names a model not yet in the registry "discovers" them as
// missing too and tries to CREATE tables that are already there — and
// applying that migration fails with "table already exists" (#96). Seeding
// this migration, and the model registry it writes (see #97's
// migrations.MakeMigrations / database/migrations/models.json), at scaffold
// time keeps Atlas's history in sync with what AutoMigrate creates from the
// very first run: a later `gombit db makemigrations create_x --model X`
// (chapter 3's own pattern) merges with this registry instead of treating
// its single model as the entire desired schema.
func bootstrapModels(module string) []migrations.Model {
	const authImport = "github.com/gombit-dev/gombit/auth"
	return []migrations.Model{
		{ImportPath: authImport, TypeName: "User"},
		{ImportPath: authImport, TypeName: "RefreshToken"},
		{ImportPath: authImport, TypeName: "Group"},
		{ImportPath: authImport, TypeName: "Permission"},
		{ImportPath: "github.com/gombit-dev/gombit/storage/claims", TypeName: "Claim"},
		{ImportPath: module + "/internal/product", TypeName: "Product"},
	}
}

// seedBootstrapMigration writes the initial database/migrations/ entry
// covering bootstrapModels and, for SQLite, applies it immediately. It
// requires a module that already resolves (go mod tidy succeeded) and the
// atlas binary. Any failure to seed it — atlas missing, or atlas present but
// unable to run (e.g. --database postgres/mysql need a running Docker
// daemon for Atlas's dev-database, and gombit new never required Docker
// before) — prints the equivalent gombit db makemigrations command instead
// of failing gombit new outright. Bootstrapping the migration is an
// automation on top of an otherwise complete, working scaffold, not a
// precondition for one — the same reasoning behind resourcegen's own
// atlas-missing fallback, extended here to cover atlas errors too since
// this runs unprompted, not because the user explicitly asked to generate a
// migration.
//
// Applying it matters as much as writing it: AutoMigrate runs unconditionally
// at every app startup (app.OnStart), including the very first `gombit dev`
// — before the reader ever reaches chapter 3's own `gombit db migrate`.
// Leaving the migration merely written (not applied) means AutoMigrate
// creates these tables live the moment `gombit dev` first runs, and applying
// the migration afterward fails with "table already exists" (#102) — the
// same symptom #96 described, just surfacing one step earlier. Applying it
// now means AutoMigrate's later run is a safe no-op against tables that
// already have the correct shape.
//
// Only SQLite is applied automatically: postgres/mysql get a placeholder DSN
// (USER:PASSWORD@...) from gombit new until the reader edits .env with real
// credentials, so there is nothing reachable to apply against yet — the
// reader runs gombit db migrate themselves once that's configured, same as
// they always have.
func seedBootstrapMigration(ctx context.Context, opts Options) error {
	models := bootstrapModels(opts.Module)

	atlasPath, lookErr := scaffoldLookPath("atlas")
	if lookErr != nil || atlasPath == "" {
		return printMakeMigrationsHint(opts, models, "atlas not on PATH")
	}

	driver := config.DatabaseDriver(opts.Database)
	err := scaffoldMakeMigrations(ctx, migrations.Options{
		WorkDir:      opts.Dest,
		Name:         "bootstrap",
		Driver:       driver,
		MigrationDir: "database/migrations",
		Models:       models,
		Stdout:       opts.Stdout,
		Stderr:       opts.Stderr,
	})
	if err != nil {
		return printMakeMigrationsHint(opts, models, fmt.Sprintf("could not seed it (%v)", err))
	}

	if driver != config.DatabaseDriverSQLite {
		return printMigrateHint(opts, "it needs "+string(driver)+" credentials in .env before it can be applied")
	}

	err = scaffoldMigrate(ctx, migrations.ApplyOptions{
		WorkDir:      opts.Dest,
		MigrationDir: "database/migrations",
		Database: config.DatabaseConfig{
			Driver: driver,
			// defaultDSN's sqlite path is relative, meant to be resolved
			// against the running app's own cwd. This runs from gombit's
			// own process, so make it absolute instead of chdir-ing.
			DSN: "file:" + filepath.Join(opts.Dest, "gombit.db") + "?cache=shared&_fk=1",
		},
		Stdout: opts.Stdout,
		Stderr: opts.Stderr,
	})
	if err != nil {
		return printMigrateHint(opts, fmt.Sprintf("seeded it but could not apply it (%v)", err))
	}
	return nil
}

func printMakeMigrationsHint(opts Options, models []migrations.Model, reason string) error {
	var args string
	for _, model := range models {
		args += " --model " + model.ImportPath + "." + model.TypeName
	}
	_, err := fmt.Fprintf(
		opts.Stdout,
		"note: %s; run this once before gombit db migrate so Atlas's history matches "+
			"what AutoMigrate creates at startup:\n  gombit db makemigrations bootstrap%s\n",
		reason, args,
	)
	return err
}

func printMigrateHint(opts Options, reason string) error {
	_, err := fmt.Fprintf(opts.Stdout, "note: %s; run gombit db migrate once it does\n", reason)
	return err
}

// scaffoldLookPath, scaffoldMakeMigrations, and scaffoldMigrate are
// indirected for tests, the same pattern resourcegen uses for its own
// atlas/makemigrations seams.
var scaffoldLookPath = func(name string) (string, error) {
	return exec.LookPath(name)
}

var scaffoldMakeMigrations = migrations.MakeMigrations

var scaffoldMigrate = migrations.Migrate
