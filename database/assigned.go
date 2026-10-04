package database

import (
	"context"
	"reflect"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

// assignedValue is one value a create or update statement writes to a column,
// as forEachAssigned finds it.
type assignedValue struct {
	Field *schema.Field
	Value reflect.Value
	// Creating is true on a create (or the update half of an upsert's insert).
	Creating bool
	// Explicit is true when the statement names the column: a map key, an
	// upsert assignment, or a Select. A struct field is not explicit: GORM
	// writes it on update only when it is non-zero, and on create fills an
	// auto create/update timestamp, or leaves a defaulted column to the
	// database, when it is zero.
	Explicit bool
	// FromStruct is true when the value comes from a struct (the model, a
	// batch element, or an Updates(struct) Dest), not a map or a clause.
	FromStruct bool
}

// forEachAssigned calls fn for every value the statement in db writes to one
// of targets, the columns a write-path callback (gombit:timerange) checks. It
// is the one definition of "what this statement writes":
//
//   - the Dest: a create's rows (struct, batch) or map, an update's map
//     (Updates(map), Update(column, value)) or struct (Save, Updates(struct),
//     UpdateColumns), any struct type, matched to the model's columns by name;
//   - on a create, the literal assignments of an upsert's ON CONFLICT DO
//     UPDATE (a column reference such as excluded.x, or an expression, is no
//     value: the inserted row it refers to is checked as the Dest);
//   - filtered by Select/Omit as GORM filters them.
//
// An update's model is walked only when it is the Dest (Save, Updates(&row));
// otherwise it holds the row's old values, which the statement does not write.
//
// targets are the model's columns the caller checks, keyed by DBName and
// cached per schema by the caller; a model with none costs nothing here.
// Only target fields are read.
func forEachAssigned(db *gorm.DB, creating bool, targets map[string]*schema.Field, fn func(assignedValue)) {
	stmt := db.Statement
	if stmt == nil || stmt.Schema == nil || len(targets) == 0 {
		return
	}
	ctx := stmt.Context
	if ctx == nil {
		ctx = context.Background()
	}
	selected, restricted := stmt.SelectAndOmitColumns(creating, !creating)
	written := func(column string) (bool, bool) {
		if v, ok := selected[column]; ok {
			return v, v
		}
		return !restricted, false
	}
	target := func(name string) *schema.Field {
		if f, ok := targets[name]; ok {
			return f
		}
		if f := stmt.Schema.LookUpField(name); f != nil {
			return targets[f.DBName]
		}
		return nil
	}
	emit := func(f *schema.Field, v reflect.Value, explicit, fromStruct bool) {
		if f == nil {
			return
		}
		ok, chosen := written(f.DBName)
		if !ok {
			return
		}
		fn(assignedValue{Field: f, Value: v, Creating: creating, Explicit: explicit || chosen, FromStruct: fromStruct})
	}

	walkStruct := func(rv reflect.Value) {
		if !rv.CanAddr() {
			copied := reflect.New(rv.Type()).Elem()
			copied.Set(rv)
			rv = copied
		}
		if rv.Type() == stmt.Schema.ModelType {
			for _, f := range targets {
				emit(f, f.ReflectValueOf(ctx, rv), false, true)
			}
			return
		}
		// Another struct type: parsed through the DB's own schema cache and
		// naming strategy, and matched to the targets by column name.
		parse := &gorm.Statement{DB: db}
		if err := parse.Parse(rv.Addr().Interface()); err != nil || parse.Schema == nil {
			return
		}
		for dbName, f := range targets {
			if src := parse.Schema.LookUpField(dbName); src != nil && src.DBName == dbName {
				emit(f, src.ReflectValueOf(ctx, rv), false, true)
			}
		}
	}
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		for v.IsValid() && (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) {
			if v.IsNil() {
				return
			}
			v = v.Elem()
		}
		switch v.Kind() {
		case reflect.Map:
			if m, ok := v.Interface().(map[string]any); ok {
				for key, value := range m {
					emit(target(key), reflect.ValueOf(value), true, false)
				}
			}
		case reflect.Slice, reflect.Array:
			for i := 0; i < v.Len(); i++ {
				walk(v.Index(i))
			}
		case reflect.Struct:
			walkStruct(v)
		}
	}
	walk(reflect.ValueOf(stmt.Dest))

	if creating {
		if c, ok := stmt.Clauses["ON CONFLICT"]; ok {
			if onConflict, ok := c.Expression.(clause.OnConflict); ok {
				for _, a := range onConflict.DoUpdates {
					switch a.Value.(type) {
					case clause.Column, clause.Expression:
						continue
					}
					if f := target(a.Column.Name); f != nil {
						fn(assignedValue{Field: f, Value: reflect.ValueOf(a.Value), Explicit: true})
					}
				}
			}
		}
	}
}

// fieldKey names f in a ValidationError the way the API names it: its JSON
// name, else its column.
func fieldKey(f *schema.Field) string {
	if name, _, _ := strings.Cut(f.Tag.Get("json"), ","); name != "" && name != "-" {
		return name
	}
	return f.DBName
}
