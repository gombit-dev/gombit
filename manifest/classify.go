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
	// Additive/replaceable object creations, and read-only or metadata
	// statements, that add rather than destroy.
	reCreateSafe = regexp.MustCompile(`(?is)^\s*CREATE\s+(?:OR\s+REPLACE\s+)?(?:MATERIALIZED\s+)?(VIEW|SEQUENCE|FUNCTION|PROCEDURE|TRIGGER|EXTENSION|SCHEMA|TYPE|DOMAIN|ROLE|AGGREGATE|OPERATOR)\b`)
	reSafeStmt   = regexp.MustCompile(`(?is)^\s*(INSERT\s|SET\s|COMMENT\s+ON\b|GRANT\s|REVOKE\s|PRAGMA\s|BEGIN\b|COMMIT\b|SAVEPOINT\s|RELEASE\s|ANALYZE\b|VACUUM\b|REINDEX\b)`)

	reDropTable  = regexp.MustCompile(`(?is)^\s*DROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?(\S+)`)
	reDropIndex  = regexp.MustCompile(`(?is)^\s*DROP\s+INDEX\s+(?:IF\s+EXISTS\s+)?(\S+)`)
	reDropAny    = regexp.MustCompile(`(?is)^\s*DROP\b`)
	reAlterTable = regexp.MustCompile(`(?is)^\s*ALTER\s+TABLE\s+(\S+)\s+(.*)$`)
	// Statements that change or remove row data.
	reDataChange = regexp.MustCompile(`(?is)^\s*(UPDATE\s|DELETE\b|TRUNCATE\b|MERGE\s|REPLACE\s)`)

	reDropColumn   = regexp.MustCompile(`(?is)\bDROP\s+COLUMN\s+(\S+)`)
	reDropBare     = regexp.MustCompile(`(?is)\bDROP\s+(\S+)`)
	reAddColumn    = regexp.MustCompile(`(?is)\bADD\s+(?:COLUMN\s+)?(\S+)`)
	reAddKeyword   = regexp.MustCompile(`(?is)\bADD\s+(CONSTRAINT|INDEX|KEY|PRIMARY\s+KEY|FOREIGN\s+KEY|UNIQUE)\b`)
	reRenameColumn = regexp.MustCompile(`(?is)\bRENAME\s+COLUMN\s+(\S+)`)
	reRenameTo     = regexp.MustCompile(`(?is)\bRENAME\s+(?:TO\s+)?(\S+)`)
	reAlterColumn  = regexp.MustCompile(`(?is)\b(?:ALTER\s+COLUMN|MODIFY(?:\s+COLUMN)?|CHANGE(?:\s+COLUMN)?)\s+(\S+)`)
)

// dropKeywords are the tokens after a bare ALTER … DROP that are NOT a column,
// so dropping them is a schema change, not row data loss. Anything else after a
// bare DROP (including PARTITION, which carries data) is treated as a column
// drop. COLUMN itself is handled by reDropColumn before the bare check.
var dropKeywords = map[string]bool{
	"CONSTRAINT": true, "INDEX": true, "KEY": true,
	"PRIMARY": true, "FOREIGN": true, "UNIQUE": true, "CHECK": true,
}

// Classify parses migration SQL into a closed set of operations. It is a
// statement-level DDL classifier (not a full SQL parser): it strips comments,
// splits on ';', and matches each statement's leading DDL. It handles the
// Postgres ("), MySQL (`) and SQLite quoting Atlas emits.
//
// Its purpose is safety classification, so it is deliberately fail-safe: only
// recognized additive/metadata statements are non_destructive; anything it does
// not positively recognize as safe — including UPDATE, unrecognized DROPs, and
// statements it cannot parse — classifies as data_loss so a host reviews it.
func Classify(sql string) []Operation {
	ops := []Operation{}
	for _, stmt := range splitStatements(sql) {
		ops = append(ops, classifyStatement(stmt))
	}
	return ops
}

func classifyStatement(stmt string) Operation {
	switch {
	case reCreateTable.MatchString(stmt):
		return Operation{Kind: OpCreateTable, Resource: ident(reCreateTable, stmt), Safety: SafetyNonDestructive}
	case reCreateIndex.MatchString(stmt):
		return Operation{Kind: OpCreateIndex, Resource: ident(reCreateIndex, stmt), Safety: SafetyNonDestructive}
	case reCreateSafe.MatchString(stmt):
		return Operation{Kind: OpOther, Safety: SafetyNonDestructive}
	case reDropTable.MatchString(stmt):
		return Operation{Kind: OpDropTable, Resource: ident(reDropTable, stmt), Safety: SafetyDataLoss}
	case reDropIndex.MatchString(stmt):
		return Operation{Kind: OpDropIndex, Resource: ident(reDropIndex, stmt), Safety: SafetyNonDestructive}
	case reDropAny.MatchString(stmt):
		// DROP SCHEMA / DATABASE / VIEW / SEQUENCE / … — destructive by default.
		return Operation{Kind: OpOther, Safety: SafetyDataLoss}
	case reAlterTable.MatchString(stmt):
		return classifyAlter(stmt)
	case reDataChange.MatchString(stmt):
		return Operation{Kind: OpOther, Safety: SafetyDataLoss}
	case reSafeStmt.MatchString(stmt):
		return Operation{Kind: OpOther, Safety: SafetyNonDestructive}
	default:
		// Fail-safe: an unrecognized statement is flagged for review, not
		// silently stamped safe.
		return Operation{Kind: OpOther, Safety: SafetyDataLoss}
	}
}

// classifyAlter classifies an ALTER TABLE statement, scanning the whole body so
// a destructive action is caught even in a multi-action ALTER
// (e.g. ADD a, DROP b). Destructive actions win.
func classifyAlter(stmt string) Operation {
	m := reAlterTable.FindStringSubmatch(stmt)
	table := clean(m[1])
	body := m[2]

	if col, ok := droppedColumn(body); ok {
		return Operation{Kind: OpDropColumn, Resource: table, Column: col, Safety: SafetyDataLoss}
	}
	if reAlterColumn.MatchString(body) {
		return Operation{Kind: OpAlterColumn, Resource: table, Column: ident(reAlterColumn, body), Safety: SafetyDataLoss}
	}
	if reRenameColumn.MatchString(body) {
		return Operation{Kind: OpRenameColumn, Resource: table, Column: ident(reRenameColumn, body), Safety: SafetyNonDestructive}
	}
	if reAddKeyword.MatchString(body) {
		return Operation{Kind: OpOther, Resource: table, Safety: SafetyNonDestructive}
	}
	if reAddColumn.MatchString(body) {
		return Operation{Kind: OpAddColumn, Resource: table, Column: ident(reAddColumn, body), Safety: SafetyNonDestructive}
	}
	if strings.Contains(strings.ToUpper(body), "RENAME") {
		return Operation{Kind: OpRenameTable, Resource: table, Column: ident(reRenameTo, body), Safety: SafetyNonDestructive}
	}
	// Remaining ALTER actions (ADD/DROP CONSTRAINT, SET/OWNER/ENABLE, …) are
	// additive or metadata — non-destructive. The destructive ALTER actions are
	// the finite set handled above.
	return Operation{Kind: OpOther, Resource: table, Safety: SafetyNonDestructive}
}

// droppedColumn returns the name of a column dropped by the ALTER body, if any.
// It recognizes DROP COLUMN and a bare DROP <ident> that is not a keyword
// (CONSTRAINT / INDEX / KEY / …), and scans every DROP in the body.
func droppedColumn(body string) (string, bool) {
	if m := reDropColumn.FindStringSubmatch(body); m != nil {
		return clean(m[1]), true
	}
	for _, m := range reDropBare.FindAllStringSubmatch(body, -1) {
		if dropKeywords[strings.ToUpper(clean(m[1]))] {
			continue
		}
		return clean(m[1]), true
	}
	return "", false
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
