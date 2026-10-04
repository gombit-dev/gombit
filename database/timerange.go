package database

import (
	"context"
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

// timeRangeDestSchemas caches the parsed schema of an Updates(struct) Dest
// whose type is not the model's.
var timeRangeDestSchemas sync.Map

func runTimeRangeCheck(db *gorm.DB, creating bool) {
	if db.Error != nil || db.Statement == nil || db.Statement.Schema == nil {
		return
	}
	stmt := db.Statement
	ctx := stmt.Context
	if ctx == nil {
		ctx = context.Background()
	}
	selected, restricted := stmt.SelectAndOmitColumns(creating, !creating)
	written := func(column string) bool {
		if v, ok := selected[column]; ok {
			return v
		}
		return !restricted
	}

	fields := map[string][]string{}
	check := func(column *schema.Field, value reflect.Value) {
		if column == nil || !isRangedTimeField(column) || !written(column.DBName) {
			return
		}
		msg := timeRangeProblem(column, value)
		if msg == "" {
			return
		}
		key := timeRangeFieldKey(column)
		for _, have := range fields[key] {
			if have == msg {
				return
			}
		}
		fields[key] = append(fields[key], msg)
	}
	checkStruct := func(rv reflect.Value) {
		if !rv.CanAddr() {
			copied := reflect.New(rv.Type()).Elem()
			copied.Set(rv)
			rv = copied
		}
		src := stmt.Schema
		if rv.Type() != stmt.Schema.ModelType {
			parsed, err := schema.Parse(rv.Addr().Interface(), &timeRangeDestSchemas, db.NamingStrategy)
			if err != nil {
				return
			}
			src = parsed
		}
		for _, f := range src.Fields {
			if f.DBName != "" {
				check(stmt.Schema.LookUpField(f.DBName), f.ReflectValueOf(ctx, rv))
			}
		}
	}
	checkMap := func(m map[string]any) {
		for key, value := range m {
			check(stmt.Schema.LookUpField(key), reflect.ValueOf(value))
		}
	}
	var checkAny func(v reflect.Value)
	checkAny = func(v reflect.Value) {
		for v.IsValid() && (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) {
			if v.IsNil() {
				return
			}
			v = v.Elem()
		}
		switch v.Kind() {
		case reflect.Map:
			if m, ok := v.Interface().(map[string]any); ok {
				checkMap(m)
			}
		case reflect.Slice, reflect.Array:
			for i := 0; i < v.Len(); i++ {
				checkAny(v.Index(i))
			}
		case reflect.Struct:
			checkStruct(v)
		}
	}
	// The Dest is what the statement writes: a create's rows or map, an
	// update's map (Updates(map), Update(column, value)) or struct (Save,
	// Updates(struct)).
	checkAny(reflect.ValueOf(stmt.Dest))

	if len(fields) > 0 {
		_ = db.AddError(NewValidationError("The request contains invalid fields.", fields))
	}
}

// isRangedTimeField reports whether f is a stored time.Time, sql.NullTime or
// types.Date column (pointer or not) that the application writes. GORM's
// auto-managed create/update timestamps are filled after this callback runs
// and are always "now", so they are left alone.
func isRangedTimeField(f *schema.Field) bool {
	if f.DBName == "" || f.AutoCreateTime > 0 || f.AutoUpdateTime > 0 {
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
// column is read as the driver would read it (RFC 3339, "YYYY-MM-DD hh:mm:ss",
// or "YYYY-MM-DD"); one that does not parse, an expression, and anything else
// is left to the database. A zero value is Go's "not set" and is left to the
// column (NOT NULL, a default, or GORM) as before; so is NULL.
func timeRangeProblem(column *schema.Field, v reflect.Value) string {
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
	case string:
		parsed, ok := parseAssignedTime(x)
		if !ok {
			return ""
		}
		t = parsed
	case []byte:
		parsed, ok := parseAssignedTime(string(x))
		if !ok {
			return ""
		}
		t = parsed
	default:
		return ""
	}
	if t.IsZero() {
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
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999", time.DateOnly} {
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

// timeRangeFieldKey names f in a ValidationError the way the API names it:
// its JSON name, else its column.
func timeRangeFieldKey(f *schema.Field) string {
	if name, _, _ := strings.Cut(f.Tag.Get("json"), ","); name != "" && name != "-" {
		return name
	}
	return f.DBName
}
