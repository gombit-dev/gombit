package migrations

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gombit-dev/gombit/config"
	"gorm.io/gorm"
)

// Rollback rolls back the latest framework_migrations batch using companion down SQL.
// Each down file is split into individual statements (like Seed) and executed
// one Exec per statement, so a down file is no longer required to be a single
// SQL statement on MySQL.
//
// On SQLite and PostgreSQL, down SQL and revision deletes run in one transaction
// so a mid-batch failure leaves neither schema nor revision ledgers partially
// updated. MySQL DDL often auto-commits, so a mid-batch failure can leave the
// schema partially rolled back while revision rows remain — including inside a
// single down file, if an earlier statement in it already committed before a
// later one failed; the error message names the failing statement and file,
// and revision rows are not deleted until every down succeeds.
func Rollback(ctx context.Context, opts ApplyOptions) error {
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

	db, err := openConfiguredDB(opts)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if err := ensureRevisionsTable(db.DB); err != nil {
		return err
	}

	batch, revisions, err := lastBatch(db.DB)
	if err != nil {
		return err
	}
	if batch == 0 || len(revisions) == 0 {
		_, _ = fmt.Fprintln(opts.Stdout, "Nothing to roll back.")
		return nil
	}

	files, skipped, err := listMigrationFiles(migrationDir)
	if err != nil {
		return err
	}
	warnSkippedMigrationFiles(opts.Stderr, skipped)

	byVersion := make(map[string]MigrationFile, len(files))
	for _, file := range files {
		byVersion[file.Version] = file
	}

	downs := make([]MigrationFile, 0, len(revisions))
	missing := make([]string, 0)
	for _, rev := range revisions {
		file, ok := byVersion[rev.Version]
		if !ok || file.DownPath == "" {
			missing = append(missing, filepath.ToSlash(filepath.Join(downSubdir, fmt.Sprintf("%s_%s.down.sql", rev.Version, rev.Name))))
			continue
		}
		downs = append(downs, file)
	}
	if len(missing) > 0 {
		return fmt.Errorf("migrations: missing down migration(s) for batch %d: %s", batch, strings.Join(missing, ", "))
	}

	useTx := opts.Database.Driver != config.DatabaseDriverMySQL
	execDB := db.DB
	var tx *gorm.DB
	if useTx {
		tx = db.Begin()
		if tx.Error != nil {
			return fmt.Errorf("migrations: begin rollback transaction: %w", tx.Error)
		}
		execDB = tx
	}

	versions := make([]string, 0, len(downs))
	for _, file := range downs {
		// #nosec G304 -- down paths are resolved from the configured migration directory.
		sqlBytes, err := os.ReadFile(file.DownPath)
		if err != nil {
			return rollbackFail(tx, useTx, versions, file.DownPath, err, "read")
		}
		sqlText := strings.TrimSpace(string(sqlBytes))
		if sqlText == "" {
			return rollbackFail(tx, useTx, versions, file.DownPath, errors.New("empty down migration"), "execute")
		}
		// Split like Seed does: go-sql-driver/mysql rejects a multi-statement
		// string in one Exec unless the DSN opts in with multiStatements=true,
		// which no gombit-generated or documented MySQL DSN does.
		stmts := splitSQLStatements(sqlText)
		if len(stmts) == 0 {
			return rollbackFail(tx, useTx, versions, file.DownPath, errors.New("down migration has no executable statements"), "execute")
		}
		for i, stmt := range stmts {
			if err := execDB.Exec(stmt).Error; err != nil {
				return rollbackFail(tx, useTx, versions, file.DownPath, err, fmt.Sprintf("execute statement %d of", i+1))
			}
		}
		versions = append(versions, file.Version)
	}

	if err := deleteRevisions(execDB, versions); err != nil {
		return rollbackFail(tx, useTx, versions, "", err, "delete revisions")
	}
	if err := deleteAtlasRevisions(execDB, versions); err != nil {
		return rollbackFail(tx, useTx, versions, "", err, "delete atlas revisions")
	}
	if useTx {
		if err := tx.Commit().Error; err != nil {
			return fmt.Errorf("migrations: commit rollback transaction: %w", err)
		}
	}

	_, _ = fmt.Fprintf(opts.Stdout, "Rolled back batch %d (%d migration(s)).\n", batch, len(versions))
	return nil
}

func rollbackFail(tx *gorm.DB, useTx bool, completed []string, path string, err error, op string) error {
	if useTx && tx != nil {
		_ = tx.Rollback()
	}
	base := path
	if base != "" {
		base = filepath.Base(path)
	}
	msg := fmt.Sprintf("migrations: %s", op)
	if base != "" {
		msg = fmt.Sprintf("migrations: %s %s", op, base)
	}
	if len(completed) > 0 && !useTx {
		return fmt.Errorf("%s after completing downs for %v: %w; framework_migrations unchanged — repair the schema manually before retrying", msg, completed, err)
	}
	if useTx {
		return fmt.Errorf("%s: %w; rollback transaction aborted and framework_migrations unchanged", msg, err)
	}
	return fmt.Errorf("%s: %w; framework_migrations unchanged", msg, err)
}
