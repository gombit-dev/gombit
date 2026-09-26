package schemaplan

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"ariga.io/atlas/sql/schema"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/manifest"
	"github.com/gombit-dev/gombit/migrations"
)

// Step codes for renames a migration declares in its own SQL.
const (
	StepRenameTable  = "rename_table"
	StepRenameColumn = "rename_column"
	// Statement-level findings: SQL the migration runs whose effect on rows
	// the before/after schema diff cannot show.
	StepDataChange      = "data_change"
	StepAlterColumn     = "alter_column"
	StepUnclassifiedSQL = "unclassified_sql"
)

// DeclaredRename is a rename a migration states in SQL: a table rename when
// Column is empty, otherwise Column on Table (the table's name at that point
// in the migration) becomes To.
type DeclaredRename struct {
	Table  string
	Column string
	To     string
}

var (
	ident            = `((?:[` + "`" + `"]?[A-Za-z0-9_]+[` + "`" + `"]?\.)?[` + "`" + `"]?[A-Za-z0-9_]+[` + "`" + `"]?)`
	reRenameTableTo  = regexp.MustCompile(`(?is)^\s*ALTER\s+TABLE\s+` + ident + `\s+RENAME\s+TO\s+` + ident + `\s*$`)
	reRenameTableCmd = regexp.MustCompile(`(?is)^\s*RENAME\s+TABLE\s+` + ident + `\s+TO\s+` + ident + `\s*$`)
	reRenameColumn   = regexp.MustCompile(`(?is)^\s*ALTER\s+TABLE\s+` + ident + `\s+RENAME\s+COLUMN\s+` + ident + `\s+TO\s+` + ident + `\s*$`)
)

// DeclaredRenames reads the table and column renames a migration's SQL
// states, in statement order. Comments are ignored.
func DeclaredRenames(sql string) []DeclaredRename {
	var out []DeclaredRename
	for _, stmt := range manifest.Statements(sql) {
		switch {
		case reRenameColumn.MatchString(stmt):
			m := reRenameColumn.FindStringSubmatch(stmt)
			out = append(out, DeclaredRename{Table: unquote(m[1]), Column: unquote(m[2]), To: unquote(m[3])})
		case reRenameTableTo.MatchString(stmt):
			m := reRenameTableTo.FindStringSubmatch(stmt)
			out = append(out, DeclaredRename{Table: unquote(m[1]), To: unquote(m[2])})
		case reRenameTableCmd.MatchString(stmt):
			m := reRenameTableCmd.FindStringSubmatch(stmt)
			out = append(out, DeclaredRename{Table: unquote(m[1]), To: unquote(m[2])})
		}
	}
	return out
}

// unquote drops identifier quotes and a schema qualifier.
func unquote(id string) string {
	if i := strings.LastIndex(id, "."); i >= 0 {
		id = id[i+1:]
	}
	return strings.Trim(id, "`\"")
}

// BuildWithRenames classifies an inspection whose desired state follows from
// the current one through the given renames, as a migration that declares
// them does. The renames are applied to the current schema first, so the diff
// shows what else changed (a narrowed type on a renamed column still
// classifies), and each rename is a safe step: the rows stay.
func BuildWithRenames(in migrations.Inspection, renames []DeclaredRename) (SchemaPlan, error) {
	current, desired := &schema.Realm{}, &schema.Realm{}
	if err := evalHCL(in.Driver, in.Current, current); err != nil {
		return SchemaPlan{}, fmt.Errorf("schemaplan: read the schema before the migration: %w", err)
	}
	if err := evalHCL(in.Driver, in.Desired, desired); err != nil {
		return SchemaPlan{}, fmt.Errorf("schemaplan: read the schema after the migration: %w", err)
	}
	var renameSteps []PlanStep
	for _, r := range renames {
		t := findTable(current, r.Table)
		if t == nil {
			continue // the diff will show whatever really happened
		}
		if r.Column == "" {
			step := newStep(StepRenameTable, SeveritySafe, r.To, "", fmt.Sprintf("Renames table %s to %s. The rows stay.", r.Table, r.To))
			step.from = r.Table
			renameSteps = append(renameSteps, step)
			t.Name = r.To
			continue
		}
		if c, ok := t.Column(r.Column); ok {
			renameSteps = append(renameSteps, newStep(StepRenameColumn, SeveritySafe, t.Name, r.To, fmt.Sprintf("Renames column %s.%s to %s. The data stays.", t.Name, r.Column, r.To)))
			c.Name = r.To
		}
	}
	alignSchemas(current, desired)
	changes, err := differ(in.Driver).RealmDiff(current, desired)
	if err != nil {
		return SchemaPlan{}, fmt.Errorf("schemaplan: diff schemas: %w", err)
	}
	steps := append(renameSteps, classifyChanges(in.Driver, changes)...)
	for i := range steps {
		if steps[i].Code == StepDropTable {
			steps[i].Hint = renameTableHint(steps[i], "")
		}
	}
	return SchemaPlan{Driver: in.Driver, Steps: steps}, nil
}

func findTable(r *schema.Realm, name string) *schema.Table {
	for _, s := range r.Schemas {
		if t, ok := s.Table(name); ok {
			return t
		}
	}
	return nil
}

var (
	reDataChangeTable = regexp.MustCompile(`(?is)^\s*(?:DELETE\s+FROM|UPDATE|TRUNCATE(?:\s+TABLE)?|MERGE\s+INTO|REPLACE\s+INTO)\s+` + ident)
	// Atlas's SQLite rebuild copy: INSERT INTO new_X (a, b) SELECT a, b FROM X.
	reRebuildCopy = regexp.MustCompile(`(?is)^\s*INSERT\s+INTO\s+` + ident + `\s*\(([^)]*)\)\s*SELECT\s+(.*?)\s+FROM\s+` + ident + `\s*$`)
	reUsing       = regexp.MustCompile(`(?is)\bUSING\b`)
)

// rebuildCopySource returns X when stmt is an Atlas-style rebuild copy into
// new_X: a column list, and a SELECT of exactly those columns, in order, from
// X. A SELECT of anything else (a literal, an expression) is not a copy.
func rebuildCopySource(stmt string) (string, bool) {
	m := reRebuildCopy.FindStringSubmatch(stmt)
	if m == nil {
		return "", false
	}
	target, source := unquote(m[1]), unquote(m[4])
	if target != "new_"+source {
		return "", false
	}
	cols, sel := splitList(m[2]), splitList(m[3])
	if len(cols) == 0 || len(cols) != len(sel) {
		return "", false
	}
	for i := range cols {
		if !plainIdent.MatchString(sel[i]) || unquote(sel[i]) != unquote(cols[i]) {
			return "", false
		}
	}
	return source, true
}

var plainIdent = regexp.MustCompile("^[`\"]?[A-Za-z0-9_]+[`\"]?$")

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// withStatementFindings adds what the migration's own SQL does to rows that
// the before/after diff cannot see. The HOST-3 statement classifier
// (manifest.Classify) is fail-safe: DELETE, UPDATE, TRUNCATE, DROP TABLE,
// ALTER COLUMN, and anything it does not recognize are data loss. A statement
// the diff already explains (a drop_column step for the same column, a step
// on an altered column, Atlas's own SQLite rebuild) adds nothing; any other
// becomes a destructive or unsafe step the migration has to acknowledge. A
// declared rename is only safe if the migration does not also drop the table.
func withStatementFindings(steps []PlanStep, sql string) []PlanStep {
	stmts := manifest.Statements(sql)
	ops := manifest.Classify(sql)
	have := map[string]bool{}
	columnInDiff := map[string]bool{}
	tableAccounted := map[string]bool{}
	for _, s := range steps {
		have[s.ID] = true
		if s.Name != "" {
			columnInDiff[s.Table+"."+s.Name] = true
		}
		if s.Code == StepTableRebuild || s.NeedsAcknowledgement() {
			tableAccounted[s.Table] = true
		}
	}
	// exemptDrop reports whether the DROP TABLE at index i is the middle of
	// Atlas's SQLite rebuild of X: an exact column-for-column copy into
	// new_X before it, the rename of new_X back to X after it, and a plan step
	// that accounts for the rebuild. Each copy exempts one drop. An empty
	// diff means nothing was rebuilt, so the drop stands.
	copies := map[string]int{}
	exemptDrop := func(i int, table string) bool {
		c, ok := copies[table]
		if !ok || c >= i || !tableAccounted[table] {
			return false
		}
		for _, stmt := range stmts[i+1:] {
			m := reRenameTableTo.FindStringSubmatch(stmt)
			if m != nil && unquote(m[1]) == "new_"+table && unquote(m[2]) == table {
				delete(copies, table)
				return true
			}
		}
		return false
	}

	dropped := map[string]bool{}
	var found []PlanStep
	add := func(s PlanStep) {
		if !have[s.ID] {
			have[s.ID] = true
			found = append(found, s)
		}
	}
	for i, op := range ops {
		if i >= len(stmts) {
			continue
		}
		if src, ok := rebuildCopySource(stmts[i]); ok {
			copies[src] = i
		}
		if op.Safety != manifest.SafetyDataLoss {
			continue
		}
		switch op.Kind {
		case manifest.OpDropTable:
			if exemptDrop(i, op.Resource) {
				continue
			}
			dropped[op.Resource] = true
			add(newStep(StepDropTable, SeverityDestructive, op.Resource, "", fmt.Sprintf("The migration runs DROP TABLE %s and every row in it goes, even if a table of that name exists afterwards.", op.Resource)))
		case manifest.OpDropColumn:
			add(newStep(StepDropColumn, SeverityDestructive, op.Resource, op.Column, fmt.Sprintf("The migration drops column %s.%s and the data in it.", op.Resource, op.Column)))
		case manifest.OpAlterColumn:
			// A USING expression rewrites the stored values; the schema after
			// it cannot say how, so it always needs an acknowledgement.
			if reUsing.MatchString(stmts[i]) {
				add(newStep(StepAlterColumn, SeverityDestructive, op.Resource, op.Column, fmt.Sprintf("The migration rewrites every value of %s.%s through an expression (%s); the schema before and after cannot show what it keeps.", op.Resource, op.Column, firstWords(stmts[i]))))
				continue
			}
			// Without USING the database converts values the implicit way, and
			// the diff's step for this column (a widen, a narrow, a default)
			// is the classification of exactly that.
			if columnInDiff[op.Resource+"."+op.Column] {
				continue
			}
			add(newStep(StepAlterColumn, SeverityUnsafe, op.Resource, op.Column, fmt.Sprintf("The migration alters column %s.%s in a way the schema before and after does not show.", op.Resource, op.Column)))
		default:
			if m := reDataChangeTable.FindStringSubmatch(stmts[i]); m != nil {
				table := unquote(m[1])
				add(newStep(StepDataChange, SeverityDestructive, table, "", fmt.Sprintf("The migration deletes or rewrites rows in %s (%s).", table, firstWords(stmts[i]))))
				continue
			}
			add(newStep(StepUnclassifiedSQL, SeverityDestructive, fmt.Sprintf("statement_%d", i+1), "", fmt.Sprintf("Gombit cannot classify statement %d (%s), so it is treated as destructive.", i+1, firstWords(stmts[i]))))
		}
	}

	// A declared rename whose table the migration drops keeps nothing.
	out := steps[:0:0]
	for _, s := range steps {
		if s.Code == StepRenameTable && (dropped[s.Table] || dropped[s.from]) {
			continue
		}
		out = append(out, s)
	}
	return append(out, found...)
}

func firstWords(stmt string) string {
	stmt = strings.Join(strings.Fields(stmt), " ")
	if len(stmt) > 60 {
		stmt = stmt[:57] + "..."
	}
	return stmt
}

// LintOptions configures Lint.
type LintOptions struct {
	WorkDir      string
	Driver       config.DatabaseDriver
	MigrationDir string
	AtlasBinary  string
	// Latest, when positive, classifies only the N newest migrations (a local
	// shortcut on slow dev databases). Zero classifies every migration, which
	// is what CI should run. The integrity check always covers the whole
	// directory.
	Latest int
	Stderr io.Writer
}

// MigrationLint is one migration's classified changes.
type MigrationLint struct {
	Version string     `json:"version"`
	Name    string     `json:"name"`
	File    string     `json:"file"`
	Steps   []PlanStep `json:"steps"`
}

// LintReport is the result of Lint.
type LintReport struct {
	Driver config.DatabaseDriver `json:"driver"`
	// Integrity is the atlas.sum / replay problem, if any. The migrations are
	// not classified while it stands.
	Integrity string `json:"integrity,omitempty"`
	// Misplaced are *.sql files in the migration directory that are not up
	// migrations (a down file outside downs/, a bad name).
	Misplaced  []string        `json:"misplaced,omitempty"`
	Migrations []MigrationLint `json:"migrations"`
}

// Failed reports whether the directory needs fixing.
func (r LintReport) Failed() bool {
	return r.Integrity != "" || len(r.Misplaced) > 0 || len(r.Unacknowledged()) > 0
}

// LintFinding is a destructive or unsafe step in a migration file that no
// `-- gombit:allow` line covers.
type LintFinding struct {
	File string
	Step PlanStep
}

// Unacknowledged returns every destructive or unsafe step no
// `-- gombit:allow` line covers.
func (r LintReport) Unacknowledged() []LintFinding {
	var out []LintFinding
	for _, m := range r.Migrations {
		for _, s := range m.Steps {
			if s.NeedsAcknowledgement() {
				out = append(out, LintFinding{File: m.File, Step: s})
			}
		}
	}
	return out
}

// Lint checks a migration directory: its integrity (atlas.sum and a clean
// replay, through `atlas migrate validate`), its layout, and the safety of its
// newest migrations. Each migration is classified from the schema before and
// after it, with the renames it declares applied, and a destructive or unsafe
// step passes only when the migration carries a `-- gombit:allow` line for it.
func Lint(ctx context.Context, opts LintOptions) (LintReport, error) {
	report := LintReport{Driver: opts.Driver, Migrations: []MigrationLint{}}
	dirOpts := migrations.DirOptions{WorkDir: opts.WorkDir, Driver: opts.Driver, MigrationDir: opts.MigrationDir, AtlasBinary: opts.AtlasBinary, Stderr: opts.Stderr}
	dir := opts.MigrationDir
	if dir == "" {
		dir = filepath.Join("database", "migrations")
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(opts.WorkDir, dir)
	}
	files, skipped, err := migrations.ListMigrationFilesWithSkipped(dir)
	if err != nil {
		return report, err
	}
	report.Misplaced = skipped
	if len(files) == 0 {
		return report, nil
	}
	if err := migrations.ValidateDir(ctx, dirOpts); err != nil {
		var cerr *migrations.ChecksumError
		var rerr *migrations.ReplayError
		if !errors.As(err, &cerr) && !errors.As(err, &rerr) {
			return report, err
		}
		report.Integrity = err.Error()
		return report, nil
	}

	start := 0
	if opts.Latest > 0 {
		start = max(len(files)-opts.Latest, 0)
	}
	states := map[string][]byte{}
	state := func(version string) ([]byte, error) {
		if hcl, ok := states[version]; ok {
			return hcl, nil
		}
		hcl, err := migrations.InspectDirAt(ctx, dirOpts, version)
		if err != nil {
			return nil, err
		}
		states[version] = hcl
		return hcl, nil
	}
	for i := start; i < len(files); i++ {
		f := files[i]
		prev := ""
		if i > 0 {
			prev = files[i-1].Version
		}
		before, err := state(prev)
		if err != nil {
			return report, err
		}
		after, err := state(f.Version)
		if err != nil {
			return report, err
		}
		sql, err := os.ReadFile(f.UpPath) // #nosec G304 -- an up migration listed from the configured directory
		if err != nil {
			return report, err
		}
		plan, err := BuildWithRenames(migrations.Inspection{Driver: opts.Driver, Current: before, Desired: after}, DeclaredRenames(string(sql)))
		if err != nil {
			return report, fmt.Errorf("schemaplan: %s: %w", f.Name, err)
		}
		plan.Steps = withStatementFindings(plan.Steps, string(sql))
		plan.Acknowledge(migrations.AllowDirectives(string(sql)))
		report.Migrations = append(report.Migrations, MigrationLint{Version: f.Version, Name: f.Name, File: filepath.Base(f.UpPath), Steps: plan.Steps})
	}
	return report, nil
}

// WriteLint renders a lint report for a terminal.
func WriteLint(w io.Writer, r LintReport) {
	if r.Integrity != "" {
		_, _ = fmt.Fprintf(w, "integrity: %s\n", r.Integrity)
	} else {
		_, _ = fmt.Fprintln(w, "integrity: ok (atlas.sum matches, and every migration applies to an empty database)")
	}
	for _, m := range r.Misplaced {
		_, _ = fmt.Fprintf(w, "misplaced: %s is not an up migration; move a down file to downs/<version>_<name>.down.sql, or rename it <version>_<name>.sql\n", m)
	}
	for _, m := range r.Migrations {
		_, _ = fmt.Fprintf(w, "\n%s\n", m.File)
		if len(m.Steps) == 0 {
			_, _ = fmt.Fprintln(w, "  no schema changes")
		}
		for _, s := range m.Steps {
			label := strings.ToUpper(string(s.Severity))
			if s.Severity == SeveritySafe {
				label = "safe"
			}
			ack := ""
			if s.Acknowledged && (s.Severity == SeverityDestructive || s.Severity == SeverityUnsafe) {
				ack = "  (allowed in the migration)"
			}
			_, _ = fmt.Fprintf(w, "  %-12s %s%s\n", label, s.ID, ack)
			if s.Severity != SeveritySafe {
				_, _ = fmt.Fprintf(w, "  %-12s %s\n", "", s.Detail)
			}
		}
	}
	if pending := r.Unacknowledged(); len(pending) > 0 {
		_, _ = fmt.Fprintf(w, "\n%d destructive or unsafe change(s) are not acknowledged. After handling the data, add a line to the migration for each, then run 'gombit db repair' (the edit changes atlas.sum):\n", len(pending))
		for _, p := range pending {
			_, _ = fmt.Fprintf(w, "  %s: -- gombit:allow %s\n", p.File, p.Step.ID)
		}
	}
}
