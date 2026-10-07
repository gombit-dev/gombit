package database

import (
	"context"
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
// timestamp, date and text columns, before an edit changes row. Pass it to
// ScopeEdit with the update that writes row back.
func StoredValues(db *gorm.DB, row any) StoredRow {
	out := StoredRow{complete: true, texts: map[string]storedText{}}
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
// update that writes row back (Save, Updates of the row pointer) writes every
// column, the ones the edit left alone included, so the write checks judge a
// column only where the edit, or a model hook, changed it, compared after the
// hooks run:
//
//   - text the row stores, unchanged, is written back as it is, even if it
//     predates the text check (a NUL byte, more than the column now allows);
//     changed text is checked;
//   - a timestamp or date that held the zero instant, and still does, is left
//     out of the SET instead of refused; one the edit or a hook gave a real
//     value is written, and checked. A zero instant written over a real value
//     is refused, even in a column Save would otherwise leave out unset (an
//     auto timestamp, a default).
//
// Any other write on the returned DB, another row included, is checked as
// usual. The admin data plane uses it for its PATCH.
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
