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
			steps = append(steps, newStep(StepAddTable, SeveritySafe, c.T.Name, "", fmt.Sprintf("Creates table %s.", c.T.Name)))
		case *schema.DropTable:
			step := newStep(StepDropTable, SeverityDestructive, c.T.Name, "", fmt.Sprintf("Drops table %s and every row in it.", c.T.Name))
			if repl := tableRenameCandidate(c.T, added); repl != "" {
				step.Hint = fmt.Sprintf("Table %s is created in the same plan. If it replaces %s, this still drops the rows: Gombit cannot rename a table yet, so copy the rows in a hand-written migration before this one applies.", repl, c.T.Name)
			}
			steps = append(steps, step)
		case *schema.ModifyTable:
			steps = append(steps, classifyTable(driver, c)...)
		default:
			steps = append(steps, newStep(StepOther, SeverityReview, changeTable(c), "", fmt.Sprintf("Unclassified schema change (%s).", changeName(c))))
		}
	}
	return steps
}

func classifyTable(driver config.DatabaseDriver, m *schema.ModifyTable) []PlanStep {
	table := m.T.Name
	// Columns added in this same change. A unique index or a foreign key that
	// covers only new nullable columns cannot fail: every existing row holds NULL.
	newNullable := map[string]bool{}
	var addedCols []*schema.Column
	for _, c := range m.Changes {
		if add, ok := c.(*schema.AddColumn); ok {
			addedCols = append(addedCols, add.C)
			if add.C.Type != nil && add.C.Type.Null {
				newNullable[add.C.Name] = true
			}
		}
	}
	onlyNewNullable := func(cols []string) bool {
		if len(cols) == 0 {
			return false
		}
		for _, c := range cols {
			if !newNullable[c] {
				return false
			}
		}
		return true
	}

	var steps []PlanStep
	for _, c := range m.Changes {
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
			steps = append(steps, classifyModifyColumn(driver, table, c)...)
		case *schema.AddIndex:
			if c.I.Unique && !onlyNewNullable(indexColumns(c.I)) {
				step := newStep(StepAddUnique, SeverityUnsafe, table, c.I.Name, fmt.Sprintf("Adds unique index %s on %s(%s). It fails if existing rows hold duplicate values.", c.I.Name, table, strings.Join(indexColumns(c.I), ", ")))
				step.Hint = "Remove or merge the duplicates before this migration applies."
				steps = append(steps, step)
				continue
			}
			steps = append(steps, newStep(StepAddIndex, SeveritySafe, table, c.I.Name, fmt.Sprintf("Adds index %s on %s(%s).", c.I.Name, table, strings.Join(indexColumns(c.I), ", "))))
		case *schema.DropIndex:
			steps = append(steps, newStep(StepDropIndex, SeveritySafe, table, c.I.Name, fmt.Sprintf("Drops index %s.", c.I.Name)))
		case *schema.ModifyIndex:
			if c.To.Unique && !c.From.Unique {
				step := newStep(StepAddUnique, SeverityUnsafe, table, c.To.Name, fmt.Sprintf("Makes index %s on %s(%s) unique. It fails if existing rows hold duplicate values.", c.To.Name, table, strings.Join(indexColumns(c.To), ", ")))
				step.Hint = "Remove or merge the duplicates before this migration applies."
				steps = append(steps, step)
				continue
			}
			steps = append(steps, newStep(StepAddIndex, SeveritySafe, table, c.To.Name, fmt.Sprintf("Changes index %s.", c.To.Name)))
		case *schema.AddForeignKey:
			cols := fkColumns(c.F)
			ref := fkRef(c.F)
			if onlyNewNullable(cols) {
				steps = append(steps, newStep(StepAddForeignKey, SeveritySafe, table, c.F.Symbol, fmt.Sprintf("Adds foreign key %s: %s(%s) references %s.", c.F.Symbol, table, strings.Join(cols, ", "), ref)))
				continue
			}
			step := newStep(StepAddForeignKey, SeverityUnsafe, table, c.F.Symbol, fmt.Sprintf("Adds foreign key %s: %s(%s) references %s. Existing rows that point at a missing row make it fail on PostgreSQL and MySQL, and stay as violations on SQLite.", c.F.Symbol, table, strings.Join(cols, ", "), ref))
			step.Hint = "Fix or null out rows that reference missing rows before this migration applies."
			steps = append(steps, step)
		case *schema.DropForeignKey:
			steps = append(steps, newStep(StepDropForeignKey, SeverityReview, table, c.F.Symbol, fmt.Sprintf("Drops foreign key %s. The database stops enforcing that %s(%s) references %s.", c.F.Symbol, table, strings.Join(fkColumns(c.F), ", "), fkRef(c.F))))
		case *schema.ModifyForeignKey:
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
		case *schema.AddPrimaryKey, *schema.ModifyPrimaryKey, *schema.DropPrimaryKey:
			steps = append(steps, newStep(StepChangePrimaryKey, SeverityUnsafe, table, "", fmt.Sprintf("Changes the primary key of %s. It fails if existing rows hold duplicate or NULL key values.", table)))
		default:
			steps = append(steps, newStep(StepOther, SeverityReview, table, "", fmt.Sprintf("Unclassified change to %s (%s).", table, changeName(c))))
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
		step.Hint = "Give the field a default (default=...), or add it as nullable, backfill it, and make it required in a later migration."
		return step
	}
	return newStep(StepAddColumn, SeveritySafe, table, c.Name, fmt.Sprintf("Adds column %s.%s.", table, c.Name))
}

func classifyModifyColumn(driver config.DatabaseDriver, table string, m *schema.ModifyColumn) []PlanStep {
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
	if rest := m.Change &^ (schema.ChangeType | schema.ChangeNull | schema.ChangeDefault); rest != schema.NoChange {
		steps = append(steps, newStep(StepOther, SeverityReview, table, name, fmt.Sprintf("Changes other attributes of %s.%s (collation, charset, comment, or generation).", table, name)))
	}
	return steps
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
			if fr, tr := textRank(f.T), textRank(t.T); fr > 0 && tr > 0 {
				return widenIf(tr >= fr)
			}
			switch {
			case t.Size == 0 && (textRank(t.T) > 0 || f.Size > 0 || strings.Contains(t.T, "varying") || t.T == "varchar"):
				// Unbounded text, or an unsized varchar/character varying.
				return typeWiden
			case f.Size == 0 && t.Size > 0:
				return typeNarrow
			case f.Size > 0 && t.Size > 0:
				return widenIf(t.Size >= f.Size)
			}
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
			fr, tr := floatBytes(f), floatBytes(t)
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

func floatBytes(t *schema.FloatType) int {
	switch strings.ToLower(t.T) {
	case "real", "float4":
		return 4
	case "double", "double precision", "float8":
		return 8
	case "float":
		if t.Precision > 0 && t.Precision <= 24 {
			return 4
		}
		return 8
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

// tableRenameCandidate returns a table created in the same plan that shares a
// non-bookkeeping column with the dropped table.
func tableRenameCandidate(dropped *schema.Table, added []*schema.Table) string {
	bookkeeping := map[string]bool{"id": true, "created_at": true, "updated_at": true, "deleted_at": true}
	for _, t := range added {
		for _, c := range dropped.Columns {
			if bookkeeping[c.Name] {
				continue
			}
			if _, ok := t.Column(c.Name); ok {
				return t.Name
			}
		}
	}
	return ""
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
