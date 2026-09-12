package migrations

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gombit-dev/gombit/config"
)

// Hash recomputes the Atlas migration directory's checksum file (atlas.sum) by
// wrapping `atlas migrate hash`. After a migration is hand-edited — e.g. to
// backfill a renamed column so a NOT NULL rebuild can apply — Atlas refuses to
// run it with a checksum error. This lets a developer recover with
// `gombit db hash` instead of reaching for the raw Atlas CLI (#219). It does not
// touch the application database.
func Hash(ctx context.Context, opts ApplyOptions) error {
	if ctx == nil {
		return errors.New("migrations: nil context")
	}
	opts = withApplyDefaults(opts)
	migrationDir, err := resolveMigrationDir(opts)
	if err != nil {
		return err
	}
	absWorkDir, err := filepath.Abs(opts.WorkDir)
	if err != nil {
		return fmt.Errorf("migrations: resolve work dir: %w", err)
	}
	args := []string{"migrate", "hash", "--dir", "file://" + filepath.ToSlash(migrationDir)}
	if err := opts.runner.Run(ctx, absWorkDir, opts.AtlasBinary, args, opts.Stdout, opts.Stderr); err != nil {
		return fmt.Errorf("migrations: atlas migrate hash: %w", err)
	}
	_, _ = fmt.Fprintln(opts.Stdout, "Rehashed the migration directory (atlas.sum).")
	return nil
}

// LintOptions configures Lint. DevURL is the Atlas dev database (a scratch
// database Atlas replays migrations into to analyze them); it defaults to an
// in-memory SQLite dev-url for the sqlite driver and is required for postgres /
// mysql. Latest bounds how many of the most recent migrations are analyzed
// (default 1 — the migration just generated).
type LintOptions struct {
	ApplyOptions
	DevURL string
	Latest int
}

// Lint analyzes recent migrations for destructive or non-appliable changes by
// wrapping `atlas migrate lint` — so a data-losing column drop or a NOT NULL
// rebuild that cannot apply to a non-empty table is surfaced before it reaches a
// real database, instead of failing mid-apply with no warning (#219).
func Lint(ctx context.Context, opts LintOptions) error {
	if ctx == nil {
		return errors.New("migrations: nil context")
	}
	opts.ApplyOptions = withApplyDefaults(opts.ApplyOptions)
	if opts.Latest <= 0 {
		opts.Latest = 1
	}
	devURL := strings.TrimSpace(opts.DevURL)
	if devURL == "" {
		var err error
		devURL, err = defaultDevURL(opts.Database.Driver)
		if err != nil {
			return err
		}
	}
	migrationDir, err := resolveMigrationDir(opts.ApplyOptions)
	if err != nil {
		return err
	}
	absWorkDir, err := filepath.Abs(opts.WorkDir)
	if err != nil {
		return fmt.Errorf("migrations: resolve work dir: %w", err)
	}
	args := []string{
		"migrate", "lint",
		"--dev-url", devURL,
		"--dir", "file://" + filepath.ToSlash(migrationDir),
		"--latest", strconv.Itoa(opts.Latest),
	}
	if err := opts.runner.Run(ctx, absWorkDir, opts.AtlasBinary, args, opts.Stdout, opts.Stderr); err != nil {
		return fmt.Errorf("migrations: atlas migrate lint: %w", err)
	}
	return nil
}

// defaultDevURL returns an Atlas dev-url for drivers where a throwaway scratch
// database is free (in-memory SQLite). Postgres and MySQL need a real dev
// database, so the caller must pass one explicitly (--dev-url).
func defaultDevURL(driver config.DatabaseDriver) (string, error) {
	switch driver {
	case config.DatabaseDriverSQLite:
		return "sqlite://file?mode=memory", nil
	case config.DatabaseDriverPostgres:
		return "", fmt.Errorf("migrations: lint needs a dev database for postgres; pass --dev-url (e.g. docker://postgres/16/dev?search_path=public)")
	case config.DatabaseDriverMySQL:
		return "", fmt.Errorf("migrations: lint needs a dev database for mysql; pass --dev-url (e.g. docker://mysql/8/dev)")
	default:
		return "", fmt.Errorf("migrations: lint needs --dev-url for driver %q", driver)
	}
}
