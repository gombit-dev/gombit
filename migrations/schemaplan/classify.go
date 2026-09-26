package schemaplan

import (
	"fmt"
	"reflect"
	"slices"
	"strings"

	"ariga.io/atlas/sql/mysql"
	"ariga.io/atlas/sql/postgres"
	"ariga.io/atlas/sql/schema"
	"ariga.io/atlas/sql/sqlite"

	"github.com/gombit-dev/gombit/config"
)

// Severity ranks a planned schema change by what it can do to existing rows.
type Severity string

const (
	// SeveritySafe changes add structure or relax a rule. Existing rows keep
	// their data and the migration applies to a populated table.
	SeveritySafe Severity = "safe"
	// SeverityReview changes apply, but change behavior a reviewer should
	// confirm (a delete rule, a rebuilt table, a widened type).
	SeverityReview Severity = "review"
	// SeverityUnsafe changes can fail to apply to a table that already has
	// rows (a new NOT NULL column with no default, a new unique index).
	SeverityUnsafe Severity = "unsafe"
	// SeverityDestructive changes lose data (a dropped table or column, a
	// narrowed type).
	SeverityDestructive Severity = "destructive"
)

// Step codes. A step ID is "<code>:<table>" or "<code>:<table>.<name>", where
// name is a column, index, or constraint. --allow accepts an ID or a bare code.
const (
	StepAddTable         = "add_table"
	StepDropTable        = "drop_table"
	StepAddColumn        = "add_column"
	StepDropColumn       = "drop_column"
	StepAddNotNull       = "add_not_null"
	StepSetNotNull       = "set_not_null"
	StepDropNotNull      = "drop_not_null"
	StepNarrowType       = "narrow_type"
	StepChangeType       = "change_type"
	StepWidenType        = "widen_type"
	StepChangeDefault    = "change_default"
	StepAddIndex         = "add_index"
	StepDropIndex        = "drop_index"
	StepAddUnique        = "add_unique"
	StepAddForeignKey    = "add_foreign_key"
	StepDropForeignKey   = "drop_foreign_key"
	StepChangeForeignKey = "change_foreign_key"
	StepAddCheck         = "add_check"
	StepDropCheck        = "drop_check"
	StepChangePrimaryKey = "change_primary_key"
	StepTableRebuild     = "table_rebuild"
	StepChangeCharset    = "change_charset"
	StepChangeCollation  = "change_collation"
	StepChangeComment    = "change_comment"
	StepChangeGenerated  = "change_generated"
	StepRenameIndex      = "rename_index"
	StepRenameForeignKey = "rename_foreign_key"
	StepRenameCheck      = "rename_check"
	StepOther            = "other"
)

// PlanStep is one classified schema change.
type PlanStep struct {
	ID       string   `json:"id"`
	Code     string   `json:"code"`
	Severity Severity `json:"severity"`
	Table    string   `json:"table"`
	// Name is the column, index, or constraint the step changes. It is empty
	// for a table-level step.
	Name   string `json:"name,omitempty"`
	Detail string `json:"detail"`
	// Hint is a safer alternative or a way to make the change apply.
	Hint string `json:"hint,omitempty"`
	// Acknowledged is set by SchemaPlan.Acknowledge for a step an --allow
	// entry (or --forget-model, for a dropped table) covers.
	Acknowledged bool `json:"acknowledged"`
	// createdInPlan are the tables the same plan creates; for a dropped table,
	// any of them may be its rename. renameTo is the one that matches it
	// exactly, if exactly one does. Build turns them into the hint.
	createdInPlan []string
	renameTo      string
}

// NeedsAcknowledgement reports whether the step is destructive or unsafe and
// nothing has acknowledged it.
func (s PlanStep) NeedsAcknowledgement() bool {
	return (s.Severity == SeverityDestructive || s.Severity == SeverityUnsafe) && !s.Acknowledged
}

func newStep(code string, sev Severity, table, name, detail string) PlanStep {
	id := code + ":" + table
	if name != "" {
		id += "." + name
	}
	return PlanStep{ID: id, Code: code, Severity: sev, Table: table, Name: name, Detail: detail}
}

// classifyChanges turns Atlas's structural diff between two schema states into
// plan steps. It is pure: the states come from `atlas schema inspect`, and the
// diff from Atlas's own differ, so Gombit never parses migration SQL here.
func classifyChanges(driver config.DatabaseDriver, changes []schema.Change) []PlanStep {
	var added []*schema.Table
	for _, c := range changes {
		if add, ok := c.(*schema.AddTable); ok {
			added = append(added, add.T)
		}
	}
	var steps []PlanStep
	for _, c := range changes {
		switch c := c.(type) {
		case *schema.AddTable:
			if driver == config.DatabaseDriverMySQL && tableKeyTooLong(c.T) {
				step := newStep(StepAddTable, SeverityUnsafe, c.T.Name, "", fmt.Sprintf("Creates table %s, but one of its keys can exceed InnoDB's %d-byte key limit (or indexes TEXT/BLOB without a prefix), which fails CREATE TABLE.", c.T.Name, innodbKeyLimit))
				step.Hint = keyLimitHint
				steps = append(steps, step)
				continue
			}
			steps = append(steps, newStep(StepAddTable, SeveritySafe, c.T.Name, "", fmt.Sprintf("Creates table %s.", c.T.Name)))
		case *schema.DropTable:
			step := newStep(StepDropTable, SeverityDestructive, c.T.Name, "", fmt.Sprintf("Drops table %s and every row in it.", c.T.Name))
			for _, t := range added {
				step.createdInPlan = append(step.createdInPlan, t.Name)
			}
			step.renameTo = tableRenameCandidate(c.T, added)
			step.Hint = renameTableHint(step, "")
			steps = append(steps, step)
		case *schema.ModifyTable:
			steps = append(steps, classifyTable(driver, c)...)
		default:
			steps = append(steps, unclassified(changeTable(c), "", changeName(c)))
		}
	}
	return steps
}

func classifyTable(driver config.DatabaseDriver, m *schema.ModifyTable) []PlanStep {
	table := m.T.Name
	// Columns added in this same change that leave every existing row NULL: a
	// nullable column with no default (or an explicit NULL default). A unique
	// index or a foreign key over only those columns cannot fail. A default
	// backfills every existing row first (the SQLite rebuild omits the column
	// from its copy, so the default applies; ADD COLUMN ... DEFAULT does the
	// same on PostgreSQL and MySQL), so it can.
	nullFilled := map[string]bool{}
	var addedCols []*schema.Column
	for _, c := range m.Changes {
		if add, ok := c.(*schema.AddColumn); ok {
			addedCols = append(addedCols, add.C)
			if add.C.Type != nil && add.C.Type.Null && nullDefault(add.C.Default) {
				nullFilled[add.C.Name] = true
			}
		}
	}
	onlyNullFilled := func(cols []string) bool {
		if len(cols) == 0 {
			return false
		}
		for _, c := range cols {
			if !nullFilled[c] {
				return false
			}
		}
		return true
	}

	renamedFrom, renamedAway := pairRenames(m.Changes)

	var steps []PlanStep
	for _, c := range m.Changes {
		if renamedAway[c] {
			continue // reported as the rename on its re-added twin
		}
		if from, ok := renamedFrom[c]; ok {
			steps = append(steps, renameStep(table, from, c))
			continue
		}
		switch c := c.(type) {
		case *schema.AddColumn:
			steps = append(steps, classifyAddColumn(driver, table, c.C, alterable(m)))
		case *schema.DropColumn:
			step := newStep(StepDropColumn, SeverityDestructive, table, c.C.Name, fmt.Sprintf("Drops column %s.%s and the data in it.", table, c.C.Name))
			if cands := renameCandidates(c.C, addedCols); len(cands) > 0 {
				step.Hint = fmt.Sprintf("%s.%s is added in the same plan. If it replaces %s, keep the data with a rename instead:\n  gombit db makemigrations <name> --rename %s.%s:%s",
					table, cands[0], c.C.Name, table, c.C.Name, cands[0])
			}
			steps = append(steps, step)
		case *schema.ModifyColumn:
			steps = append(steps, classifyModifyColumn(driver, m.T, c)...)
		case *schema.AddIndex:
			cols := strings.Join(indexColumns(c.I), ", ")
			switch {
			case driver == config.DatabaseDriverMySQL && keyTooLong(c.I.Parts):
				code := StepAddIndex
				if c.I.Unique {
					code = StepAddUnique
				}
				step := newStep(code, SeverityUnsafe, table, c.I.Name, fmt.Sprintf("Adds index %s on %s(%s), but its key can exceed InnoDB's %d-byte limit (or indexes TEXT/BLOB without a prefix), which fails even on an empty table.", c.I.Name, table, cols, innodbKeyLimit))
				step.Hint = keyLimitHint
				steps = append(steps, step)
			case c.I.Unique && onlyNullFilled(indexColumns(c.I)):
				steps = append(steps, newStep(StepAddUnique, SeveritySafe, table, c.I.Name, fmt.Sprintf("Adds unique index %s on %s(%s). The columns are new, nullable, and have no default, so every existing row holds NULL and nothing collides.", c.I.Name, table, cols)))
			case c.I.Unique:
				step := newStep(StepAddUnique, SeverityUnsafe, table, c.I.Name, fmt.Sprintf("Adds unique index %s on %s(%s). It fails if existing rows hold duplicate values, including a default written into every existing row.", c.I.Name, table, cols))
				step.Hint = "Remove or merge the duplicates before this migration applies."
				steps = append(steps, step)
			default:
				steps = append(steps, newStep(StepAddIndex, SeveritySafe, table, c.I.Name, fmt.Sprintf("Adds index %s on %s(%s).", c.I.Name, table, cols)))
			}
		case *schema.DropIndex:
			steps = append(steps, newStep(StepDropIndex, SeveritySafe, table, c.I.Name, fmt.Sprintf("Drops index %s.", c.I.Name)))
		case *schema.ModifyIndex:
			// Atlas matches indexes by name, so a same-named index whose
			// uniqueness or columns change is a ModifyIndex. Anything past a
			// comment re-creates it (drop and create on PostgreSQL and MySQL,
			// a table rebuild on SQLite), so a unique index is checked against
			// the existing rows again under its new key.
			cols := strings.Join(indexColumns(c.To), ", ")
			recreated := c.Change&^schema.ChangeComment != schema.NoChange
			switch {
			case recreated && driver == config.DatabaseDriverMySQL && keyTooLong(c.To.Parts):
				code := StepAddIndex
				if c.To.Unique {
					code = StepAddUnique
				}
				step := newStep(code, SeverityUnsafe, table, c.To.Name, fmt.Sprintf("Re-creates index %s on %s(%s), but its key can exceed InnoDB's %d-byte limit (or indexes TEXT/BLOB without a prefix), which fails the migration.", c.To.Name, table, cols, innodbKeyLimit))
				step.Hint = keyLimitHint
				steps = append(steps, step)
			case c.To.Unique && recreated && onlyNullFilled(indexColumns(c.To)):
				steps = append(steps, newStep(StepAddUnique, SeveritySafe, table, c.To.Name, fmt.Sprintf("Re-creates unique index %s on %s(%s). The columns are new, nullable, and have no default, so every existing row holds NULL and nothing collides.", c.To.Name, table, cols)))
			case c.To.Unique && recreated:
				step := newStep(StepAddUnique, SeverityUnsafe, table, c.To.Name, fmt.Sprintf("Re-creates unique index %s on %s(%s). It fails if existing rows hold duplicate values under the new key.", c.To.Name, table, cols))
				step.Hint = "Remove or merge the duplicates before this migration applies."
				steps = append(steps, step)
			default:
				steps = append(steps, newStep(StepAddIndex, SeveritySafe, table, c.To.Name, fmt.Sprintf("Changes index %s on %s(%s).", c.To.Name, table, cols)))
			}
		case *schema.AddForeignKey:
			cols := fkColumns(c.F)
			ref := fkRef(c.F)
			if driver == config.DatabaseDriverMySQL && keyTooLong(columnParts(c.F.Columns)) {
				step := newStep(StepAddForeignKey, SeverityUnsafe, table, c.F.Symbol, fmt.Sprintf("Adds foreign key %s: %s(%s) references %s, but the index MySQL builds for it can exceed InnoDB's %d-byte key limit, which fails the migration.", c.F.Symbol, table, strings.Join(cols, ", "), ref, innodbKeyLimit))
				step.Hint = keyLimitHint
				steps = append(steps, step)
				continue
			}
			if onlyNullFilled(cols) {
				steps = append(steps, newStep(StepAddForeignKey, SeveritySafe, table, c.F.Symbol, fmt.Sprintf("Adds foreign key %s: %s(%s) references %s. The columns are new, nullable, and have no default, so every existing row holds NULL and passes.", c.F.Symbol, table, strings.Join(cols, ", "), ref)))
				continue
			}
			step := newStep(StepAddForeignKey, SeverityUnsafe, table, c.F.Symbol, fmt.Sprintf("Adds foreign key %s: %s(%s) references %s. Existing rows that point at a missing row, including a default written into every existing row, make it fail on PostgreSQL and MySQL and stay as violations on SQLite.", c.F.Symbol, table, strings.Join(cols, ", "), ref))
			step.Hint = "Fix or null out rows that reference missing rows before this migration applies."
			steps = append(steps, step)
		case *schema.DropForeignKey:
			steps = append(steps, newStep(StepDropForeignKey, SeverityReview, table, c.F.Symbol, fmt.Sprintf("Drops foreign key %s. The database stops enforcing that %s(%s) references %s.", c.F.Symbol, table, strings.Join(fkColumns(c.F), ", "), fkRef(c.F))))
		case *schema.ModifyForeignKey:
			// A change to the columns or the referenced key re-adds the
			// constraint, which validates the existing rows like a new one. A
			// pure ON DELETE / ON UPDATE change keeps an already-valid key.
			if c.Change.Is(schema.ChangeColumn) || c.Change.Is(schema.ChangeRefColumn) || c.Change.Is(schema.ChangeRefTable) {
				cols := fkColumns(c.To)
				if driver == config.DatabaseDriverMySQL && keyTooLong(columnParts(c.To.Columns)) {
					step := newStep(StepAddForeignKey, SeverityUnsafe, table, c.To.Symbol, fmt.Sprintf("Re-creates foreign key %s: %s, but the index MySQL builds for it can exceed InnoDB's %d-byte key limit, which fails the migration.", c.To.Symbol, describeFKChange(c), innodbKeyLimit))
					step.Hint = keyLimitHint
					steps = append(steps, step)
					continue
				}
				if onlyNullFilled(cols) {
					steps = append(steps, newStep(StepAddForeignKey, SeveritySafe, table, c.To.Symbol, fmt.Sprintf("Re-creates foreign key %s: %s. The columns are new, nullable, and have no default, so every existing row holds NULL and passes.", c.To.Symbol, describeFKChange(c))))
					continue
				}
				step := newStep(StepAddForeignKey, SeverityUnsafe, table, c.To.Symbol, fmt.Sprintf("Re-creates foreign key %s: %s. Existing rows that point at a missing row make it fail on PostgreSQL and MySQL and stay as violations on SQLite.", c.To.Symbol, describeFKChange(c)))
				step.Hint = "Fix or null out rows that reference missing rows before this migration applies."
				steps = append(steps, step)
				continue
			}
			steps = append(steps, newStep(StepChangeForeignKey, SeverityReview, table, c.To.Symbol, fmt.Sprintf("Changes foreign key %s: %s.", c.To.Symbol, describeFKChange(c))))
		case *schema.AddCheck:
			step := newStep(StepAddCheck, SeverityUnsafe, table, c.C.Name, fmt.Sprintf("Adds check %s (%s). It fails if existing rows violate it.", c.C.Name, c.C.Expr))
			step.Hint = "Fix the rows that violate the check before this migration applies."
			steps = append(steps, step)
		case *schema.ModifyCheck:
			step := newStep(StepAddCheck, SeverityUnsafe, table, c.To.Name, fmt.Sprintf("Changes check %s to (%s). It fails if existing rows violate it.", c.To.Name, c.To.Expr))
			step.Hint = "Fix the rows that violate the check before this migration applies."
			steps = append(steps, step)
		case *schema.DropCheck:
			steps = append(steps, newStep(StepDropCheck, SeveritySafe, table, c.C.Name, fmt.Sprintf("Drops check %s.", c.C.Name)))
		case *schema.AddPrimaryKey, *schema.ModifyPrimaryKey:
			steps = append(steps, newStep(StepChangePrimaryKey, SeverityUnsafe, table, "", fmt.Sprintf("Changes the primary key of %s. It fails if existing rows hold duplicate or NULL key values.", table)))
		case *schema.DropPrimaryKey:
			steps = append(steps, newStep(StepChangePrimaryKey, SeverityReview, table, "", fmt.Sprintf("Drops the primary key of %s. The database stops enforcing unique row keys.", table)))
		case *schema.AddAttr, *schema.DropAttr, *schema.ModifyAttr:
			// Table options. Charset, collation, and comment set defaults for
			// columns added later; they convert no stored value.
			if tableOptionOnly(c) {
				steps = append(steps, newStep(StepOther, SeveritySafe, table, "", fmt.Sprintf("Changes a table option of %s (charset, collation, or comment) for columns added later. Stored values are not converted.", table)))
				continue
			}
			steps = append(steps, unclassified(table, "", changeName(c)))
		default:
			steps = append(steps, unclassified(table, "", changeName(c)))
		}
	}
	// SQLite cannot alter most things in place. Atlas copies the rows into a new
	// table, drops the old one, and renames the copy; a copy that cannot satisfy
	// the new table (NOT NULL, a check) fails, which the steps above report.
	if driver == config.DatabaseDriverSQLite && !alterable(m) {
		steps = append(steps, newStep(StepTableRebuild, SeverityReview, table, "", fmt.Sprintf("SQLite rebuilds %s: it copies the rows into new_%s, drops %s, and renames the copy.", table, table, table)))
	}
	return steps
}

func classifyAddColumn(driver config.DatabaseDriver, table string, c *schema.Column, inPlace bool) PlanStep {
	if c.Type != nil && !c.Type.Null && c.Default == nil && !autoIncrement(c) {
		detail := fmt.Sprintf("Adds NOT NULL column %s.%s with no default. The migration fails when %s already has rows.", table, c.Name, table)
		if driver == config.DatabaseDriverSQLite && inPlace {
			detail = fmt.Sprintf("Adds NOT NULL column %s.%s with no default. SQLite rejects that even on an empty table.", table, c.Name)
		}
		step := newStep(StepAddNotNull, SeverityUnsafe, table, c.Name, detail)
		step.Hint = "Give the column a database default (a gorm:\"default:...\" tag on the model field; Gombit's default= field modifier is applied by the API, not the database, so it does not change this), or add it as nullable, backfill it, and make it required in a later migration."
		return step
	}
	return newStep(StepAddColumn, SeveritySafe, table, c.Name, fmt.Sprintf("Adds column %s.%s.", table, c.Name))
}

func classifyModifyColumn(driver config.DatabaseDriver, t *schema.Table, m *schema.ModifyColumn) []PlanStep {
	table := t.Name
	name := m.To.Name
	var steps []PlanStep
	if m.Change.Is(schema.ChangeType) {
		from, to := typeString(m.From), typeString(m.To)
		dir := typeDirection(driver, m.From.Type.Type, m.To.Type.Type)
		switch {
		case driver == config.DatabaseDriverSQLite && dir != typeWiden:
			// SQLite column types are affinities: the table copy keeps every
			// stored value as it is, so the change neither fails nor truncates.
			steps = append(steps, newStep(StepChangeType, SeverityReview, table, name, fmt.Sprintf("Changes %s.%s from %s to %s. SQLite keeps stored values as they are, so existing rows may not match the new type.", table, name, from, to)))
		case dir == typeWiden && driver == config.DatabaseDriverMySQL && indexOverKeyLimit(t, name):
			// Every value fits the column, but MySQL rebuilds the indexes on it
			// and a key past the InnoDB limit (or an unprefixed TEXT) fails.
			step := newStep(StepWidenType, SeverityUnsafe, table, name, fmt.Sprintf("Widens %s.%s from %s to %s, but an index on it can exceed InnoDB's %d-byte key limit (or indexes TEXT without a prefix), which fails the migration.", table, name, from, to, innodbKeyLimit))
			step.Hint = "Index a prefix of the column, or keep the narrower type."
			steps = append(steps, step)
		case dir == typeWiden:
			steps = append(steps, newStep(StepWidenType, SeverityReview, table, name, fmt.Sprintf("Widens %s.%s from %s to %s. Existing values fit.", table, name, from, to)))
		case dir == typeNarrow:
			step := newStep(StepNarrowType, SeverityDestructive, table, name, fmt.Sprintf("Narrows %s.%s from %s to %s. Values that do not fit are truncated or make the migration fail.", table, name, from, to))
			step.Hint = "Check the longest or largest stored value first, or keep the wider type."
			steps = append(steps, step)
		default:
			steps = append(steps, newStep(StepChangeType, SeverityUnsafe, table, name, fmt.Sprintf("Changes %s.%s from %s to %s. Values that do not convert make the migration fail or lose data.", table, name, from, to)))
		}
	}
	if m.Change.Is(schema.ChangeNull) {
		if m.To.Type.Null {
			steps = append(steps, newStep(StepDropNotNull, SeveritySafe, table, name, fmt.Sprintf("Makes %s.%s nullable.", table, name)))
		} else {
			step := newStep(StepSetNotNull, SeverityUnsafe, table, name, fmt.Sprintf("Makes %s.%s NOT NULL. The migration fails if any row holds NULL there.", table, name))
			step.Hint = fmt.Sprintf("Backfill the NULLs first (UPDATE %s SET %s = ... WHERE %s IS NULL) in an earlier migration.", table, name, name)
			steps = append(steps, step)
		}
	}
	if m.Change.Is(schema.ChangeDefault) {
		steps = append(steps, newStep(StepChangeDefault, SeveritySafe, table, name, fmt.Sprintf("Changes the default of %s.%s. Existing rows keep their values.", table, name)))
	}
	if m.Change.Is(schema.ChangeCharset) {
		from, to := columnCharset(m.From), columnCharset(m.To)
		switch {
		case strings.EqualFold(to, "utf8mb4") && indexOverKeyLimit(t, name):
			// Every character converts, but the indexes on the column are
			// rebuilt at 4 bytes per character and must fit InnoDB's key limit.
			step := newStep(StepChangeCharset, SeverityUnsafe, table, name, fmt.Sprintf("Converts %s.%s from charset %s to %s. An index on the column can exceed InnoDB's %d-byte key limit at 4 bytes per character, which fails the migration.", table, name, orUnset(from), to, innodbKeyLimit))
			step.Hint = "Shorten the column or index a prefix of it before converting."
			steps = append(steps, step)
		case strings.EqualFold(to, "utf8mb4"):
			// utf8mb4 holds every character the other charsets can.
			steps = append(steps, newStep(StepChangeCharset, SeverityReview, table, name, fmt.Sprintf("Converts %s.%s from charset %s to %s. Every stored character has an equivalent, and the indexes on the column fit the key limit.", table, name, orUnset(from), to)))
		default:
			step := newStep(StepChangeCharset, SeverityUnsafe, table, name, fmt.Sprintf("Converts %s.%s from charset %s to %s. A stored character with no equivalent in %s fails the migration in strict mode or is replaced.", table, name, orUnset(from), orUnset(to), orUnset(to)))
			step.Hint = "Check the stored values for characters outside the new charset first."
			steps = append(steps, step)
		}
	}
	if m.Change.Is(schema.ChangeCollate) {
		from, to := columnCollation(m.From), columnCollation(m.To)
		if other, ok := fkCollationMismatch(t, m.To); ok {
			step := newStep(StepChangeCollation, SeverityUnsafe, table, name, fmt.Sprintf("Changes the collation of %s.%s from %s to %s, but it is in a foreign key with %s, which keeps a different collation. Both sides of a foreign key must share a collation, so the migration fails.", table, name, orUnset(from), orUnset(to), other))
			step.Hint = "Change the collation on both sides of the foreign key in the same migration."
			steps = append(steps, step)
		} else if uniquelyIndexed(t, name) {
			step := newStep(StepChangeCollation, SeverityUnsafe, table, name, fmt.Sprintf("Changes the collation of %s.%s from %s to %s. The column is in a unique key, which is rebuilt and fails if two stored values compare equal under the new collation.", table, name, orUnset(from), orUnset(to)))
			step.Hint = "Check for values that differ only in case or accents before this migration applies."
			steps = append(steps, step)
		} else {
			steps = append(steps, newStep(StepChangeCollation, SeverityReview, table, name, fmt.Sprintf("Changes the collation of %s.%s from %s to %s. Sorting and comparisons change.", table, name, orUnset(from), orUnset(to))))
		}
	}
	if m.Change.Is(schema.ChangeComment) {
		steps = append(steps, newStep(StepChangeComment, SeveritySafe, table, name, fmt.Sprintf("Changes the comment of %s.%s.", table, name)))
	}
	if m.Change.Is(schema.ChangeGenerated) {
		step := newStep(StepChangeGenerated, SeverityUnsafe, table, name, fmt.Sprintf("Changes the generated expression of %s.%s. Stored values are recomputed, which fails if the new expression errors on an existing row.", table, name))
		step.Hint = "Evaluate the new expression against the stored rows first."
		steps = append(steps, step)
	}
	known := schema.ChangeType | schema.ChangeNull | schema.ChangeDefault | schema.ChangeCharset | schema.ChangeCollate | schema.ChangeComment | schema.ChangeGenerated
	if m.Change&^known != schema.NoChange {
		steps = append(steps, unclassified(table, name, "column attribute change"))
	}
	return steps
}

// unclassified is a change the classifier does not recognize. Like HOST-3's
// manifest.Classify, it fails closed: the step is unsafe, so the gate stops
// until someone reads the SQL and acknowledges it.
func unclassified(table, name, what string) PlanStep {
	where := table
	if name != "" {
		where += "." + name
	}
	step := newStep(StepOther, SeverityUnsafe, table, name, fmt.Sprintf("Gombit cannot classify this change to %s (%s), so it is treated as unsafe.", where, what))
	step.Hint = "Read the SQL makemigrations would write for it before acknowledging it."
	return step
}

// tableOptionOnly reports a table attribute change limited to charset,
// collation, or comment.
func tableOptionOnly(c schema.Change) bool {
	option := func(a schema.Attr) bool {
		switch a.(type) {
		case *schema.Charset, *schema.Collation, *schema.Comment:
			return true
		}
		return false
	}
	switch c := c.(type) {
	case *schema.AddAttr:
		return option(c.A)
	case *schema.DropAttr:
		return option(c.A)
	case *schema.ModifyAttr:
		return option(c.From) && option(c.To)
	}
	return false
}

func columnCharset(c *schema.Column) string {
	var cs schema.Charset
	for _, a := range c.Attrs {
		if v, ok := a.(*schema.Charset); ok {
			cs = *v
		}
	}
	return cs.V
}

func columnCollation(c *schema.Column) string {
	var co schema.Collation
	for _, a := range c.Attrs {
		if v, ok := a.(*schema.Collation); ok {
			co = *v
		}
	}
	return co.V
}

const keyLimitHint = "Shorten the columns, or index a prefix of them."

// innodbKeyLimit is InnoDB's index key limit for the DYNAMIC row format, the
// MySQL 8 default.
const innodbKeyLimit = 3072

// A MySQL key is built for the primary key, every index, and every foreign
// key (InnoDB indexes the referencing columns when no index covers them).
// Each must fit innodbKeyLimit, or the statement that builds it fails, even on
// an empty table.

// keyBytes is an upper bound on a key's length in bytes. ok is false when a
// part cannot be bounded (an expression, TEXT or BLOB with no prefix, a type
// with no known width); callers treat that as over the limit.
func keyBytes(parts []*schema.IndexPart) (n int, ok bool) {
	for _, p := range parts {
		if p.C == nil {
			return 0, false
		}
		b, ok := partBytes(p)
		if !ok {
			return 0, false
		}
		n += b
	}
	return n, true
}

// keyTooLong reports a key MySQL may refuse to build.
func keyTooLong(parts []*schema.IndexPart) bool {
	n, ok := keyBytes(parts)
	return !ok || n > innodbKeyLimit
}

func partBytes(p *schema.IndexPart) (int, bool) {
	for _, a := range p.Attrs {
		if sub, ok := a.(*mysql.SubPart); ok && sub.Len > 0 {
			// A prefix counts characters on a string and bytes on a binary
			// column; 4 bytes a unit bounds both.
			return sub.Len * maxBytesPerChar, true
		}
	}
	return columnKeyBytes(p.C)
}

// columnKeyBytes bounds one column's key length with no prefix.
func columnKeyBytes(c *schema.Column) (int, bool) {
	if c.Type == nil {
		return 0, false
	}
	switch t := c.Type.Type.(type) {
	case *schema.StringType:
		if t.Size > 0 {
			return t.Size * maxBytesPerChar, true
		}
	case *schema.BinaryType:
		if t.Size != nil && *t.Size > 0 {
			return *t.Size, true
		}
	case *schema.IntegerType, *schema.FloatType, *schema.TimeType:
		return 8, true
	case *schema.DecimalType:
		if t.Precision > 0 {
			return t.Precision/2 + 1, true
		}
		return 33, true // DECIMAL(65) is at most 30 bytes
	case *schema.BoolType:
		return 1, true
	case *schema.EnumType:
		return 2, true
	case *schema.UUIDType:
		return 16, true
	}
	return 0, false
}

func columnParts(cols []*schema.Column) []*schema.IndexPart {
	parts := make([]*schema.IndexPart, 0, len(cols))
	for _, c := range cols {
		parts = append(parts, &schema.IndexPart{C: c})
	}
	return parts
}

// tableKeys are the keys MySQL builds for t.
func tableKeys(t *schema.Table) [][]*schema.IndexPart {
	var keys [][]*schema.IndexPart
	if t.PrimaryKey != nil {
		keys = append(keys, t.PrimaryKey.Parts)
	}
	for _, idx := range t.Indexes {
		keys = append(keys, idx.Parts)
	}
	for _, fk := range t.ForeignKeys {
		keys = append(keys, columnParts(fk.Columns))
	}
	return keys
}

// tableKeyTooLong reports a new table with a key MySQL may refuse to build.
func tableKeyTooLong(t *schema.Table) bool {
	for _, k := range tableKeys(t) {
		if keyTooLong(k) {
			return true
		}
	}
	return false
}

// indexOverKeyLimit reports whether a key on column may exceed InnoDB's
// limit once the column takes its new type or charset. t is the desired table.
func indexOverKeyLimit(t *schema.Table, column string) bool {
	for _, k := range tableKeys(t) {
		for _, p := range k {
			if p.C != nil && p.C.Name == column && keyTooLong(k) {
				return true
			}
		}
	}
	return false
}

// fkCollationMismatch reports a foreign key through column c whose other side
// ends up with a different effective collation, and names that side. It
// checks both the keys t declares and the keys other tables declare against t.
func fkCollationMismatch(t *schema.Table, c *schema.Column) (string, bool) {
	want := effectiveCollation(c, t)
	for _, fk := range t.ForeignKeys {
		for i, col := range fk.Columns {
			if col.Name != c.Name || i >= len(fk.RefColumns) || fk.RefTable == nil {
				continue
			}
			ref := fk.RefColumns[i]
			if got := effectiveCollation(ref, fk.RefTable); !strings.EqualFold(got, want) {
				return fmt.Sprintf("%s.%s (%s)", fk.RefTable.Name, ref.Name, orUnset(got)), true
			}
		}
	}
	if t.Schema == nil {
		return "", false
	}
	for _, child := range t.Schema.Tables {
		for _, fk := range child.ForeignKeys {
			if fk.RefTable == nil || fk.RefTable.Name != t.Name {
				continue
			}
			for i, ref := range fk.RefColumns {
				if ref.Name != c.Name || i >= len(fk.Columns) {
					continue
				}
				col := fk.Columns[i]
				if got := effectiveCollation(col, child); !strings.EqualFold(got, want) {
					return fmt.Sprintf("%s.%s (%s)", child.Name, col.Name, orUnset(got)), true
				}
			}
		}
	}
	return "", false
}

// effectiveCollation is the column's collation, else its table's, else its
// schema's.
func effectiveCollation(c *schema.Column, t *schema.Table) string {
	if v := columnCollation(c); v != "" {
		return v
	}
	find := func(attrs []schema.Attr) string {
		for _, a := range attrs {
			if v, ok := a.(*schema.Collation); ok {
				return v.V
			}
		}
		return ""
	}
	if t != nil {
		if v := find(t.Attrs); v != "" {
			return v
		}
		if t.Schema != nil {
			return find(t.Schema.Attrs)
		}
	}
	return ""
}

func orUnset(v string) string {
	if v == "" {
		return "(default)"
	}
	return v
}

// uniquelyIndexed reports whether column is part of a unique index or the
// primary key of t.
func uniquelyIndexed(t *schema.Table, column string) bool {
	in := func(parts []*schema.IndexPart) bool {
		for _, p := range parts {
			if p.C != nil && p.C.Name == column {
				return true
			}
		}
		return false
	}
	if t.PrimaryKey != nil && in(t.PrimaryKey.Parts) {
		return true
	}
	for _, idx := range t.Indexes {
		if idx.Unique && in(idx.Parts) {
			return true
		}
	}
	return false
}

type typeDir int

const (
	typeUnknown typeDir = iota
	typeWiden
	typeNarrow
)

// typeDirection reports whether a column type change keeps every existing
// value (widen), can lose one (narrow), or cannot be judged (unknown).
func typeDirection(driver config.DatabaseDriver, from, to schema.Type) typeDir {
	switch f := from.(type) {
	case *schema.IntegerType:
		switch t := to.(type) {
		case *schema.IntegerType:
			if f.Unsigned != t.Unsigned {
				return typeUnknown
			}
			fr, tr := intBytes(driver, f.T), intBytes(driver, t.T)
			if fr == 0 || tr == 0 {
				return typeUnknown
			}
			return widenIf(tr >= fr)
		case *schema.DecimalType:
			if t.Precision == 0 {
				return typeWiden
			}
			return widenIf(t.Precision-t.Scale >= intDigits(driver, f.T))
		case *schema.StringType:
			return widenIf(t.Size == 0 || t.Size >= 20)
		}
	case *schema.StringType:
		if t, ok := to.(*schema.StringType); ok {
			return widenIf(stringFits(stringCapacity(driver, f), stringCapacity(driver, t)))
		}
	case *schema.DecimalType:
		if t, ok := to.(*schema.DecimalType); ok {
			switch {
			case t.Precision == 0:
				return typeWiden
			case f.Precision == 0:
				return typeNarrow
			}
			return widenIf(t.Precision-t.Scale >= f.Precision-f.Scale && t.Scale >= f.Scale)
		}
		if t, ok := to.(*schema.StringType); ok && t.Size == 0 {
			return typeWiden
		}
	case *schema.FloatType:
		if t, ok := to.(*schema.FloatType); ok {
			fr, tr := floatBytes(driver, f), floatBytes(driver, t)
			if fr == 0 || tr == 0 {
				return typeUnknown
			}
			return widenIf(tr >= fr)
		}
	case *schema.TimeType:
		if t, ok := to.(*schema.TimeType); ok && strings.EqualFold(f.T, t.T) {
			fp, tp := precision(f.Precision), precision(t.Precision)
			return widenIf(tp >= fp)
		}
	}
	// Any value renders as unbounded text without loss.
	if t, ok := to.(*schema.StringType); ok && t.Size == 0 && textRank(t.T) >= textRank("text") {
		switch from.(type) {
		case *schema.BoolType, *schema.TimeType, *schema.FloatType, *schema.UUIDType:
			return typeWiden
		}
	}
	return typeUnknown
}

// stringCap is what a string type holds: characters for a sized char or
// varchar, bytes for MySQL's text types, or no limit.
type stringCap struct {
	chars, bytes int64
	unbounded    bool
}

// maxBytesPerChar bounds a character when the charset is unknown (utf8mb4).
const maxBytesPerChar = 4

// stringCapacity reads a string type's limit. Atlas stores no size for the
// MySQL text types, but each has a byte cap; size 0 is unbounded only for
// PostgreSQL text and an unsized varchar.
func stringCapacity(driver config.DatabaseDriver, t *schema.StringType) stringCap {
	if driver == config.DatabaseDriverMySQL {
		switch strings.ToLower(t.T) {
		case "tinytext":
			return stringCap{bytes: 255}
		case "text":
			return stringCap{bytes: 65535}
		case "mediumtext":
			return stringCap{bytes: 16777215}
		case "longtext":
			return stringCap{bytes: 4294967295}
		}
	}
	if t.Size > 0 {
		return stringCap{chars: int64(t.Size)}
	}
	return stringCap{unbounded: true}
}

// stringFits reports whether every value of from fits in to.
func stringFits(from, to stringCap) bool {
	switch {
	case to.unbounded:
		return true
	case from.unbounded:
		return false
	case from.chars > 0 && to.chars > 0:
		return to.chars >= from.chars
	case from.bytes > 0 && to.bytes > 0:
		return to.bytes >= from.bytes
	case from.chars > 0:
		// Characters into a byte cap: assume the widest encoding.
		return from.chars*maxBytesPerChar <= to.bytes
	default:
		// A byte cap into characters: a character is at least one byte.
		return to.chars >= from.bytes
	}
}

func widenIf(ok bool) typeDir {
	if ok {
		return typeWiden
	}
	return typeNarrow
}

func intBytes(driver config.DatabaseDriver, t string) int {
	switch strings.ToLower(t) {
	case "tinyint":
		return 1
	case "smallint", "int2":
		return 2
	case "mediumint":
		return 3
	case "int", "int4":
		return 4
	case "integer":
		// SQLite INTEGER stores up to 8 bytes.
		if driver == config.DatabaseDriverSQLite {
			return 8
		}
		return 4
	case "bigint", "int8":
		return 8
	}
	return 0
}

// intDigits is the decimal digits an integer type needs.
func intDigits(driver config.DatabaseDriver, t string) int {
	switch intBytes(driver, t) {
	case 1:
		return 3
	case 2:
		return 5
	case 3:
		return 8
	case 4:
		return 10
	default:
		return 20
	}
}

// textRank orders the unbounded text types. Zero means t is not one of them.
func textRank(t string) int {
	switch strings.ToLower(t) {
	case "tinytext":
		return 1
	case "text", "clob":
		return 2
	case "mediumtext":
		return 3
	case "longtext":
		return 4
	}
	return 0
}

// floatBytes is the storage width of a floating-point type, per dialect.
func floatBytes(driver config.DatabaseDriver, t *schema.FloatType) int {
	name := strings.ToLower(t.T)
	switch driver {
	case config.DatabaseDriverMySQL:
		// MySQL FLOAT is single precision; FLOAT(p) switches to double at
		// p >= 24. REAL is DOUBLE unless REAL_AS_FLOAT is set.
		switch name {
		case "float":
			if t.Precision >= 24 {
				return 8
			}
			return 4
		case "double", "double precision", "real":
			return 8
		}
	case config.DatabaseDriverSQLite:
		// Every SQLite floating-point value is an 8-byte REAL.
		return 8
	default:
		// PostgreSQL: float(p) is real for p 1..24 and double precision for
		// 25..53 or no p.
		switch name {
		case "real", "float4":
			return 4
		case "double precision", "float8":
			return 8
		case "float":
			if t.Precision > 0 && t.Precision <= 24 {
				return 4
			}
			return 8
		}
	}
	return 0
}

func precision(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func typeString(c *schema.Column) string {
	if c.Type == nil {
		return "?"
	}
	if c.Type.Raw != "" {
		return c.Type.Raw
	}
	switch t := c.Type.Type.(type) {
	case *schema.StringType:
		if t.Size > 0 {
			return fmt.Sprintf("%s(%d)", t.T, t.Size)
		}
		return t.T
	case *schema.DecimalType:
		if t.Precision > 0 {
			return fmt.Sprintf("%s(%d,%d)", t.T, t.Precision, t.Scale)
		}
		return t.T
	case *schema.IntegerType:
		if t.Unsigned {
			return t.T + " unsigned"
		}
		return t.T
	}
	if v := reflect.ValueOf(c.Type.Type); v.Kind() == reflect.Pointer && !v.IsNil() {
		if f := v.Elem().FieldByName("T"); f.IsValid() && f.Kind() == reflect.String {
			return f.String()
		}
	}
	return fmt.Sprintf("%T", c.Type.Type)
}

// renameTableHint tells a dropped table's owner how to keep its rows when a
// table created in the same plan replaces it. models are the --model /
// --forget-model flags of the run, already formatted.
func renameTableHint(s PlanStep, models string) string {
	switch {
	case s.renameTo != "":
		return fmt.Sprintf("Table %s is created in the same plan with the same columns. If it replaces %s, keep the rows with a rename instead:\n  gombit db makemigrations <name> --rename-table %s:%s%s", s.renameTo, s.Table, s.Table, s.renameTo, models)
	case len(s.createdInPlan) > 0:
		return fmt.Sprintf("The same plan creates %s. If one of them replaces %s, keep the rows with a table rename, then --rename any column that changed name:\n  gombit db makemigrations <name> --rename-table %s:<new_table>%s\nIf %s is really going away, acknowledge the drop with --allow drop_table:%s.", strings.Join(s.createdInPlan, ", "), s.Table, s.Table, models, s.Table, s.Table)
	}
	return ""
}

// pairRenames finds constraints dropped and re-added under a new name with
// the same definition: GORM derives index and foreign key names from the
// table, so a renamed table shows its keys that way. It pairs only an exact
// definition (columns, order, direction, prefixes, predicate, nulls
// handling, every attribute), because re-creating anything stricter can
// reject rows the old one accepted; those fall through to the Add*
// classifiers. renamedFrom maps the re-adding change to the old name;
// renamedAway marks the matching drop.
func pairRenames(changes []schema.Change) (renamedFrom map[schema.Change]string, renamedAway map[schema.Change]bool) {
	renamedFrom, renamedAway = map[schema.Change]string{}, map[schema.Change]bool{}
	var drops []schema.Change
	for _, c := range changes {
		switch c.(type) {
		case *schema.DropIndex, *schema.DropForeignKey, *schema.DropCheck:
			drops = append(drops, c)
		}
	}
	for _, c := range changes {
		for _, d := range drops {
			if !renamedAway[d] && sameDefinition(d, c) {
				renamedAway[d] = true
				renamedFrom[c] = constraintName(d)
				break
			}
		}
	}
	return renamedFrom, renamedAway
}

// sameDefinition reports whether add re-creates exactly what drop removes.
func sameDefinition(drop, add schema.Change) bool {
	switch d := drop.(type) {
	case *schema.DropIndex:
		a, ok := add.(*schema.AddIndex)
		return ok && sameIndex(d.I, a.I)
	case *schema.DropForeignKey:
		a, ok := add.(*schema.AddForeignKey)
		return ok && fkSignature(d.F) == fkSignature(a.F)
	case *schema.DropCheck:
		a, ok := add.(*schema.AddCheck)
		return ok && d.C.Expr == a.C.Expr && attrsEqual(d.C.Attrs, a.C.Attrs)
	}
	return false
}

func sameIndex(a, b *schema.Index) bool {
	if a.Unique != b.Unique || len(a.Parts) != len(b.Parts) || !attrsEqual(a.Attrs, b.Attrs) {
		return false
	}
	for i := range a.Parts {
		pa, pb := a.Parts[i], b.Parts[i]
		if pa.Desc != pb.Desc || !reflect.DeepEqual(pa.X, pb.X) || !attrsEqual(pa.Attrs, pb.Attrs) {
			return false
		}
		if (pa.C == nil) != (pb.C == nil) || (pa.C != nil && pa.C.Name != pb.C.Name) {
			return false
		}
	}
	return true
}

// attrsEqual compares attribute lists (a predicate, NULLS NOT DISTINCT, a
// prefix length, an index type, ...) by value; nil and empty are equal.
func attrsEqual(a, b []schema.Attr) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

func fkSignature(f *schema.ForeignKey) string {
	return fmt.Sprintf("fk|%s|%s|%s|%s", strings.Join(fkColumns(f), ","), fkRef(f), fkAction(f.OnDelete), fkAction(f.OnUpdate))
}

func constraintName(c schema.Change) string {
	switch c := c.(type) {
	case *schema.DropIndex:
		return c.I.Name
	case *schema.DropForeignKey:
		return c.F.Symbol
	case *schema.DropCheck:
		return c.C.Name
	}
	return ""
}

// renameStep reports a constraint re-created under a new name with the same
// definition. The existing rows already satisfy it, so it applies.
func renameStep(table, from string, c schema.Change) PlanStep {
	switch c := c.(type) {
	case *schema.AddIndex:
		return newStep(StepRenameIndex, SeveritySafe, table, c.I.Name, fmt.Sprintf("Renames index %s to %s on %s(%s). The definition is unchanged, so the existing rows already satisfy it.", from, c.I.Name, table, strings.Join(indexColumns(c.I), ", ")))
	case *schema.AddForeignKey:
		return newStep(StepRenameForeignKey, SeveritySafe, table, c.F.Symbol, fmt.Sprintf("Renames foreign key %s to %s. The definition is unchanged, so the existing rows already satisfy it.", from, c.F.Symbol))
	case *schema.AddCheck:
		return newStep(StepRenameCheck, SeveritySafe, table, c.C.Name, fmt.Sprintf("Renames check %s to %s. The expression is unchanged, so the existing rows already satisfy it.", from, c.C.Name))
	}
	return unclassified(table, "", changeName(c))
}

// renameCandidates are the columns added in the same change whose type family
// matches the dropped column: the shape a field rename takes in a diff.
func renameCandidates(dropped *schema.Column, added []*schema.Column) []string {
	var out []string
	for _, a := range added {
		if dropped.Type != nil && a.Type != nil && reflect.TypeOf(dropped.Type.Type) == reflect.TypeOf(a.Type.Type) {
			out = append(out, a.Name)
		}
	}
	return out
}

// tableRenameCandidate returns the one table created in the same plan that
// holds every non-bookkeeping column of the dropped table with the same type.
// It returns "" when none or more than one does, or when the dropped table has
// only bookkeeping columns: then nothing identifies the rename.
func tableRenameCandidate(dropped *schema.Table, added []*schema.Table) string {
	bookkeeping := map[string]bool{"id": true, "created_at": true, "updated_at": true, "deleted_at": true}
	var cols []*schema.Column
	for _, c := range dropped.Columns {
		if !bookkeeping[c.Name] {
			cols = append(cols, c)
		}
	}
	if len(cols) == 0 {
		return ""
	}
	match := ""
	for _, t := range added {
		all := true
		for _, c := range cols {
			tc, ok := t.Column(c.Name)
			if !ok || typeString(tc) != typeString(c) {
				all = false
				break
			}
		}
		if !all {
			continue
		}
		if match != "" {
			return "" // ambiguous
		}
		match = t.Name
	}
	return match
}

func indexColumns(i *schema.Index) []string {
	var cols []string
	for _, p := range i.Parts {
		switch {
		case p.C != nil:
			cols = append(cols, p.C.Name)
		case p.X != nil:
			cols = append(cols, "<expr>")
		}
	}
	return cols
}

func fkColumns(f *schema.ForeignKey) []string {
	cols := make([]string, 0, len(f.Columns))
	for _, c := range f.Columns {
		cols = append(cols, c.Name)
	}
	return cols
}

func fkRef(f *schema.ForeignKey) string {
	cols := make([]string, 0, len(f.RefColumns))
	for _, c := range f.RefColumns {
		cols = append(cols, c.Name)
	}
	table := "?"
	if f.RefTable != nil {
		table = f.RefTable.Name
	}
	return fmt.Sprintf("%s(%s)", table, strings.Join(cols, ", "))
}

func describeFKChange(m *schema.ModifyForeignKey) string {
	var parts []string
	if m.Change.Is(schema.ChangeDeleteAction) {
		parts = append(parts, fmt.Sprintf("ON DELETE %s -> %s", fkAction(m.From.OnDelete), fkAction(m.To.OnDelete)))
	}
	if m.Change.Is(schema.ChangeUpdateAction) {
		parts = append(parts, fmt.Sprintf("ON UPDATE %s -> %s", fkAction(m.From.OnUpdate), fkAction(m.To.OnUpdate)))
	}
	if m.Change.Is(schema.ChangeRefTable) || m.Change.Is(schema.ChangeRefColumn) || m.Change.Is(schema.ChangeColumn) {
		parts = append(parts, fmt.Sprintf("now %s(%s) references %s", m.To.Table.Name, strings.Join(fkColumns(m.To), ", "), fkRef(m.To)))
	}
	if len(parts) == 0 {
		return "its definition changes"
	}
	return strings.Join(parts, "; ")
}

func fkAction(a schema.ReferenceOption) string {
	if a == "" {
		return string(schema.NoAction)
	}
	return string(a)
}

// nullDefault reports whether a column default leaves existing rows NULL:
// no default at all, or an explicit NULL.
func nullDefault(d schema.Expr) bool {
	switch d := d.(type) {
	case nil:
		return true
	case *schema.Literal:
		return strings.EqualFold(strings.TrimSpace(d.V), "NULL")
	case *schema.RawExpr:
		return strings.EqualFold(strings.TrimSpace(d.X), "NULL")
	}
	return false
}

// autoIncrement reports a column the database fills itself, so a NOT NULL
// column with no default still applies.
func autoIncrement(c *schema.Column) bool {
	if _, ok := c.Type.Type.(*postgres.SerialType); ok {
		return true
	}
	for _, a := range c.Attrs {
		switch a.(type) {
		case *sqlite.AutoIncrement, *mysql.AutoIncrement, *postgres.Identity:
			return true
		}
	}
	return false
}

// alterable mirrors Atlas's SQLite planner (sql/sqlite/migrate.go alterable):
// a table change SQLite applies in place instead of rebuilding the table.
func alterable(m *schema.ModifyTable) bool {
	for _, change := range m.Changes {
		switch change := change.(type) {
		case *schema.RenameColumn, *schema.RenameIndex, *schema.DropIndex, *schema.AddIndex:
		case *schema.AddColumn:
			if len(change.C.Indexes) > 0 || len(change.C.ForeignKeys) > 0 {
				return false
			}
			switch x := change.C.Default.(type) {
			case *schema.Literal:
				if slices.Contains([]string{"CURRENT_TIME", "CURRENT_DATE", "CURRENT_TIMESTAMP"}, x.V) {
					return false
				}
			case *schema.RawExpr:
				return false
			}
			if x := (schema.GeneratedExpr{}); hasAttr(change.C.Attrs, &x) && strings.EqualFold(x.Type, "STORED") {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func hasAttr(attrs []schema.Attr, target *schema.GeneratedExpr) bool {
	for _, a := range attrs {
		if g, ok := a.(*schema.GeneratedExpr); ok {
			*target = *g
			return true
		}
	}
	return false
}

func changeTable(c schema.Change) string {
	switch c := c.(type) {
	case *schema.AddTable:
		return c.T.Name
	case *schema.DropTable:
		return c.T.Name
	case *schema.ModifyTable:
		return c.T.Name
	}
	return ""
}

func changeName(c schema.Change) string {
	return strings.TrimPrefix(fmt.Sprintf("%T", c), "*schema.")
}
