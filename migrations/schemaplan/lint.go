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
	plan, _, err := buildWithRenames(in, renames)
	return plan, err
}

// buildWithRenames is BuildWithRenames, also returning the state after the
// migration, which the statement findings check a rebuild copy against.
func buildWithRenames(in migrations.Inspection, renames []DeclaredRename) (SchemaPlan, *schema.Realm, error) {
	current, desired := &schema.Realm{}, &schema.Realm{}
	if err := evalHCL(in.Driver, in.Current, current); err != nil {
		return SchemaPlan{}, nil, fmt.Errorf("schemaplan: read the schema before the migration: %w", err)
	}
	if err := evalHCL(in.Driver, in.Desired, desired); err != nil {
		return SchemaPlan{}, nil, fmt.Errorf("schemaplan: read the schema after the migration: %w", err)
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
		return SchemaPlan{}, nil, fmt.Errorf("schemaplan: diff schemas: %w", err)
	}
	steps := append(renameSteps, classifyChanges(in.Driver, changes)...)
	for i := range steps {
		if steps[i].Code == StepDropTable {
			steps[i].Hint = renameTableHint(steps[i], "")
		}
	}
	return SchemaPlan{Driver: in.Driver, Steps: steps}, desired, nil
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
// X, that covers every column X has after the migration except the ones it
// adds (desired is that state; added is keyed table.column). A SELECT of
// anything else (a literal, an expression) or one that leaves a surviving
// column out is not a copy.
func rebuildCopySource(stmt string, desired *schema.Realm, added map[string]bool) (string, bool) {
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
	copied := map[string]bool{}
	for i := range cols {
		if !plainIdent.MatchString(sel[i]) || unquote(sel[i]) != unquote(cols[i]) {
			return "", false
		}
		copied[unquote(cols[i])] = true
	}
	after := findTable(desired, source)
	if after == nil {
		return "", false
	}
	for _, c := range after.Columns {
		if !copied[c.Name] && !added[source+"."+c.Name] {
			return "", false // a surviving column the copy leaves out
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
// the before/after diff cannot see. ALTER TABLE statements are read action by
// action; every other statement goes through the fail-safe HOST-3 statement
// classifier (manifest.Classify), for which DELETE, UPDATE, TRUNCATE, DROP
// TABLE, and anything it does not recognize are data loss. An action or
// statement the diff already explains (the column drop it reports, an
// implicit conversion of a column it reports, Atlas's own SQLite rebuild)
// adds nothing; any other becomes a destructive or unsafe step the migration
// has to acknowledge. A declared rename is only safe if the migration does
// not also drop the table. desired is the schema after the migration.
func withStatementFindings(steps []PlanStep, sql string, desired *schema.Realm) []PlanStep {
	stmts := manifest.Statements(sql)
	ops := manifest.Classify(sql)
	have := map[string]bool{}
	columnInDiff := map[string]bool{}
	tableAccounted := map[string]bool{}
	added := map[string]bool{}
	for _, s := range steps {
		have[s.ID] = true
		if s.Name != "" {
			columnInDiff[s.Table+"."+s.Name] = true
		}
		if s.Code == StepTableRebuild || s.NeedsAcknowledgement() {
			tableAccounted[s.Table] = true
		}
		if s.Code == StepAddColumn || s.Code == StepAddNotNull {
			added[s.Table+"."+s.Name] = true
		}
	}
	// exemptDrop reports whether the DROP TABLE at index i is the middle of
	// Atlas's SQLite rebuild of X: a copy into new_X before it that selects
	// every column X still has afterwards (new ones aside), the rename of
	// new_X back to X after it, and a plan step that accounts for the
	// rebuild. Each copy exempts one drop. An empty diff means nothing was
	// rebuilt, so the drop stands.
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
	alterColumn := func(table, column, stmt string, rewrite bool) {
		if rewrite {
			// A USING expression rewrites the stored values; the schema after
			// it cannot say how, so it always needs an acknowledgement.
			add(newStep(StepAlterColumn, SeverityDestructive, table, column, fmt.Sprintf("The migration rewrites every value of %s.%s through an expression (%s); the schema before and after cannot show what it keeps.", table, column, firstWords(stmt))))
			return
		}
		// Without USING the database converts values the implicit way, and
		// the diff's step for this column is the classification of that.
		if columnInDiff[table+"."+column] {
			return
		}
		add(newStep(StepAlterColumn, SeverityUnsafe, table, column, fmt.Sprintf("The migration alters column %s.%s in a way the schema before and after does not show.", table, column)))
	}
	for i, stmt := range stmts {
		if src, ok := rebuildCopySource(stmt, desired, added); ok {
			copies[src] = i
		}
		if m := reAlterTableBody.FindStringSubmatch(stmt); m != nil {
			table := unquote(m[1])
			for _, action := range splitActions(m[2]) {
				switch a := classifyAction(action); a.kind {
				case actionDropColumn:
					add(newStep(StepDropColumn, SeverityDestructive, table, a.column, fmt.Sprintf("The migration drops column %s.%s and the data in it.", table, a.column)))
				case actionAlterColumn:
					alterColumn(table, a.column, stmt, a.rewrite)
				case actionUnknownDrop:
					add(newStep(StepUnclassifiedSQL, SeverityDestructive, fmt.Sprintf("statement_%d", i+1), "", fmt.Sprintf("Gombit cannot classify statement %d (%s), so it is treated as destructive.", i+1, firstWords(stmt))))
				}
			}
			continue
		}
		if i >= len(ops) || ops[i].Safety != manifest.SafetyDataLoss {
			continue
		}
		op := ops[i]
		switch op.Kind {
		case manifest.OpDropTable:
			if exemptDrop(i, op.Resource) {
				continue
			}
			dropped[op.Resource] = true
			add(newStep(StepDropTable, SeverityDestructive, op.Resource, "", fmt.Sprintf("The migration runs DROP TABLE %s and every row in it goes, even if a table of that name exists afterwards.", op.Resource)))
		default:
			if m := reDataChangeTable.FindStringSubmatch(stmt); m != nil {
				table := unquote(m[1])
				add(newStep(StepDataChange, SeverityDestructive, table, "", fmt.Sprintf("The migration deletes or rewrites rows in %s (%s).", table, firstWords(stmt))))
				continue
			}
			add(newStep(StepUnclassifiedSQL, SeverityDestructive, fmt.Sprintf("statement_%d", i+1), "", fmt.Sprintf("Gombit cannot classify statement %d (%s), so it is treated as destructive.", i+1, firstWords(stmt))))
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

var reAlterTableBody = regexp.MustCompile(`(?is)^\s*ALTER\s+TABLE\s+(?:ONLY\s+)?(?:IF\s+EXISTS\s+)?` + ident + `\s+(.*)$`)

// splitActions splits an ALTER TABLE body into its comma-separated actions,
// ignoring commas inside parentheses and quotes.
func splitActions(body string) []string {
	var out []string
	depth, start := 0, 0
	var quote rune
	for i, r := range body {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"' || r == '`':
			quote = r
		case r == '(':
			depth++
		case r == ')':
			depth--
		case r == ',' && depth == 0:
			out = append(out, strings.TrimSpace(body[start:i]))
			start = i + 1
		}
	}
	if rest := strings.TrimSpace(body[start:]); rest != "" {
		out = append(out, rest)
	}
	return out
}

type actionKind int

const (
	actionOther actionKind = iota
	actionDropColumn
	actionAlterColumn
	actionUnknownDrop
)

type alterAction struct {
	kind    actionKind
	column  string
	rewrite bool
}

var (
	reActionDrop      = regexp.MustCompile(`(?is)^DROP\s+(?:COLUMN\s+)?(?:IF\s+EXISTS\s+)?` + ident + `(?:\s+(?:CASCADE|RESTRICT))?$`)
	reActionAlter     = regexp.MustCompile(`(?is)^(?:ALTER\s+(?:COLUMN\s+)?|MODIFY\s+(?:COLUMN\s+)?|CHANGE\s+(?:COLUMN\s+)?)` + ident + `(?:\s+(.*))?$`)
	reActionKeyword   = regexp.MustCompile(`(?is)^DROP\s+(?:CONSTRAINT|INDEX|KEY|PRIMARY\s+KEY|FOREIGN\s+KEY|UNIQUE|CHECK)\b`)
	reActionAlterMeta = regexp.MustCompile(`(?is)^ALTER\s+(?:CONSTRAINT|INDEX)\b`)
	reActionDropAny   = regexp.MustCompile(`(?is)^DROP\b`)
)

// classifyAction reads one ALTER TABLE action. Additive and metadata actions
// (ADD, RENAME, a dropped constraint or index) are actionOther; a DROP the
// reader cannot place is actionUnknownDrop.
func classifyAction(action string) alterAction {
	switch {
	case reActionKeyword.MatchString(action):
		return alterAction{kind: actionOther}
	case reActionDrop.MatchString(action):
		return alterAction{kind: actionDropColumn, column: unquote(reActionDrop.FindStringSubmatch(action)[1])}
	case reActionAlter.MatchString(action) && !reActionAlterMeta.MatchString(action):
		m := reActionAlter.FindStringSubmatch(action)
		return alterAction{kind: actionAlterColumn, column: unquote(m[1]), rewrite: reUsing.MatchString(action)}
	case reActionDropAny.MatchString(action):
		return alterAction{kind: actionUnknownDrop}
	}
	return alterAction{kind: actionOther}
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
// replay, through `atlas migrate validate`), its layout, and the safety of
// every migration (or the Latest newest, when set). Each migration is classified from the schema before and
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
		plan, desired, err := buildWithRenames(migrations.Inspection{Driver: opts.Driver, Current: before, Desired: after}, DeclaredRenames(string(sql)))
		if err != nil {
			return report, fmt.Errorf("schemaplan: %s: %w", f.Name, err)
		}
		plan.Steps = withStatementFindings(plan.Steps, string(sql), desired)
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
