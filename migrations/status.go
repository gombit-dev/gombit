package migrations

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/gombit-dev/gombit/config"
)

// Status reports applied and pending migrations, then runs atlas migrate status.
func Status(ctx context.Context, opts ApplyOptions) error {
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
	applied := appliedVersionSet(revisions)
	pending := pendingFiles(files, applied)

	_, _ = fmt.Fprintln(opts.Stdout, "Gombit migration status")
	tw := tabwriter.NewWriter(opts.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "STATE\tVERSION\tNAME\tBATCH\tAPPLIED AT")
	for _, rev := range revisions {
		_, _ = fmt.Fprintf(tw, "applied\t%s\t%s\t%d\t%s\n",
			rev.Version,
			rev.Name,
			rev.Batch,
			rev.AppliedAt.UTC().Format(time.RFC3339),
		)
	}
	for _, file := range pending {
		_, _ = fmt.Fprintf(tw, "pending\t%s\t%s\t-\t-\n", file.Version, file.Name)
	}
	if len(revisions) == 0 && len(pending) == 0 {
		_, _ = fmt.Fprintln(tw, "-\t-\t-\t-\t-")
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("migrations: write status: %w", err)
	}

	absWorkDir, err := filepath.Abs(opts.WorkDir)
	if err != nil {
		return fmt.Errorf("migrations: resolve work dir: %w", err)
	}
	_, _ = fmt.Fprintln(opts.Stdout)
	_, _ = fmt.Fprintln(opts.Stdout, "Atlas migration status")
	args := []string{
		"migrate",
		"status",
		"--url",
		atlasURL,
		"--dir",
		"file://" + filepath.ToSlash(migrationDir),
	}
	args = withAtlasRevisionsSchema(opts.Database.Driver, args)
	hints := newHintWriter(opts.Stderr)
	runErr := opts.runner.Run(ctx, absWorkDir, opts.AtlasBinary, args, opts.Stdout, hints)
	hints.Flush()
	if err := runErr; err != nil {
		return fmt.Errorf("migrations: atlas migrate status: %w", err)
	}
	return nil
}
