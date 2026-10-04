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
	// Creating is true on a create (and for an upsert's DO UPDATE literal).
	Creating bool
	// FromStruct is true when the value comes from a struct (the model, a batch
	// element, an Updates(struct) Dest); false for a map value or an upsert
	// literal, which the statement names explicitly and writes as given.
	FromStruct bool
	// Named is true when Select names the column itself, not through "*"
	// (Save and the admin data plane select "*" to write the whole row).
	Named bool
}

// forEachAssigned calls fn for every value the statement in db writes to one
// of targets, the columns a write-path callback (gombit:timerange) checks. It
// is the one definition of "what this statement writes", following GORM's own
// column choice (callbacks/create.go and update.go):
//
//   - the Dest: a create's rows (struct, batch) or map, an update's map
//     (Updates(map), Update(column, value)) or struct (Save, Updates(struct),
//     UpdateColumns), any struct type, matched to the model's columns by name;
//   - on a create, the literal assignments of an upsert's ON CONFLICT DO
//     UPDATE (a column reference such as excluded.x, or an expression, is no
//     value: the inserted row it refers to is checked as the Dest);
//   - filtered by Select/Omit as GORM filters them; on a create, an auto
//     create/update timestamp column is always in the INSERT, as GORM has it.
//
// An update's model is walked only when it is the Dest (Save, Updates(&row));
// otherwise it holds the row's old values, which the statement does not write.
// Whether a zero struct value is actually written (GORM fills a zero auto
// timestamp, the database a zero defaulted column) is the caller's to decide
// from the assignedValue.
//
// targets are the model's columns the caller checks, keyed by DBName and
// cached per schema by the caller; a model with none costs nothing here. Only
// target fields are read.
func forEachAssigned(db *gorm.DB, creating bool, targets map[string]*schema.Field, fn func(assignedValue)) {
	stmt := db.Statement
	if stmt == nil || stmt.Schema == nil || len(targets) == 0 {
		return
	}
	w := assignWalker{db: db, stmt: stmt, ctx: stmt.Context, targets: targets, creating: creating}
	if w.ctx == nil {
		w.ctx = context.Background()
	}
	// Select("*") alone (what Save sends) restricts nothing.
	selectsAll := len(stmt.Selects) == 1 && stmt.Selects[0] == "*"
	if len(stmt.Omits) > 0 || (len(stmt.Selects) > 0 && !selectsAll) {
		w.selected, w.restricted = stmt.SelectAndOmitColumns(creating, !creating)
	}
	w.walk(reflect.ValueOf(stmt.Dest), fn)
	if creating {
		w.upsertLiterals(fn)
	}
}

type assignWalker struct {
	db         *gorm.DB
	stmt       *gorm.Statement
	ctx        context.Context
	targets    map[string]*schema.Field
	selected   map[string]bool
	restricted bool
	creating   bool
}

// written reports whether GORM writes f's column.
func (w *assignWalker) written(f *schema.Field) bool {
	if v, ok := w.selected[f.DBName]; ok {
		return v
	}
	return !w.restricted || (w.creating && (f.AutoCreateTime > 0 || f.AutoUpdateTime > 0))
}

// named reports whether Select names f's column itself.
func (w *assignWalker) named(f *schema.Field) bool {
	for _, s := range w.stmt.Selects {
		if s == f.DBName || s == f.Name {
			return true
		}
	}
	return false
}

func (w *assignWalker) emit(f *schema.Field, v reflect.Value, fromStruct bool, fn func(assignedValue)) {
	if f == nil {
		return
	}
	if !w.written(f) {
		return
	}
	fn(assignedValue{Field: f, Value: v, Creating: w.creating, FromStruct: fromStruct, Named: fromStruct && w.named(f)})
}

func (w *assignWalker) target(name string) *schema.Field {
	if f, ok := w.targets[name]; ok {
		return f
	}
	if f := w.stmt.Schema.LookUpField(name); f != nil {
		return w.targets[f.DBName]
	}
	return nil
}

func (w *assignWalker) walk(v reflect.Value, fn func(assignedValue)) {
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
				w.emit(w.target(key), reflect.ValueOf(value), false, fn)
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			w.walk(v.Index(i), fn)
		}
	case reflect.Struct:
		w.walkStruct(v, fn)
	}
}

func (w *assignWalker) walkStruct(rv reflect.Value, fn func(assignedValue)) {
	if rv.Type() == w.stmt.Schema.ModelType {
		for _, f := range w.targets {
			w.emit(f, f.ReflectValueOf(w.ctx, rv), true, fn)
		}
		return
	}
	// Another struct type: parsed through the DB's own schema cache and naming
	// strategy, and matched to the targets by column name.
	if !rv.CanAddr() {
		copied := reflect.New(rv.Type()).Elem()
		copied.Set(rv)
		rv = copied
	}
	parse := &gorm.Statement{DB: w.db}
	if err := parse.Parse(rv.Addr().Interface()); err != nil || parse.Schema == nil {
		return
	}
	for dbName, f := range w.targets {
		if src := parse.Schema.LookUpField(dbName); src != nil && src.DBName == dbName {
			w.emit(f, src.ReflectValueOf(w.ctx, rv), true, fn)
		}
	}
}

func (w *assignWalker) upsertLiterals(fn func(assignedValue)) {
	c, ok := w.stmt.Clauses["ON CONFLICT"]
	if !ok {
		return
	}
	onConflict, ok := c.Expression.(clause.OnConflict)
	if !ok {
		return
	}
	for _, a := range onConflict.DoUpdates {
		switch a.Value.(type) {
		case clause.Column, clause.Expression:
			continue
		}
		if f := w.target(a.Column.Name); f != nil {
			fn(assignedValue{Field: f, Value: reflect.ValueOf(a.Value), Creating: true})
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
