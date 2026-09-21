package migrations

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gombit-dev/gombit/config"
)

// Rename is a single column rename: OldColumn on Table becomes NewColumn. Gombit
// emits it as a native, data-preserving `ALTER TABLE ... RENAME COLUMN`, which
// SQLite (>= 3.25), PostgreSQL, and MySQL 8 all support.
type Rename struct {
	Table     string
	OldColumn string
	NewColumn string
}

var sqlIdentifierPattern = goIdentifierPattern

// ParseRename parses a rename spec of the form "table.old_column:new_column".
func ParseRename(spec string) (Rename, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return Rename{}, errors.New("migrations: empty rename spec")
	}
	colon := strings.LastIndex(spec, ":")
	if colon <= 0 || colon == len(spec)-1 {
		return Rename{}, fmt.Errorf("migrations: rename %q must be table.old_column:new_column", spec)
	}
	left, newCol := spec[:colon], spec[colon+1:]
	dot := strings.Index(left, ".")
	if dot <= 0 || dot == len(left)-1 {
		return Rename{}, fmt.Errorf("migrations: rename %q must be table.old_column:new_column", spec)
	}
	r := Rename{
		Table:     strings.TrimSpace(left[:dot]),
		OldColumn: strings.TrimSpace(left[dot+1:]),
		NewColumn: strings.TrimSpace(newCol),
	}
	if err := validateRename(r); err != nil {
		return Rename{}, err
	}
	return r, nil
}

func validateRename(r Rename) error {
	for _, part := range []struct {
		what string
		id   string
	}{
		{"table", r.Table},
		{"old column", r.OldColumn},
		{"new column", r.NewColumn},
	} {
		// Identifiers are validated (and never escaped): a table/column name is a
		// GORM snake_case identifier, so anything outside this set is a typo, not a
		// name — and rejecting it keeps the generated SQL injection-free.
		if !sqlIdentifierPattern.MatchString(part.id) {
			return fmt.Errorf("migrations: rename %s %q must be a simple identifier (letters, digits, underscore)", part.what, part.id)
		}
	}
	if r.OldColumn == r.NewColumn {
		return fmt.Errorf("migrations: rename on table %q has the same old and new column %q", r.Table, r.OldColumn)
	}
	return nil
}

// makeRenameMigration writes a native RENAME COLUMN migration and refreshes
// atlas.sum. Because a rename maps the old column to the new one in place, it
// preserves data — unlike the drop+add table rebuild `atlas migrate diff` emits
// for a field rename, which drops the column's data (or fails a NOT NULL apply).
// It reconciles the schema, so a later `gombit db makemigrations` sees the
// directory as synced (#299).
func makeRenameMigration(ctx context.Context, opts Options) error {
	absWorkDir, err := filepath.Abs(opts.WorkDir)
	if err != nil {
		return fmt.Errorf("migrations: resolve work dir: %w", err)
	}
	migrationDir := opts.MigrationDir
	if !filepath.IsAbs(migrationDir) {
		migrationDir = filepath.Join(absWorkDir, migrationDir)
	}
	if err := os.MkdirAll(migrationDir, 0o750); err != nil {
		return fmt.Errorf("migrations: create migration dir: %w", err)
	}

	version, err := nextMigrationVersion(migrationDir)
	if err != nil {
		return err
	}
	sql := renameSQL(opts.Driver, opts.Renames)
	filename := fmt.Sprintf("%s_%s.sql", version, opts.Name)
	if err := os.WriteFile(filepath.Join(migrationDir, filename), []byte(sql), 0o600); err != nil {
		return fmt.Errorf("migrations: write rename migration: %w", err)
	}

	// atlas.sum must include the new file or `atlas migrate apply` refuses it as a
	// checksum mismatch. `atlas migrate hash` is Atlas Community Edition (ADR-012).
	hashArgs := []string{"migrate", "hash", "--dir", "file://" + filepath.ToSlash(migrationDir)}
	if err := opts.runner.Run(ctx, absWorkDir, opts.AtlasBinary, hashArgs, opts.Stderr, opts.Stderr); err != nil {
		return fmt.Errorf("migrations: atlas migrate hash: %w", err)
	}

	_, _ = fmt.Fprintf(opts.Stdout, "Wrote data-preserving rename migration %s\n", filename)
	_, _ = fmt.Fprintln(opts.Stdout, "Review it, then apply with 'gombit db migrate'.")
	return nil
}

// renameSQL renders one ALTER TABLE ... RENAME COLUMN statement per rename,
// with driver-appropriate identifier quoting.
func renameSQL(driver config.DatabaseDriver, renames []Rename) string {
	var b strings.Builder
	for i, r := range renames {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "-- Rename column %q to %q on table %q\n", r.OldColumn, r.NewColumn, r.Table)
		fmt.Fprintf(&b, "ALTER TABLE %s RENAME COLUMN %s TO %s;\n",
			quoteRenameIdent(driver, r.Table),
			quoteRenameIdent(driver, r.OldColumn),
			quoteRenameIdent(driver, r.NewColumn),
		)
	}
	return b.String()
}

func quoteRenameIdent(driver config.DatabaseDriver, id string) string {
	if driver == config.DatabaseDriverPostgres {
		return `"` + id + `"`
	}
	// SQLite and MySQL both accept backtick-quoted identifiers; this matches the
	// style `atlas migrate diff` already writes for those drivers.
	return "`" + id + "`"
}

// nextMigrationVersion returns a 14-digit version that sorts after every existing
// migration in dir. Atlas orders migrations lexically by version, so a rename
// written with a stale/lower timestamp would be replayed before the table it
// renames exists; this guarantees it lands last.
func nextMigrationVersion(dir string) (string, error) {
	files, err := ListMigrationFiles(dir)
	if err != nil {
		return "", err
	}
	latest := ""
	for _, f := range files {
		if f.Version > latest {
			latest = f.Version
		}
	}
	now := time.Now().UTC().Format("20060102150405")
	if now > latest {
		return now, nil
	}
	// Clock is behind the newest existing version (e.g. migrations authored on
	// another machine, or several within the same second): bump past it.
	n, convErr := strconv.ParseInt(latest, 10, 64)
	if convErr != nil {
		return "", fmt.Errorf("migrations: parse latest migration version %q: %w", latest, convErr)
	}
	return fmt.Sprintf("%0*d", len(latest), n+1), nil
}
