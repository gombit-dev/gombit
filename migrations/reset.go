package migrations

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
)

// ResetOptions configures gombit db reset (drop + migrate + seed).
type ResetOptions struct {
	ApplyOptions
	SeedDir     string
	Force       bool
	Environment config.Environment
}

// DropSchema removes all user tables and views from the configured database.
//
// Driver wipe strategy:
//   - SQLite: DROP every non-sqlite_* table/view from sqlite_master
//   - Postgres: DROP SCHEMA atlas_schema_revisions CASCADE (Atlas default);
//     DROP SCHEMA public CASCADE; CREATE SCHEMA public; restore grants
//   - MySQL: disable FK checks, DROP every base table and view, re-enable checks
//
// The SQLite database file is not deleted so in-memory and shared-cache DSNs work.
func DropSchema(ctx context.Context, opts ApplyOptions) error {
	if ctx == nil {
		return errors.New("migrations: nil context")
	}
	opts = withApplyDefaults(opts)
	if err := config.ValidateDatabase(opts.Database); err != nil {
		return err
	}

	db, err := openConfiguredDB(opts)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	switch opts.Database.Driver {
	case config.DatabaseDriverSQLite:
		if err := dropSQLiteSchema(db); err != nil {
			return err
		}
	case config.DatabaseDriverPostgres:
		if err := dropPostgresSchema(db); err != nil {
			return err
		}
	case config.DatabaseDriverMySQL:
		if err := dropMySQLSchema(db); err != nil {
			return err
		}
	default:
		return fmt.Errorf("migrations: unsupported driver %q", opts.Database.Driver)
	}
	_, _ = fmt.Fprintln(opts.Stdout, "Dropped database schema.")
	return nil
}

// Reset drops the database schema, applies migrations, then runs seeders.
//
// When Environment is production, Force must be true.
func Reset(ctx context.Context, opts ResetOptions) error {
	if ctx == nil {
		return errors.New("migrations: nil context")
	}
	if opts.Environment == config.EnvironmentProduction && !opts.Force {
		return errors.New("migrations: refuse reset in production without --force")
	}

	applyOpts := withApplyDefaults(opts.ApplyOptions)
	if err := DropSchema(ctx, applyOpts); err != nil {
		return err
	}
	if err := Migrate(ctx, applyOpts); err != nil {
		return err
	}
	return Seed(ctx, SeedOptions{
		WorkDir:  applyOpts.WorkDir,
		SeedDir:  opts.SeedDir,
		Database: applyOpts.Database,
		Stdout:   applyOpts.Stdout,
		Stderr:   applyOpts.Stderr,
		openDB:   applyOpts.openDB,
	})
}

func dropSQLiteSchema(db *database.DB) error {
	if err := db.Exec("PRAGMA foreign_keys = OFF").Error; err != nil {
		return fmt.Errorf("migrations: disable sqlite foreign keys: %w", err)
	}
	type object struct {
		Type string
		Name string
	}
	var objects []object
	if err := db.Raw(`
SELECT type, name FROM sqlite_master
WHERE type IN ('table', 'view')
  AND name NOT LIKE 'sqlite\_%' ESCAPE '\'
ORDER BY type DESC, name`).Scan(&objects).Error; err != nil {
		return fmt.Errorf("migrations: list sqlite objects: %w", err)
	}
	for _, obj := range objects {
		stmt := fmt.Sprintf("DROP %s IF EXISTS %s", strings.ToUpper(obj.Type), quoteIdent(obj.Name))
		if err := db.Exec(stmt).Error; err != nil {
			return fmt.Errorf("migrations: drop sqlite %s %s: %w", obj.Type, obj.Name, err)
		}
	}
	if err := db.Exec("PRAGMA foreign_keys = ON").Error; err != nil {
		return fmt.Errorf("migrations: enable sqlite foreign keys: %w", err)
	}
	return nil
}

func dropPostgresSchema(db *database.DB) error {
	stmts := []string{
		// Atlas's historical default revisions schema (pre --revisions-schema public).
		"DROP SCHEMA IF EXISTS atlas_schema_revisions CASCADE",
		"DROP SCHEMA IF EXISTS public CASCADE",
		"CREATE SCHEMA public",
		"GRANT ALL ON SCHEMA public TO public",
		"GRANT ALL ON SCHEMA public TO CURRENT_USER",
	}
	for _, stmt := range stmts {
		if err := db.Exec(stmt).Error; err != nil {
			return fmt.Errorf("migrations: postgres schema wipe (%s): %w", stmt, err)
		}
	}
	return nil
}

func dropMySQLSchema(db *database.DB) error {
	if err := db.Exec("SET FOREIGN_KEY_CHECKS = 0").Error; err != nil {
		return fmt.Errorf("migrations: disable mysql foreign keys: %w", err)
	}

	var dbName string
	if err := db.Raw("SELECT DATABASE()").Scan(&dbName).Error; err != nil {
		return fmt.Errorf("migrations: resolve mysql database: %w", err)
	}
	if dbName == "" {
		return errors.New("migrations: mysql DATABASE() is empty")
	}

	type object struct {
		Name string
		Type string
	}
	var objects []object
	if err := db.Raw(`
SELECT table_name AS name, table_type AS type
FROM information_schema.tables
WHERE table_schema = ?
ORDER BY table_type DESC, table_name`, dbName).Scan(&objects).Error; err != nil {
		return fmt.Errorf("migrations: list mysql objects: %w", err)
	}
	for _, obj := range objects {
		kind := "TABLE"
		if strings.EqualFold(obj.Type, "VIEW") {
			kind = "VIEW"
		}
		stmt := fmt.Sprintf("DROP %s IF EXISTS `%s`", kind, strings.ReplaceAll(obj.Name, "`", "``"))
		if err := db.Exec(stmt).Error; err != nil {
			return fmt.Errorf("migrations: drop mysql %s %s: %w", kind, obj.Name, err)
		}
	}

	if err := db.Exec("SET FOREIGN_KEY_CHECKS = 1").Error; err != nil {
		return fmt.Errorf("migrations: enable mysql foreign keys: %w", err)
	}
	return nil
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
