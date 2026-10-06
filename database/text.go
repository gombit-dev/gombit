package database

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// TextMaxBytes is what a `text` column holds on every supported driver:
// MySQL's TEXT, the smallest of the three (PostgreSQL and SQLite take more).
const TextMaxBytes = 65535

var (
	rangeNullStringType = reflect.TypeOf(sql.NullString{})
	valuerType          = reflect.TypeOf((*driver.Valuer)(nil)).Elem()
)

// TextProblem describes why s cannot be stored in, or compared against, a
// text column on every supported driver, or returns "" when it can:
// PostgreSQL refuses a NUL byte and invalid UTF-8 in text (SQLSTATE 22021),
// and MySQL refuses invalid UTF-8 in a utf8mb4 column. A filter or search
// term with either reaches the same refusal on PostgreSQL.
func TextProblem(s string) string {
	if strings.IndexByte(s, 0) >= 0 {
		return "must not contain a NUL character"
	}
	if !utf8.ValidString(s) {
		return "must be valid UTF-8"
	}
	return ""
}

// registerTextCallback refuses, before the SQL runs on every create and
// update, a string a text column cannot store on every supported driver
// (issue #444): a NUL byte or invalid UTF-8 (PostgreSQL refused it with a
// 500, SQLite stored it), or more than the column holds: n characters in a
// size:n / varchar(n) / char(n) column (PostgreSQL and MySQL refused more,
// SQLite stored it), TextMaxBytes in a `text` column (MySQL refused more).
// The failure is a *ValidationError naming the field, so MapPersistError
// answers it with a 422 on the generated API, the admin data plane and any
// other write alike. What a statement writes is forEachAssigned's
// assignment set, as for the time range check.
func registerTextCallback(db *gorm.DB) error {
	if err := db.Use(&textGuard{}); err != nil {
		return fmt.Errorf("database: register text callbacks: %w", err)
	}
	return nil
}

// textGuard is the gombit:text check as a GORM plugin, so its per-schema
// cache of text columns belongs to one *gorm.DB.
type textGuard struct {
	columns sync.Map // *schema.Schema -> map[string]*schema.Field
	limits  sync.Map // *schema.Field -> textLimit
}

func (*textGuard) Name() string { return "gombit:text" }

func (g *textGuard) Initialize(db *gorm.DB) error {
	create := db.Callback().Create().Before("gorm:create")
	if err := create.Register("gombit:text", func(tx *gorm.DB) { g.run(tx, true) }); err != nil {
		return fmt.Errorf("create: %w", err)
	}
	update := db.Callback().Update().Before("gorm:update")
	if err := update.Register("gombit:text", func(tx *gorm.DB) { g.run(tx, false) }); err != nil {
		return fmt.Errorf("update: %w", err)
	}
	return nil
}

func (g *textGuard) run(db *gorm.DB, creating bool) {
	if db.Error != nil || db.Statement == nil || db.Statement.Schema == nil {
		return
	}
	targets := g.textFields(db.Statement.Schema)
	if len(targets) == 0 {
		return
	}
	c := textCheck{guard: g}
	forEachAssigned(db, creating, targets, c.check)
	if len(c.fields) > 0 {
		_ = db.AddError(NewValidationError("The request contains invalid fields.", c.fields))
	}
}

type textCheck struct {
	guard  *textGuard
	fields map[string][]string // allocated on the first problem
}

func (c *textCheck) check(a assignedValue) {
	s, ok := textValue(a.Value)
	if !ok {
		return
	}
	msg := TextProblem(s)
	if msg == "" {
		msg = c.guard.limit(a.Field).problem(s)
	}
	if msg == "" {
		return
	}
	key := fieldKey(a.Field)
	if c.fields == nil {
		c.fields = map[string][]string{}
	}
	c.fields[key] = append(c.fields[key], msg)
}

// textFields returns sch's text columns by DBName, cached per schema.
func (g *textGuard) textFields(sch *schema.Schema) map[string]*schema.Field {
	if cached, ok := g.columns.Load(sch); ok {
		return cached.(map[string]*schema.Field)
	}
	out := map[string]*schema.Field{}
	for _, f := range sch.Fields {
		if isTextField(f) {
			out[f.DBName] = f
		}
	}
	actual, _ := g.columns.LoadOrStore(sch, out)
	return actual.(map[string]*schema.Field)
}

func (g *textGuard) limit(f *schema.Field) textLimit {
	if cached, ok := g.limits.Load(f); ok {
		return cached.(textLimit)
	}
	l := textLimitOf(f)
	g.limits.Store(f, l)
	return l
}

// isTextField reports a string column the Go value is written to as is: a
// string (or named string) field, a *string, or an sql.NullString. A field
// whose value is converted on the way (a GORM serializer, a type with its own
// driver.Valuer) writes something else, which is not checked, and nor is a
// column declared binary, which holds any byte.
func isTextField(f *schema.Field) bool {
	if f.DBName == "" || f.Serializer != nil {
		return false
	}
	t := f.FieldType
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t != rangeNullStringType && (t.Kind() != reflect.String ||
		t.Implements(valuerType) || reflect.PointerTo(t).Implements(valuerType)) {
		return false
	}
	declared := strings.ToLower(f.TagSettings["TYPE"])
	for _, binary := range []string{"blob", "bytea", "binary"} {
		if strings.Contains(declared, binary) {
			return false
		}
	}
	return true
}

// textValue reads the string v writes to a text column. A nil pointer, an
// invalid NullString and a value of another kind (an expression, a number)
// are not text, and are left to the database.
func textValue(v reflect.Value) (string, bool) {
	for v.IsValid() && (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) {
		if v.IsNil() {
			return "", false
		}
		v = v.Elem()
	}
	switch {
	case !v.IsValid():
		return "", false
	case v.Kind() == reflect.String:
		return v.String(), true
	case v.Type() == rangeNullStringType:
		ns := readValue[sql.NullString](v)
		return ns.String, ns.Valid
	}
	return "", false
}

// textLimit is what a text column holds on every supported driver: at most
// chars characters, or bytes bytes; zero is no limit.
type textLimit struct {
	chars, bytes int
}

func (l textLimit) problem(s string) string {
	switch {
	case l.bytes > 0 && len(s) > l.bytes:
		return "must be at most " + strconv.Itoa(l.bytes) + " bytes"
	case l.chars > 0 && len(s) > l.chars && utf8.RuneCountInString(s) > l.chars:
		return "must be at most " + strconv.Itoa(l.chars) + " characters"
	}
	return ""
}

// textLimitOf reads a column's capacity on every driver from its declared
// type, else its size, as GORM's DDL has it: varchar(n), char(n) and size:n
// hold n characters (PostgreSQL and MySQL count characters); text, tinytext
// and mediumtext hold MySQL's byte capacity. A string column with neither is
// text on PostgreSQL and SQLite and longtext on MySQL, except that MySQL makes
// it varchar(191) when it is a primary key, indexed, unique or defaulted,
// which then holds 191 characters everywhere.
func textLimitOf(f *schema.Field) textLimit {
	declared := strings.ToLower(strings.TrimSpace(f.TagSettings["TYPE"]))
	if declared == "" || declared == "string" {
		switch {
		case f.Size > 0 && f.Size < 1<<16:
			return textLimit{chars: f.Size}
		case f.Size == 0 && (f.PrimaryKey || f.HasDefaultValue || f.TagSettings["INDEX"] != "" || f.TagSettings["UNIQUE"] != ""):
			return textLimit{chars: 191}
		}
		return textLimit{}
	}
	switch declared {
	case "tinytext":
		return textLimit{bytes: 255}
	case "text":
		return textLimit{bytes: TextMaxBytes}
	case "mediumtext":
		return textLimit{bytes: 1<<24 - 1}
	}
	for _, prefix := range []string{"varchar(", "character varying(", "char(", "character(", "nvarchar(", "nchar("} {
		if rest, ok := strings.CutPrefix(declared, prefix); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(strings.TrimSuffix(rest, ")"))); err == nil && n > 0 {
				return textLimit{chars: n}
			}
		}
	}
	return textLimit{}
}
