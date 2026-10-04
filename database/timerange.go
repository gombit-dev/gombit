package database

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
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
func registerTimeRangeCallback(db *gorm.DB) error {
	create := db.Callback().Create().Before("gorm:create")
	if err := create.Register("gombit:timerange", runTimeRangeCheck); err != nil {
		return fmt.Errorf("database: register create time range callback: %w", err)
	}
	update := db.Callback().Update().Before("gorm:update")
	if err := update.Register("gombit:timerange", runTimeRangeCheck); err != nil {
		return fmt.Errorf("database: register update time range callback: %w", err)
	}
	return nil
}

func runTimeRangeCheck(db *gorm.DB) {
	if db.Error != nil || db.Statement == nil || db.Statement.Schema == nil {
		return
	}
	stmt := db.Statement
	ctx := stmt.Context
	if ctx == nil {
		ctx = context.Background()
	}
	fields := map[string][]string{}
	add := func(f *schema.Field, msg string) {
		key := timeRangeFieldKey(f)
		for _, have := range fields[key] {
			if have == msg {
				return
			}
		}
		fields[key] = append(fields[key], msg)
	}

	checkStruct := func(rv reflect.Value) {
		for rv.Kind() == reflect.Pointer {
			if rv.IsNil() {
				return
			}
			rv = rv.Elem()
		}
		if rv.Kind() != reflect.Struct || rv.Type() != stmt.Schema.ModelType {
			return
		}
		for _, f := range stmt.Schema.Fields {
			if !isRangedTimeField(f) {
				continue
			}
			if msg := timeRangeProblem(f.ReflectValueOf(ctx, rv)); msg != "" {
				add(f, msg)
			}
		}
	}

	rv := stmt.ReflectValue
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		for i := 0; i < rv.Len(); i++ {
			checkStruct(rv.Index(i))
		}
	default:
		checkStruct(rv)
	}
	// Updates(map) and Update(column, value) carry the written values in Dest,
	// keyed by column or field name; Updates(struct) carries a struct there.
	switch dest := stmt.Dest.(type) {
	case map[string]any:
		for key, value := range dest {
			f := stmt.Schema.LookUpField(key)
			if f == nil || !isRangedTimeField(f) {
				continue
			}
			if msg := timeRangeProblem(reflect.ValueOf(value)); msg != "" {
				add(f, msg)
			}
		}
	default:
		if dv := reflect.ValueOf(stmt.Dest); dv.IsValid() {
			checkStruct(dv)
		}
	}

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

// timeRangeProblem describes why v cannot be stored and returned on every
// driver, or returns "" when it can. A zero value is Go's "not set" and is
// left to the column (NOT NULL, a default, or GORM) as before; so is an
// invalid sql.NullTime, which is NULL.
func timeRangeProblem(v reflect.Value) string {
	for v.IsValid() && (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) {
		if v.IsNil() {
			return ""
		}
		v = v.Elem()
	}
	if !v.IsValid() || !v.CanInterface() {
		return ""
	}
	var err error
	switch x := v.Interface().(type) {
	case time.Time:
		if !x.IsZero() {
			err = types.TimeWithin(x)
		}
	case sql.NullTime:
		if x.Valid && !x.Time.IsZero() {
			err = types.TimeWithin(x.Time)
		}
	case types.Date:
		if !x.IsZero() {
			err = types.DateWithin(x)
		}
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

// timeRangeFieldKey names f in a ValidationError the way the API names it:
// its JSON name, else its column.
func timeRangeFieldKey(f *schema.Field) string {
	if name, _, _ := strings.Cut(f.Tag.Get("json"), ","); name != "" && name != "-" {
		return name
	}
	return f.DBName
}
