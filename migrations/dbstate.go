package migrations

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
)

// BookkeepingTables are the tables Gombit and Atlas keep in the application
// database to record applied migrations. They are not part of the schema the
// migrations build, so a schema comparison ignores them.
var BookkeepingTables = []string{revisionsTable, atlasRevisionsTable}

// DatabaseState is what `gombit db check` reads from the application
// database: which migrations it has applied and the schema it actually has.
type DatabaseState struct {
	// Pending are the up migrations the database has not applied, in order.
	Pending []MigrationFile
	// LastApplied is the newest applied version with a file in the
	// directory, or "" when none is.
	LastApplied string
	// Unknown are versions the database records as applied that have no
	// file in the migration directory (a migration renamed or deleted after
	// it ran).
	Unknown []string
	// Schema is the HCL `atlas schema inspect` prints for the live database.
	Schema []byte
}

// InspectDatabase reads the application database without changing it: the
// applied versions from framework_migrations (a missing table means none), the
// migrations still pending, and the live schema.
func InspectDatabase(ctx context.Context, opts ApplyOptions) (DatabaseState, error) {
	if ctx == nil {
		return DatabaseState{}, errors.New("migrations: nil context")
	}
	opts = withApplyDefaults(opts)
	migrationDir, err := resolveMigrationDir(opts)
	if err != nil {
		return DatabaseState{}, err
	}
	atlasURL, err := AtlasURL(opts.Database)
	if err != nil {
		return DatabaseState{}, err
	}
	db, err := openConfiguredDB(opts)
	if err != nil {
		return DatabaseState{}, err
	}
	defer func() { _ = db.Close() }()

	files, err := ListMigrationFiles(migrationDir)
	if err != nil {
		return DatabaseState{}, err
	}
	var revisions []Revision
	if db.Migrator().HasTable(&Revision{}) {
		if revisions, err = listRevisions(db.DB); err != nil {
			return DatabaseState{}, err
		}
	}
	applied := appliedVersionSet(revisions)
	state := DatabaseState{Pending: pendingFiles(files, applied)}
	inDir := make(map[string]bool, len(files))
	for _, f := range files {
		inDir[f.Version] = true
		if _, ok := applied[f.Version]; ok {
			state.LastApplied = f.Version
		}
	}
	for _, rev := range revisions {
		if !inDir[rev.Version] {
			state.Unknown = append(state.Unknown, rev.Version)
		}
	}

	absWorkDir, err := filepath.Abs(opts.WorkDir)
	if err != nil {
		return DatabaseState{}, fmt.Errorf("migrations: resolve work dir: %w", err)
	}
	var out, errOut bytes.Buffer
	args := []string{"schema", "inspect", "--url", atlasURL}
	if err := opts.runner.Run(ctx, absWorkDir, opts.AtlasBinary, args, &out, &errOut); err != nil {
		if msg := atlasMessage(errOut.String()); msg != "" {
			return DatabaseState{}, fmt.Errorf("migrations: inspect the database: %w: %s", err, msg)
		}
		return DatabaseState{}, fmt.Errorf("migrations: inspect the database: %w", err)
	}
	state.Schema = out.Bytes()
	return state, nil
}
