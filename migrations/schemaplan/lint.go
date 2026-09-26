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
	"github.com/gombit-dev/gombit/migrations"
)

// Step codes for renames a migration declares in its own SQL.
const (
	StepRenameTable  = "rename_table"
	StepRenameColumn = "rename_column"
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
	for _, stmt := range statements(sql) {
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

// statements strips `--` comments and splits on ';'.
func statements(sql string) []string {
	var b strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	var out []string
	for _, s := range strings.Split(b.String(), ";") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
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
			renameSteps = append(renameSteps, newStep(StepRenameTable, SeveritySafe, r.To, "", fmt.Sprintf("Renames table %s to %s. The rows stay.", r.Table, r.To)))
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

// LintOptions configures Lint.
type LintOptions struct {
	WorkDir      string
	Driver       config.DatabaseDriver
	MigrationDir string
	AtlasBinary  string
	// Latest is how many of the newest migrations to classify; All classifies
	// every one. The integrity check always covers the whole directory.
	Latest int
	All    bool
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

	start := len(files) - 1
	switch {
	case opts.All:
		start = 0
	case opts.Latest > 1:
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
