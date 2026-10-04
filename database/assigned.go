package database

import (
	"context"
	"reflect"
	"strings"
	"sync"

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

// assignedDestSchemas caches the parsed schema of an Updates(struct) Dest
// whose type is not the model's, as GORM's own statement does.
var assignedDestSchemas sync.Map

// forEachAssigned calls fn for every value the statement in db writes, the
// assignment set the write-path callbacks (gombit:timerange) check. It is the
// one definition of "what this statement writes":
//
//   - the Dest: a create's rows (struct, batch) or map, an update's map
//     (Updates(map), Update(column, value)) or struct (Save, Updates(struct),
//     UpdateColumns), any struct type, matched to the model's columns by name;
//   - on a create, the literal assignments of an upsert's ON CONFLICT DO
//     UPDATE (Clauses(clause.OnConflict{DoUpdates: ...}));
//   - filtered by Select/Omit as GORM filters them.
//
// An update's model is walked only when it is the Dest (Save, Updates(&row));
// otherwise it holds the row's old values, which the statement does not write.
func forEachAssigned(db *gorm.DB, creating bool, fn func(assignedValue)) {
	stmt := db.Statement
	if stmt == nil || stmt.Schema == nil {
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
	emit := func(f *schema.Field, v reflect.Value, explicit, fromStruct bool) {
		if f == nil || f.DBName == "" {
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
		src := stmt.Schema
		if rv.Type() != stmt.Schema.ModelType {
			parsed, err := schema.Parse(rv.Addr().Interface(), &assignedDestSchemas, db.NamingStrategy)
			if err != nil {
				return
			}
			src = parsed
		}
		for _, f := range src.Fields {
			if f.DBName != "" {
				emit(stmt.Schema.LookUpField(f.DBName), f.ReflectValueOf(ctx, rv), false, true)
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
					emit(stmt.Schema.LookUpField(key), reflect.ValueOf(value), true, false)
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
					if f := stmt.Schema.LookUpField(a.Column.Name); f != nil && f.DBName != "" {
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
