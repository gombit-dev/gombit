package database

import (
	"context"
	"fmt"
	"reflect"

	"gorm.io/gorm"
)

// StoredRow is what a row loaded from the database stores in the columns the
// write checks judge: which timestamp and date columns hold the zero instant,
// and each text column's value. StoredValues takes it; ScopeEdit uses it so an
// edit of the row is judged on what it changes, not on what the row already
// held.
type StoredRow struct {
	zeroTimes []string              // ranged time columns holding the zero instant
	texts     map[string]storedText // text columns, as loaded
	complete  bool                  // taken by StoredValues: every checked column is known
	edited    []string              // the deprecated KeepStoredZeros: columns the edit names
	values    map[string]any        // every updatable column's value, as loaded
}

type storedText struct {
	s  string
	ok bool // false for NULL
}

// storesText reports whether the row stored exactly this text (or NULL) in
// column: a value the edit, and the model's hooks, left as it was.
func (r *StoredRow) storesText(column, s string, ok bool) bool {
	if r.texts == nil {
		return false
	}
	t, known := r.texts[column]
	return known && t.ok == ok && t.s == s
}

// StoredValues records what row, a model as loaded from db, stores in its
// columns, before an edit changes row. Pass it to ScopeEdit with the update
// that writes row back.
func StoredValues(db *gorm.DB, row any) StoredRow {
	out := StoredRow{complete: true, texts: map[string]storedText{}, values: map[string]any{}}
	rv := reflect.Indirect(reflect.ValueOf(row))
	if rv.Kind() != reflect.Struct {
		return out
	}
	stmt := &gorm.Statement{DB: db}
	if err := stmt.Parse(row); err != nil || stmt.Schema == nil {
		return out
	}
	ctx := context.Background()
	for _, f := range stmt.Schema.Fields {
		if f.DBName != "" && f.Updatable {
			out.values[f.DBName] = snapshotValue(f.ReflectValueOf(ctx, rv))
		}
		switch {
		case isRangedTimeField(f):
			if zeroInstant(f.ReflectValueOf(ctx, rv)) {
				out.zeroTimes = append(out.zeroTimes, f.DBName)
			}
		case isTextField(f):
			s, ok := textValue(f.ReflectValueOf(ctx, rv))
			out.texts[f.DBName] = storedText{s: s, ok: ok}
		}
	}
	return out
}

// ScopeEdit returns db scoped for an edit of row, a model loaded from the
// database whose stored values StoredValues recorded before the edit. An
// update that writes row back with Select("*") (Save, or
// Model(row).Select("*").Updates(row)) writes only the columns whose value the
// edit, or a model hook, changed, compared after the hooks run (and an
// auto-update timestamp, as GORM stamps it): the others keep what the row
// holds now, so a concurrent change to a column the edit did not touch is
// not reverted (issue #450). An update that changes nothing writes no column
// (but an auto-update timestamp, which GORM stamps). The write checks judge
// only what is written:
//
//   - text the row stores, unchanged, is not written, even if it predates the
//     text check (a NUL byte, more than the column now allows); changed text
//     is checked;
//   - a timestamp or date that held the zero instant, and still does, is not
//     written; one the edit or a hook gave a real value is written, and
//     checked. A zero instant written over a real value is refused, even in a
//     column Save would otherwise leave out unset (an auto timestamp, a
//     default).
//
// Use Updates. Save keeps GORM's fallback for an update that affected no row,
// an insert that writes (and checks) every column: it fires for a row deleted
// meanwhile, which comes back, and for an edit that changed nothing on a
// model without an auto-update timestamp, which writes the whole row. With
// Updates, RowsAffected 0 means the row is gone, or, as MySQL counts, matched
// but left unchanged. Any other write on the
// returned DB, another row included, is written and checked as usual. The
// admin data plane uses it for its PATCH.
func ScopeEdit(db *gorm.DB, row any, stored StoredRow) *gorm.DB {
	if rv := reflect.ValueOf(row); rv.Kind() != reflect.Pointer || rv.IsNil() {
		return db
	}
	return db.Set(editScopeKey, editScope{row: row, stored: stored})
}

// KeepStoredZeros scopes an edit of row that sets the edited columns, leaving
// out each kept column (StoredZeroColumns) that still holds the zero instant.
//
// Deprecated: use ScopeEdit with StoredValues, which also keeps the row's
// stored text editable and judges columns by whether their value changed.
func KeepStoredZeros(db *gorm.DB, row any, kept, edited []string) *gorm.DB {
	return ScopeEdit(db, row, StoredRow{zeroTimes: kept, edited: edited})
}

const editScopeKey = "gombit:edit_scope"

type editScope struct {
	row    any // the pointer the update writes
	stored StoredRow
}

// editScopeOf returns what ScopeEdit recorded, when stmt writes its row.
func editScopeOf(stmt *gorm.Statement) *StoredRow {
	v, ok := stmt.Settings.Load(editScopeKey)
	if !ok {
		return nil
	}
	if k, ok := v.(editScope); ok && stmt.Dest == k.row {
		return &k.stored
	}
	return nil
}

// snapshotValue copies v deeply enough that a later edit of the row, or a
// model hook changing a pointer's target, a slice, a map or a struct's field
// in place, does not change the copy too (issue #450's comparison would then
// see no change and drop the write).
func snapshotValue(v reflect.Value) any {
	return deepCopy(v).Interface()
}

// deepCopy copies pointers, slices, maps and the exported fields of structs
// recursively. Unexported fields are copied as they are (a time.Time's
// Location, a decimal's big.Int), which no hook edits in place.
func deepCopy(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		c := reflect.New(v.Type().Elem())
		c.Elem().Set(deepCopy(v.Elem()))
		return c
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		c := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			c.Index(i).Set(deepCopy(v.Index(i)))
		}
		return c
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		c := reflect.MakeMapWithSize(v.Type(), v.Len())
		iter := v.MapRange()
		for iter.Next() {
			c.SetMapIndex(iter.Key(), deepCopy(iter.Value()))
		}
		return c
	case reflect.Struct:
		c := reflect.New(v.Type()).Elem()
		c.Set(v)
		for i := 0; i < v.NumField(); i++ {
			if f := c.Field(i); f.CanSet() {
				f.Set(deepCopy(v.Field(i)))
			}
		}
		return c
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		c := reflect.New(v.Type()).Elem()
		c.Set(deepCopy(v.Elem()))
		return c
	}
	return v
}

// registerEditColumnsCallback narrows a scoped whole-row update to the columns
// the edit changed (ScopeEdit). It is registered before the write checks, so
// they judge only what is written.
func registerEditColumnsCallback(db *gorm.DB) error {
	narrow := db.Callback().Update().Before("gorm:update")
	if err := narrow.Register("gombit:edit_columns", narrowToChanged); err != nil {
		return fmt.Errorf("database: register edit columns callback: %w", err)
	}
	restore := db.Callback().Update().After("gorm:update")
	if err := restore.Register("gombit:edit_columns_restore", restoreSelects); err != nil {
		return fmt.Errorf("database: register edit columns restore: %w", err)
	}
	return nil
}

const savedSelectsKey = "gombit:edit_columns:selects"

type savedSelects struct{ selects []string }

// narrowToChanged replaces a scoped update's Select("*") with the columns
// whose value differs from what the row stored (StoredValues), after the
// model's hooks have run.
func narrowToChanged(db *gorm.DB) {
	stmt := db.Statement
	if db.Error != nil || stmt == nil || stmt.Schema == nil ||
		len(stmt.Selects) != 1 || stmt.Selects[0] != "*" {
		return
	}
	scope := editScopeOf(stmt)
	if scope == nil || scope.values == nil {
		return
	}
	rv := reflect.Indirect(reflect.ValueOf(stmt.Dest))
	if rv.Kind() != reflect.Struct {
		return
	}
	ctx := stmt.Context
	if ctx == nil {
		ctx = context.Background()
	}
	var changed []string
	for _, f := range stmt.Schema.Fields {
		if f.DBName == "" || !f.Updatable {
			continue
		}
		stored, known := scope.values[f.DBName]
		if known && reflect.DeepEqual(f.ReflectValueOf(ctx, rv).Interface(), stored) {
			continue
		}
		changed = append(changed, f.DBName)
	}
	if len(changed) == 0 {
		for _, pf := range stmt.Schema.PrimaryFields {
			changed = append(changed, pf.DBName)
		}
	}
	stmt.Settings.Store(savedSelectsKey, savedSelects{selects: stmt.Selects})
	stmt.Selects = changed
}

// restoreSelects puts back the Select narrowToChanged replaced, for a chain
// that is used again.
func restoreSelects(db *gorm.DB) {
	if db.Statement == nil {
		return
	}
	if v, ok := db.Statement.Settings.LoadAndDelete(savedSelectsKey); ok {
		db.Statement.Selects = v.(savedSelects).selects
	}
}
