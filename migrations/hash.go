package migrations

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
)

// Hash recomputes the Atlas migration directory's checksum file (atlas.sum) by
// wrapping `atlas migrate hash`. After a migration is hand-edited — e.g. to
// backfill a renamed column so a NOT NULL rebuild can apply — Atlas refuses to
// run it with a checksum error. This lets a developer recover with
// `gombit db hash` instead of reaching for the raw Atlas CLI (#219). It does not
// touch the application database, and `atlas migrate hash` is part of Atlas
// Community Edition (ADR-012).
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
