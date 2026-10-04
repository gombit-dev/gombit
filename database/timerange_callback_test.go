package database

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/types"
	"gorm.io/gorm"
)

type rangedEvent struct {
	ID        uint `gorm:"primaryKey"`
	Name      string
	Due       time.Time
	Paid      *time.Time
	Noted     sql.NullTime
	Issued    types.Date `json:"issued_on"`
	Shipped   *types.Date
	CreatedAt time.Time
	UpdatedAt time.Time
}

var (
	yearZero = time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC)
	yearHuge = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	inRange  = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	dateZero = types.NewDate(time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC))
	dateFine = types.NewDate(inRange)
)

func openRangedDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(config.DatabaseConfig{
		Driver: config.DatabaseDriverSQLite,
		DSN:    "file:" + filepath.Join(t.TempDir(), "ranged.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	migrateRanged(t, db)
	return db
}

func migrateRanged(t *testing.T, db *DB) {
	t.Helper()
	_ = db.Migrator().DropTable(&rangedEvent{})
	if err := db.AutoMigrate(&rangedEvent{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Migrator().DropTable(&rangedEvent{}) })
}

// wantRangeError asserts err is the time-range ValidationError naming field,
// and that the API answers it with a D10 422 carrying that field.
func wantRangeError(t *testing.T, what string, err error, field string) {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("%s: err = %v, want a *ValidationError", what, err)
	}
	if len(ve.Fields[field]) == 0 {
		t.Fatalf("%s: fields = %v, want %q", what, ve.Fields, field)
	}
	var envelope *contract.ErrorEnvelope
	if !errors.As(MapPersistError(context.Background(), err, "conflict", "internal"), &envelope) ||
		envelope.GetStatus() != http.StatusUnprocessableEntity || len(envelope.Body.Fields[field]) == 0 {
		t.Fatalf("%s: MapPersistError = %+v, want a 422 on %q", what, envelope, field)
	}
}

// Every write path refuses a timestamp or date no supported driver can store
// and return (issue #443): Create, a batch, Save, Updates with a map or a
// struct, and Update of one column. Nothing is written. The integration tests
// run it on PostgreSQL and MySQL.
func TestTimeRangeIsEnforcedOnEveryWritePath(t *testing.T) {
	testTimeRangeWritePaths(t, openRangedDB(t))
}

func testTimeRangeWritePaths(t *testing.T, db *DB) {
	t.Helper()
	good := rangedEvent{Name: "ok", Due: inRange, Issued: dateFine}
	if err := db.Create(&good).Error; err != nil {
		t.Fatalf("in-range Create: %v", err)
	}

	wantRangeError(t, "Create due", db.Create(&rangedEvent{Name: "a", Due: yearZero, Issued: dateFine}).Error, "due")
	wantRangeError(t, "Create *time", db.Create(&rangedEvent{Name: "b", Due: inRange, Paid: &yearHuge, Issued: dateFine}).Error, "paid")
	wantRangeError(t, "Create NullTime", db.Create(&rangedEvent{Name: "c", Due: inRange, Noted: sql.NullTime{Time: yearZero, Valid: true}, Issued: dateFine}).Error, "noted")
	wantRangeError(t, "Create date (json name)", db.Create(&rangedEvent{Name: "d", Due: inRange, Issued: dateZero}).Error, "issued_on")
	wantRangeError(t, "Create *date", db.Create(&rangedEvent{Name: "e", Due: inRange, Issued: dateFine, Shipped: &dateZero}).Error, "shipped")
	wantRangeError(t, "batch", db.Create(&[]rangedEvent{{Name: "f", Due: inRange, Issued: dateFine}, {Name: "g", Due: yearHuge, Issued: dateFine}}).Error, "due")

	saved := good
	saved.Due = yearZero
	wantRangeError(t, "Save", db.Save(&saved).Error, "due")
	wantRangeError(t, "Updates(map)", db.Model(&good).Updates(map[string]any{"due": yearZero}).Error, "due")
	wantRangeError(t, "Updates(map by field name)", db.Model(&good).Updates(map[string]any{"Issued": dateZero}).Error, "issued_on")
	wantRangeError(t, "Updates(struct)", db.Model(&good).Updates(rangedEvent{Due: yearHuge}).Error, "due")
	wantRangeError(t, "Update(column)", db.Model(&good).Update("paid", yearZero).Error, "paid")

	var n int64
	db.Model(&rangedEvent{}).Count(&n)
	var stored rangedEvent
	if err := db.First(&stored, good.ID).Error; err != nil {
		t.Fatal(err)
	}
	if n != 1 || !stored.Due.Equal(inRange) || stored.Paid != nil {
		t.Fatalf("a refused write changed the table: %d rows, stored %+v", n, stored)
	}
}

// Unset values are left alone, as before: a zero time.Time is Go's "not set"
// (GORM fills the create/update timestamps after the check), and nil pointers
// and an invalid sql.NullTime are NULL. (A zero types.Date was never storable:
// its Value refuses it.) In-range values on every path are written.
func TestTimeRangeLeavesUnsetAndInRangeValuesAlone(t *testing.T) {
	db := openRangedDB(t)
	row := rangedEvent{Name: "unset", Issued: dateFine}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("Create with every time unset: %v", err)
	}
	if row.CreatedAt.IsZero() {
		t.Fatal("GORM's CreatedAt was not filled")
	}
	paid := inRange
	for what, err := range map[string]error{
		"Updates(map)":   db.Model(&row).Updates(map[string]any{"due": inRange, "issued": dateFine}).Error,
		"Update(column)": db.Model(&row).Update("paid", &paid).Error,
		"Save":           db.Save(&rangedEvent{ID: row.ID, Name: "saved", Due: inRange, Issued: dateFine, Noted: sql.NullTime{Time: inRange, Valid: true}}).Error,
		"NULL":           db.Model(&row).Updates(map[string]any{"paid": nil, "noted": sql.NullTime{}}).Error,
		"expression":     db.Model(&row).Update("due", gorm.Expr("due")).Error,
	} {
		if err != nil {
			t.Errorf("%s: %v", what, err)
		}
	}
}

// The check is keyed on what a statement writes, not on the values reachable
// from it (#560 review findings, applied here). An update's model holds the
// row's old values, so a row that already stores an out-of-range value (from
// before this check, or written by hand) can still have its other columns
// changed; Select/Omit decide what is written; a string bound for a timestamp
// or date column is read as the driver would read it; and a struct of another
// type passed to Updates is matched to the model's columns by name.
func TestTimeRangeChecksOnlyTheAssignmentSet(t *testing.T) {
	db := openRangedDB(t)
	row := rangedEvent{Name: "legacy", Due: inRange, Issued: dateFine}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	// A value stored before the check existed, bypassing the callbacks.
	if err := db.Exec("UPDATE ranged_events SET due = ? WHERE id = ?", "0000-01-01 00:00:00+00:00", row.ID).Error; err != nil {
		t.Fatal(err)
	}
	var legacy rangedEvent
	if err := db.First(&legacy, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	for what, err := range map[string]error{
		"Update(other column)":       db.Model(&legacy).Update("name", "renamed").Error,
		"Updates(map, other column)": db.Model(&legacy).Updates(map[string]any{"name": "again"}).Error,
		"Omit the bad column":        db.Model(&legacy).Omit("due").Updates(rangedEvent{Name: "omitted", Due: yearZero}).Error,
		"Select another column":      db.Model(&legacy).Select("name").Updates(rangedEvent{Name: "selected", Due: yearHuge}).Error,
		"Create omitting it":         db.Omit("due").Create(&rangedEvent{Name: "c", Due: yearZero, Issued: dateFine}).Error,
		"expression":                 db.Model(&legacy).Update("due", gorm.Expr("due")).Error,
	} {
		if err != nil {
			t.Errorf("%s: %v, want the write allowed", what, err)
		}
	}

	type dueOnly struct {
		Due time.Time
	}
	wantRangeError(t, "Update(column, string)", db.Model(&legacy).Update("due", "0000-01-01T00:00:00Z").Error, "due")
	wantRangeError(t, "Updates(map, string date)", db.Model(&legacy).Updates(map[string]any{"issued": "0999-12-31"}).Error, "issued_on")
	wantRangeError(t, "Updates(other struct)", db.Model(&legacy).Updates(dueOnly{Due: yearZero}).Error, "due")
	wantRangeError(t, "Select the bad column", db.Model(&legacy).Select("due").Updates(rangedEvent{Due: yearHuge}).Error, "due")
	if err := db.Model(&legacy).Update("due", "2026-10-04T00:00:00Z").Error; err != nil {
		t.Errorf("an in-range string: %v", err)
	}
}
