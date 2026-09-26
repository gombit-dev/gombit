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

// TableRename renames table Old to New with a native, data-preserving
// `ALTER TABLE ... RENAME TO`, which SQLite, PostgreSQL, and MySQL all
// support. Foreign keys in other tables follow the table on all three.
type TableRename struct {
	Old string
	New string
}

var sqlIdentifierPattern = goIdentifierPattern

// ParseRename parses a rename spec of the form "table.old_column:new_column",
// or "table.old_column=table.new_column".
func ParseRename(spec string) (Rename, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return Rename{}, errors.New("migrations: empty rename spec")
	}
	if left, right, ok := strings.Cut(spec, "="); ok {
		oldTable, oldCol, ok1 := strings.Cut(strings.TrimSpace(left), ".")
		newTable, newCol, ok2 := strings.Cut(strings.TrimSpace(right), ".")
		if !ok1 || !ok2 {
			return Rename{}, fmt.Errorf("migrations: rename %q must be table.old_column:new_column or table.old_column=table.new_column", spec)
		}
		if oldTable != newTable {
			return Rename{}, fmt.Errorf("migrations: rename %q changes the table; rename the table with --rename-table %s:%s", spec, oldTable, newTable)
		}
		spec = oldTable + "." + oldCol + ":" + newCol
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

// ParseTableRename parses a table rename spec of the form "old_table:new_table".
func ParseTableRename(spec string) (TableRename, error) {
	oldName, newName, ok := strings.Cut(strings.TrimSpace(spec), ":")
	if !ok {
		return TableRename{}, fmt.Errorf("migrations: table rename %q must be old_table:new_table", spec)
	}
	r := TableRename{Old: strings.TrimSpace(oldName), New: strings.TrimSpace(newName)}
	if err := validateTableRename(r); err != nil {
		return TableRename{}, err
	}
	return r, nil
}

func validateTableRename(r TableRename) error {
	for _, id := range []string{r.Old, r.New} {
		if !sqlIdentifierPattern.MatchString(id) {
			return fmt.Errorf("migrations: table rename %q must be a simple identifier (letters, digits, underscore)", id)
		}
	}
	if r.Old == r.New {
		return fmt.Errorf("migrations: table rename has the same old and new name %q", r.Old)
	}
	return nil
}

// validateRenameSet checks a rename run as a whole. Table renames apply
// first, so a column rename names the table's new name.
func validateRenameSet(tables []TableRename, columns []Rename) error {
	oldNames, newNames := map[string]bool{}, map[string]bool{}
	for _, t := range tables {
		if err := validateTableRename(t); err != nil {
			return err
		}
		if oldNames[t.Old] {
			return fmt.Errorf("migrations: table %q is renamed twice", t.Old)
		}
		if newNames[t.New] {
			return fmt.Errorf("migrations: two tables are renamed to %q", t.New)
		}
		oldNames[t.Old], newNames[t.New] = true, true
	}
	// A chain (a:b with b:c) or a swap depends on statement order and fails
	// while the target name is still taken; write it as separate migrations.
	for _, t := range tables {
		if oldNames[t.New] {
			return fmt.Errorf("migrations: table rename %s:%s renames onto a table this run also renames; split chained or swapped renames into separate migrations", t.Old, t.New)
		}
	}
	for _, c := range columns {
		if err := validateRename(c); err != nil {
			return err
		}
		if oldNames[c.Table] && !newNames[c.Table] {
			return fmt.Errorf("migrations: column rename %s.%s names table %q, which this run renames; table renames apply first, so use its new name", c.Table, c.OldColumn, c.Table)
		}
	}
	return nil
}

// makeRenameMigration writes a native RENAME TABLE / RENAME COLUMN migration
// and refreshes atlas.sum. Because a rename maps the old column to the new one in place, it
// preserves data — unlike the drop+add table rebuild `atlas migrate diff` emits
// for a field rename, which drops the column's data (or fails a NOT NULL apply).
// It reconciles the schema, so a later `gombit db makemigrations` sees the
// directory as synced (#299). A table rename may also name the models it
// swaps in the registry (--model / --forget-model); the registry is written
// only once the migration is hashed (#310).
func makeRenameMigration(ctx context.Context, opts Options) error {
	absWorkDir, err := filepath.Abs(opts.WorkDir)
	if err != nil {
		return fmt.Errorf("migrations: resolve work dir: %w", err)
	}
	migrationDir := opts.MigrationDir
	if !filepath.IsAbs(migrationDir) {
		migrationDir = filepath.Join(absWorkDir, migrationDir)
	}
	_, statErr := os.Stat(migrationDir)
	dirExisted := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("migrations: inspect migration dir: %w", statErr)
	}
	if err := os.MkdirAll(migrationDir, 0o750); err != nil {
		return fmt.Errorf("migrations: create migration dir: %w", err)
	}

	// The registry swap a model rename needs, checked before anything is
	// written.
	var registry []Model
	swapModels := len(opts.Models) > 0 || len(opts.ForgetModels) > 0
	if swapModels {
		registered, err := LoadRegistry(migrationDir)
		if err != nil {
			return err
		}
		known := MergeModels(registered, opts.Models)
		if err := ensureForgetModelsTracked(known, opts.ForgetModels); err != nil {
			return err
		}
		registry = SubtractModels(known, opts.ForgetModels)
	}
	registryPath := RegistryPath(migrationDir)
	// #nosec G304 -- models.json inside the configured migration directory
	prevRegistry, regErr := os.ReadFile(registryPath)
	registryExisted := regErr == nil
	if regErr != nil && !errors.Is(regErr, os.ErrNotExist) {
		return fmt.Errorf("migrations: read model registry: %w", regErr)
	}

	version, err := nextMigrationVersion(migrationDir)
	if err != nil {
		return err
	}
	sumPath := filepath.Join(migrationDir, "atlas.sum")
	// #nosec G304 -- atlas.sum inside the configured migration directory
	prevSum, sumErr := os.ReadFile(sumPath)
	sumExisted := sumErr == nil
	if sumErr != nil && !errors.Is(sumErr, os.ErrNotExist) {
		return fmt.Errorf("migrations: read atlas.sum: %w", sumErr)
	}

	sql := renameSQL(opts.Driver, opts.TableRenames, opts.Renames)
	filename := fmt.Sprintf("%s_%s.sql", version, opts.Name)
	migrationPath := filepath.Join(migrationDir, filename)
	if err := os.WriteFile(migrationPath, []byte(sql), 0o600); err != nil {
		return fmt.Errorf("migrations: write rename migration: %w", err)
	}

	// atlas.sum must include the new file or `atlas migrate apply` refuses it as a
	// checksum mismatch. `atlas migrate hash` is Atlas Community Edition (ADR-012).
	hashArgs := []string{"migrate", "hash", "--dir", "file://" + filepath.ToSlash(migrationDir)}
	hints := newHintWriter(opts.Stderr)
	hashErr := opts.runner.Run(ctx, absWorkDir, opts.AtlasBinary, hashArgs, hints, hints)
	hints.Flush()
	if err := hashErr; err != nil {
		// Unlike `atlas migrate diff`, which writes the file and the sum together,
		// this path writes the SQL itself. Leaving it unhashed would make the
		// directory unappliable, and rerunning after installing Atlas would add a
		// second RENAME COLUMN for the same column. Put the directory back exactly
		// as it was so a rerun starts clean.
		rollbackRenameMigration(migrationDir, migrationPath, sumPath, prevSum, sumExisted, dirExisted)
		return fmt.Errorf("migrations: atlas migrate hash: %w", err)
	}
	// The exact inverse, so `gombit db rollback` can undo the rename. downs/
	// is outside what Atlas hashes, so it is written after the hash.
	downPath := DownPath(migrationDir, version, opts.Name)
	_, downDirErr := os.Stat(filepath.Dir(downPath))
	downDirExisted := downDirErr == nil
	undo := func() {
		_ = os.Remove(downPath)
		if !downDirExisted {
			_ = os.Remove(filepath.Dir(downPath))
		}
		restoreFile(registryPath, prevRegistry, registryExisted)
		rollbackRenameMigration(migrationDir, migrationPath, sumPath, prevSum, sumExisted, dirExisted)
	}
	if err := os.MkdirAll(filepath.Dir(downPath), 0o750); err != nil {
		undo()
		return fmt.Errorf("migrations: create downs dir: %w", err)
	}
	if err := os.WriteFile(downPath, []byte(renameDownSQL(opts.Driver, opts.TableRenames, opts.Renames)), 0o600); err != nil {
		undo()
		return fmt.Errorf("migrations: write rename down migration: %w", err)
	}
	if swapModels {
		if err := SaveRegistry(migrationDir, registry); err != nil {
			undo()
			return err
		}
	}

	_, _ = fmt.Fprintf(opts.Stdout, "Wrote data-preserving rename migration %s\n", filename)
	_, _ = fmt.Fprintln(opts.Stdout, "Review it, then apply with 'gombit db migrate'.")
	return nil
}

// rollbackRenameMigration undoes makeRenameMigration's writes after a failed
// hash: it removes the new migration, restores atlas.sum to its prior content
// (or removes it if the run created it), and removes the migration directory if
// the run created it and it is now empty. Best effort: the hash error is what the
// caller reports.
func rollbackRenameMigration(migrationDir, migrationPath, sumPath string, prevSum []byte, sumExisted, dirExisted bool) {
	_ = os.Remove(migrationPath)
	restoreFile(sumPath, prevSum, sumExisted)
	if !dirExisted {
		_ = os.Remove(migrationDir)
	}
}

// restoreFile puts path back to prev, or removes it if it did not exist.
func restoreFile(path string, prev []byte, existed bool) {
	if existed {
		// #nosec G703 -- path is a file inside the configured migration directory
		_ = os.WriteFile(path, prev, 0o600)
		return
	}
	_ = os.Remove(path)
}

// renameSQL renders one ALTER TABLE ... RENAME TO statement per table rename,
// then one ALTER TABLE ... RENAME COLUMN per column rename, with
// driver-appropriate identifier quoting.
func renameSQL(driver config.DatabaseDriver, tables []TableRename, renames []Rename) string {
	var b strings.Builder
	for i, t := range tables {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "-- Rename table %q to %q\n", t.Old, t.New)
		fmt.Fprintf(&b, "ALTER TABLE %s RENAME TO %s;\n", quoteRenameIdent(driver, t.Old), quoteRenameIdent(driver, t.New))
	}
	for i, r := range renames {
		if i > 0 || len(tables) > 0 {
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

// renameDownSQL is the inverse of renameSQL: the column renames undone in
// reverse order, then the table renames.
func renameDownSQL(driver config.DatabaseDriver, tables []TableRename, renames []Rename) string {
	var b strings.Builder
	for i := len(renames) - 1; i >= 0; i-- {
		r := renames[i]
		fmt.Fprintf(&b, "ALTER TABLE %s RENAME COLUMN %s TO %s;\n", quoteRenameIdent(driver, r.Table), quoteRenameIdent(driver, r.NewColumn), quoteRenameIdent(driver, r.OldColumn))
	}
	for i := len(tables) - 1; i >= 0; i-- {
		t := tables[i]
		fmt.Fprintf(&b, "ALTER TABLE %s RENAME TO %s;\n", quoteRenameIdent(driver, t.New), quoteRenameIdent(driver, t.Old))
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
