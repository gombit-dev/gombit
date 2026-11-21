package migrations

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"gorm.io/gorm"
)

// ApplyOptions configures migrate, rollback, and status commands.
type ApplyOptions struct {
	WorkDir      string
	MigrationDir string
	AtlasBinary  string
	Database     config.DatabaseConfig
	Stdout       io.Writer
	Stderr       io.Writer

	runner commandRunner
	openDB func(config.DatabaseConfig) (*database.DB, error)
	now    func() time.Time
}

func withApplyDefaults(opts ApplyOptions) ApplyOptions {
	if opts.WorkDir == "" {
		opts.WorkDir = "."
	}
	if opts.MigrationDir == "" {
		opts.MigrationDir = defaultMigrationDir
	}
	if opts.AtlasBinary == "" {
		opts.AtlasBinary = defaultAtlasBinary
	}
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}
	if opts.runner == nil {
		opts.runner = execRunner{}
	}
	if opts.openDB == nil {
		opts.openDB = database.Open
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	return opts
}

func resolveMigrationDir(opts ApplyOptions) (string, error) {
	absWorkDir, err := filepath.Abs(opts.WorkDir)
	if err != nil {
		return "", fmt.Errorf("migrations: resolve work dir: %w", err)
	}
	migrationDir := opts.MigrationDir
	if !filepath.IsAbs(migrationDir) {
		migrationDir = filepath.Join(absWorkDir, migrationDir)
	}
	return migrationDir, nil
}

func openConfiguredDB(opts ApplyOptions) (*database.DB, error) {
	db, err := opts.openDB(opts.Database)
	if err != nil {
		return nil, fmt.Errorf("migrations: open database: %w", err)
	}
	return db, nil
}

// Migrate applies pending Atlas migrations and mirrors them into framework_migrations.
//
// After atlas migrate apply succeeds, only pending versions that appear in
// atlas_schema_revisions are recorded. This keeps Gombit's ledger aligned with
// Atlas when the two previously diverged or when Atlas applies a subset.
func Migrate(ctx context.Context, opts ApplyOptions) error {
	if ctx == nil {
		return errors.New("migrations: nil context")
	}
	opts = withApplyDefaults(opts)
	if err := config.ValidateDatabase(opts.Database); err != nil {
		return err
	}

	migrationDir, err := resolveMigrationDir(opts)
	if err != nil {
		return err
	}
	atlasURL, err := AtlasURL(opts.Database)
	if err != nil {
		return err
	}

	db, err := openConfiguredDB(opts)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if err := ensureRevisionsTable(db.DB); err != nil {
		return err
	}

	files, skipped, err := listMigrationFiles(migrationDir)
	if err != nil {
		return err
	}
	warnSkippedMigrationFiles(opts.Stderr, skipped)

	revisions, err := listRevisions(db.DB)
	if err != nil {
		return err
	}
	pending := pendingFiles(files, appliedVersionSet(revisions))
	if len(pending) == 0 {
		_, _ = fmt.Fprintln(opts.Stdout, "No pending migrations.")
		return nil
	}

	absWorkDir, err := filepath.Abs(opts.WorkDir)
	if err != nil {
		return fmt.Errorf("migrations: resolve work dir: %w", err)
	}
	args := []string{
		"migrate",
		"apply",
		"--url",
		atlasURL,
		"--dir",
		"file://" + filepath.ToSlash(migrationDir),
		// framework_migrations (and later app tables) make the DB non-empty before Atlas runs.
		"--allow-dirty",
	}
	args = withAtlasRevisionsSchema(opts.Database.Driver, args)
	hints := newHintWriter(opts.Stderr)
	applyErr := opts.runner.Run(ctx, absWorkDir, opts.AtlasBinary, args, opts.Stdout, hints)
	hints.Flush()
	if applyErr != nil {
		// Atlas commits each file as it goes, so files before the failing one
		// are applied. Record them so the ledger matches the schema.
		applied, _, recErr := recordAtlasApplied(db.DB, pending, opts)
		if recErr != nil {
			return errors.Join(fmt.Errorf("migrations: atlas migrate apply: %w", applyErr), recErr)
		}
		if len(applied) > 0 {
			_, _ = fmt.Fprintf(opts.Stderr, "migrations: recorded %d migration(s) applied before the failure\n", len(applied))
		}
		return fmt.Errorf("migrations: atlas migrate apply: %w", applyErr)
	}

	atlasVersions, err := listAtlasVersions(db.DB)
	if err != nil {
		return err
	}
	if len(pendingPresentInAtlas(pending, atlasVersions)) == 0 {
		return fmt.Errorf("migrations: atlas migrate apply succeeded but none of the %d pending version(s) appear in %s", len(pending), atlasRevisionsTable)
	}
	applied, batch, err := recordAtlasApplied(db.DB, pending, opts)
	if err != nil {
		return err
	}
	if len(applied) < len(pending) {
		_, _ = fmt.Fprintf(opts.Stderr, "migrations: warning: recording %d of %d pending version(s) present in %s\n", len(applied), len(pending), atlasRevisionsTable)
	}
	_, _ = fmt.Fprintf(opts.Stdout, "Applied %d migration(s) in batch %d.\n", len(applied), batch)
	return nil
}

// recordAtlasApplied writes one ledger batch for the pending versions that
// atlas_schema_revisions lists. It returns the recorded files and their batch;
// no files means nothing was recorded.
func recordAtlasApplied(db *gorm.DB, pending []MigrationFile, opts ApplyOptions) ([]MigrationFile, int, error) {
	atlasVersions, err := listAtlasVersions(db)
	if err != nil {
		return nil, 0, err
	}
	applied := pendingPresentInAtlas(pending, atlasVersions)
	if len(applied) == 0 {
		return nil, 0, nil
	}
	batch, err := nextBatch(db)
	if err != nil {
		return nil, 0, err
	}
	if err := insertBatch(db, batch, applied, opts.now().UTC()); err != nil {
		return nil, 0, err
	}
	return applied, batch, nil
}
