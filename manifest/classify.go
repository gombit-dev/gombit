package manifest

import (
	"regexp"
	"strings"
)

// OpKind is a classified migration operation. The set is closed; unrecognized
// statements classify as OpOther (HOST-3 / ADR-015).
type OpKind string

const (
	OpCreateTable  OpKind = "create_table"
	OpAddColumn    OpKind = "add_column"
	OpCreateIndex  OpKind = "create_index"
	OpDropColumn   OpKind = "drop_column"
	OpDropTable    OpKind = "drop_table"
	OpDropIndex    OpKind = "drop_index"
	OpRenameTable  OpKind = "rename_table"
	OpRenameColumn OpKind = "rename_column"
	OpAlterColumn  OpKind = "alter_column"
	OpOther        OpKind = "other"
)

// Safety classifies whether an operation can destroy data.
type Safety string

const (
	SafetyNonDestructive Safety = "non_destructive"
	SafetyDataLoss       Safety = "data_loss"
)

// Operation is one classified statement from a migration.
type Operation struct {
	Kind     OpKind `json:"kind"`
	Resource string `json:"resource,omitempty"`
	Column   string `json:"column,omitempty"`
	Safety   Safety `json:"safety"`
}

var (
	reCreateTable = regexp.MustCompile(`(?is)^\s*CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?(\S+)`)
	reCreateIndex = regexp.MustCompile(`(?is)^\s*CREATE\s+(?:UNIQUE\s+)?INDEX\s+(?:IF\s+NOT\s+EXISTS\s+)?(\S+)`)
	reDropTable   = regexp.MustCompile(`(?is)^\s*DROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?(\S+)`)
	reDropIndex   = regexp.MustCompile(`(?is)^\s*DROP\s+INDEX\s+(?:IF\s+EXISTS\s+)?(\S+)`)
	reAlterTable  = regexp.MustCompile(`(?is)^\s*ALTER\s+TABLE\s+(\S+)\s+(.*)$`)
	reDeleteData  = regexp.MustCompile(`(?is)^\s*(DELETE\s+FROM|TRUNCATE)\b`)

	reDropColumn   = regexp.MustCompile(`(?is)\bDROP\s+COLUMN\s+(\S+)`)
	reDropBare     = regexp.MustCompile(`(?is)\bDROP\s+(\S+)`)
	reDropKeyword  = regexp.MustCompile(`(?is)\bDROP\s+(CONSTRAINT|INDEX|KEY|PRIMARY\s+KEY|FOREIGN\s+KEY)\b`)
	reAddColumn    = regexp.MustCompile(`(?is)\bADD\s+(?:COLUMN\s+)?(\S+)`)
	reAddKeyword   = regexp.MustCompile(`(?is)\bADD\s+(CONSTRAINT|INDEX|KEY|PRIMARY\s+KEY|FOREIGN\s+KEY|UNIQUE)\b`)
	reRenameColumn = regexp.MustCompile(`(?is)\bRENAME\s+COLUMN\s+(\S+)`)
	reRenameTo     = regexp.MustCompile(`(?is)\bRENAME\s+(?:TO\s+)?(\S+)`)
	reAlterColumn  = regexp.MustCompile(`(?is)\b(?:ALTER\s+COLUMN|MODIFY(?:\s+COLUMN)?|CHANGE(?:\s+COLUMN)?)\s+(\S+)`)
)

// Classify parses migration SQL into a closed set of operations. It is a
// statement-level DDL classifier (not a full SQL parser): it strips comments,
// splits on ';', and matches each statement's leading DDL. It handles the
// Postgres ("), MySQL (`) and SQLite quoting Atlas emits. Its purpose is safety
// classification — flagging data loss — so when in doubt it errs toward
// data_loss rather than declaring a statement safe.
func Classify(sql string) []Operation {
	ops := []Operation{}
	for _, stmt := range splitStatements(sql) {
		if op, ok := classifyStatement(stmt); ok {
			ops = append(ops, op)
		}
	}
	return ops
}

func classifyStatement(stmt string) (Operation, bool) {
	switch {
	case reCreateTable.MatchString(stmt):
		return Operation{Kind: OpCreateTable, Resource: ident(reCreateTable, stmt), Safety: SafetyNonDestructive}, true
	case reCreateIndex.MatchString(stmt):
		return Operation{Kind: OpCreateIndex, Resource: ident(reCreateIndex, stmt), Safety: SafetyNonDestructive}, true
	case reDropTable.MatchString(stmt):
		return Operation{Kind: OpDropTable, Resource: ident(reDropTable, stmt), Safety: SafetyDataLoss}, true
	case reDropIndex.MatchString(stmt):
		return Operation{Kind: OpDropIndex, Resource: ident(reDropIndex, stmt), Safety: SafetyNonDestructive}, true
	case reAlterTable.MatchString(stmt):
		return classifyAlter(stmt), true
	case reDeleteData.MatchString(stmt):
		return Operation{Kind: OpOther, Safety: SafetyDataLoss}, true
	default:
		return Operation{Kind: OpOther, Safety: SafetyNonDestructive}, true
	}
}

// classifyAlter classifies the alteration in an ALTER TABLE statement. Atlas
// emits one action per ALTER, so a single operation per statement is reported.
func classifyAlter(stmt string) Operation {
	m := reAlterTable.FindStringSubmatch(stmt)
	table := clean(m[1])
	body := m[2]

	switch {
	case reDropColumn.MatchString(body):
		return Operation{Kind: OpDropColumn, Resource: table, Column: ident(reDropColumn, body), Safety: SafetyDataLoss}
	case reAlterColumn.MatchString(body):
		return Operation{Kind: OpAlterColumn, Resource: table, Column: ident(reAlterColumn, body), Safety: SafetyDataLoss}
	case reRenameColumn.MatchString(body):
		return Operation{Kind: OpRenameColumn, Resource: table, Column: ident(reRenameColumn, body), Safety: SafetyNonDestructive}
	case reAddKeyword.MatchString(body):
		return Operation{Kind: OpOther, Resource: table, Safety: SafetyNonDestructive}
	case reAddColumn.MatchString(body):
		return Operation{Kind: OpAddColumn, Resource: table, Column: ident(reAddColumn, body), Safety: SafetyNonDestructive}
	case reDropKeyword.MatchString(body):
		// DROP CONSTRAINT / INDEX / KEY — schema change, not row data loss.
		return Operation{Kind: OpOther, Resource: table, Safety: SafetyNonDestructive}
	case reDropBare.MatchString(body):
		// A bare `DROP <ident>` that is not a known keyword drops a column.
		return Operation{Kind: OpDropColumn, Resource: table, Column: ident(reDropBare, body), Safety: SafetyDataLoss}
	case strings.Contains(strings.ToUpper(body), "RENAME"):
		return Operation{Kind: OpRenameTable, Resource: table, Column: ident(reRenameTo, body), Safety: SafetyNonDestructive}
	default:
		return Operation{Kind: OpOther, Resource: table, Safety: SafetyNonDestructive}
	}
}

// splitStatements strips comments and splits SQL into trimmed, non-empty
// statements on ';'.
func splitStatements(sql string) []string {
	var b strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	var out []string
	for _, stmt := range strings.Split(b.String(), ";") {
		if s := strings.TrimSpace(stmt); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ident returns the cleaned first captured identifier of re against s.
func ident(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	return clean(m[1])
}

// clean strips quoting ("`[]), a trailing "(", and any schema prefix from an
// identifier token.
func clean(tok string) string {
	tok = strings.TrimSpace(tok)
	tok = strings.TrimSuffix(tok, "(")
	if i := strings.LastIndexAny(tok, ".") + 1; i > 0 && i < len(tok) {
		// keep the part after a schema/table qualifier only when it is not
		// itself quoted away below
		tok = tok[i:]
	}
	tok = strings.Trim(tok, "\"`[]")
	return tok
}
