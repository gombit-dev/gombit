package database

import (
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/gombit-dev/gombit/types"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

var (
	rangeTimeType     = reflect.TypeOf(time.Time{})
	rangeNullTimeType = reflect.TypeOf(sql.NullTime{})
	rangeDateType     = reflect.TypeOf(types.Date{})
)

// registerTimeRangeCallback refuses, before the SQL runs on every create and
// update, a timestamp or date outside what every supported database can store
// and return (issue #443): types.TimeWithin / types.DateWithin. PostgreSQL
// stored year 0 as 1 BC, after which every read of the row failed to encode,
// and MySQL refused it. The failure is a *ValidationError naming the field, so
// MapPersistError answers it with a D10 422 on the generated API, the admin
// data plane and any other write alike: the bound has one home, like Validate.
//
// The check is keyed on the assignment set, the columns a statement actually
// writes: a create's rows (or map), an update's map or the struct it was given
// (matched to the model's columns by name, any struct type), filtered by
// Select/Omit as GORM filters them. An update's model is checked only when it
// is that struct (Save, Updates(&row)); otherwise it holds the row's old
// values, which the statement does not write, and refusing them would block
// every other change to a row that already holds one.
func registerTimeRangeCallback(db *gorm.DB) error {
	create := db.Callback().Create().Before("gorm:create")
	if err := create.Register("gombit:timerange", func(tx *gorm.DB) { runTimeRangeCheck(tx, true) }); err != nil {
		return fmt.Errorf("database: register create time range callback: %w", err)
	}
	update := db.Callback().Update().Before("gorm:update")
	if err := update.Register("gombit:timerange", func(tx *gorm.DB) { runTimeRangeCheck(tx, false) }); err != nil {
		return fmt.Errorf("database: register update time range callback: %w", err)
	}
	return nil
}

func runTimeRangeCheck(db *gorm.DB, creating bool) {
	if db.Error != nil || db.Statement == nil || db.Statement.Schema == nil {
		return
	}
	targets := rangedTimeFields(db.Statement.Schema)
	if len(targets) == 0 {
		return
	}
	skipHooks := db.Statement.SkipHooks
	fields := map[string][]string{}
	forEachAssigned(db, creating, targets, func(a assignedValue) {
		f := a.Field
		// GORM writes now over an auto-update timestamp on a struct update
		// that runs hooks, whatever the struct holds.
		if !a.Creating && a.FromStruct && !skipHooks && f.AutoUpdateTime > 0 {
			return
		}
		msg := timeRangeProblem(f, a.Value, zeroIsWritten(a))
		if msg == "" {
			return
		}
		key := fieldKey(f)
		for _, have := range fields[key] {
			if have == msg {
				return
			}
		}
		fields[key] = append(fields[key], msg)
	})
	if len(fields) > 0 {
		_ = db.AddError(NewValidationError("The request contains invalid fields.", fields))
	}
}

// zeroIsWritten reports whether a zero time in a is what reaches the column,
// as GORM decides it. A struct field's zero is not written on an update unless
// the column is selected, and on a create GORM fills an auto timestamp or
// leaves a column with a default to the database. Anything the statement
// names explicitly (a map, an upsert assignment, a Select) is written as is.
func zeroIsWritten(a assignedValue) bool {
	if a.Explicit || !a.FromStruct {
		return true
	}
	if !a.Creating {
		return false
	}
	f := a.Field
	return f.AutoCreateTime == 0 && f.AutoUpdateTime == 0 && !f.HasDefaultValue
}

// rangedFieldsBySchema caches each schema's ranged columns, so a model with
// none skips the check without reflecting over anything.
var rangedFieldsBySchema sync.Map // *schema.Schema -> map[string]*schema.Field

func rangedTimeFields(sch *schema.Schema) map[string]*schema.Field {
	if cached, ok := rangedFieldsBySchema.Load(sch); ok {
		return cached.(map[string]*schema.Field)
	}
	out := map[string]*schema.Field{}
	for _, f := range sch.Fields {
		if isRangedTimeField(f) {
			out[f.DBName] = f
		}
	}
	rangedFieldsBySchema.Store(sch, out)
	return out
}

// isRangedTimeField reports whether f is a stored time.Time, sql.NullTime or
// types.Date column, pointer or not. Auto create/update timestamps are
// included: GORM fills them only when they are zero, and keeps a value a
// caller sets (a hook, a seeder, a map update naming updated_at).
func isRangedTimeField(f *schema.Field) bool {
	if f.DBName == "" {
		return false
	}
	t := f.FieldType
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t == rangeTimeType || t == rangeNullTimeType || t == rangeDateType
}

// timeRangeProblem describes why v, assigned to column, cannot be stored and
// returned on every driver, or returns "" when it can. A string headed for the
// column is read as the drivers read it (RFC 3339, "YYYY-MM-DD hh:mm:ss",
// "YYYY-MM-DD"); one that does not parse is refused too, since PostgreSQL
// accepts forms ('infinity', '... BC') no Go time can be read back from.
// Expressions arrive as clause values, not strings, and are left to the
// database, as are NULL (a nil pointer, an invalid sql.NullTime) and, when
// zeroWritten is false, a zero value GORM will not write.
func timeRangeProblem(column *schema.Field, v reflect.Value, zeroWritten bool) string {
	for v.IsValid() && (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) {
		if v.IsNil() {
			return ""
		}
		v = v.Elem()
	}
	if !v.IsValid() || !v.CanInterface() {
		return ""
	}
	dateColumn := isRangedDateField(column)
	var t time.Time
	switch x := v.Interface().(type) {
	case time.Time:
		t = x
	case sql.NullTime:
		if !x.Valid {
			return ""
		}
		t = x.Time
	case types.Date:
		t = x.Time()
	case string, []byte:
		raw := fmt.Sprint(x)
		if b, ok := x.([]byte); ok {
			raw = string(b)
		}
		parsed, ok := parseAssignedTime(raw)
		if !ok {
			if dateColumn {
				return "must be a date (YYYY-MM-DD)"
			}
			return "must be an RFC 3339 timestamp"
		}
		t = parsed
	default:
		// A named string type (type Stamp string) is written as its string.
		if v.Kind() != reflect.String {
			return ""
		}
		parsed, ok := parseAssignedTime(v.String())
		if !ok {
			if dateColumn {
				return "must be a date (YYYY-MM-DD)"
			}
			return "must be an RFC 3339 timestamp"
		}
		t = parsed
	}
	if t.IsZero() && !zeroWritten {
		return ""
	}
	var err error
	if dateColumn {
		err = types.DateWithin(types.NewDate(t))
	} else {
		err = types.TimeWithin(t)
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

// parseAssignedTime reads a string assigned to a timestamp or date column in
// the forms the drivers accept.
func parseAssignedTime(s string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999", time.DateOnly} {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func isRangedDateField(f *schema.Field) bool {
	t := f.FieldType
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t == rangeDateType
}
