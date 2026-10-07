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
	"gorm.io/gorm/clause"
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

type rangedNullable struct {
	ID  uint         `gorm:"primaryKey"`
	Opt *time.Time   `gorm:"default:null"`
	NT  sql.NullTime `gorm:"default:null"`
}

type rangedDefault struct {
	ID   uint `gorm:"primaryKey"`
	Name string
	At   time.Time `gorm:"default:CURRENT_TIMESTAMP"`
}

// What GORM does not write is left alone: a nil pointer or an invalid
// sql.NullTime (NULL), a zero auto create/update timestamp (GORM fills it), a
// zero column with a default (the database fills it), a zero struct field on
// an update (not written), and an auto-update timestamp on a struct update
// that runs hooks (GORM writes now over it). In-range values on every path are
// written. The integration tests run it on PostgreSQL and MySQL.
func TestTimeRangeLeavesUnsetAndInRangeValuesAlone(t *testing.T) {
	testTimeRangeUnsetAndInRange(t, openRangedDB(t))
}

func testTimeRangeUnsetAndInRange(t *testing.T, db *DB) {
	t.Helper()
	row := rangedEvent{Name: "unset", Due: inRange, Issued: dateFine}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("Create with the optional times unset: %v", err)
	}
	if row.CreatedAt.IsZero() || row.UpdatedAt.IsZero() {
		t.Fatal("GORM's CreatedAt/UpdatedAt were not filled")
	}
	// The column's default is spelled per driver (MySQL wants the precision of
	// a datetime(3) repeated in CURRENT_TIMESTAMP); the model's default tag is
	// what tells GORM to leave a zero At to it.
	_ = db.Migrator().DropTable(&rangedDefault{})
	ddl := map[Driver]string{
		DriverSQLite:   "CREATE TABLE ranged_defaults (id integer PRIMARY KEY, name text, at datetime DEFAULT CURRENT_TIMESTAMP)",
		DriverPostgres: "CREATE TABLE ranged_defaults (id serial PRIMARY KEY, name text, at timestamptz DEFAULT now())",
		DriverMySQL:    "CREATE TABLE ranged_defaults (id bigint AUTO_INCREMENT PRIMARY KEY, name varchar(64), at datetime(3) DEFAULT CURRENT_TIMESTAMP(3))",
	}[db.Driver()]
	if err := db.Exec(ddl).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Migrator().DropTable(&rangedDefault{}) })
	defaulted := rangedDefault{Name: "defaulted"}
	if err := db.Create(&defaulted).Error; err != nil {
		t.Errorf("a zero column with a default: %v", err)
	}
	// The default only fills a create; an update naming the column writes
	// the zero instant itself.
	wantRangeError(t, "Select(At) Updates, zero", db.Model(&defaulted).Select("At").Updates(rangedDefault{}).Error, "at")
	var before rangedDefault
	if err := db.First(&before, defaulted.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Save(&rangedDefault{ID: defaulted.ID, Name: "saved"}).Error; err != nil {
		t.Errorf("Save, zero defaulted column: %v", err)
	}
	var after rangedDefault
	if err := db.First(&after, defaulted.ID).Error; err != nil || after.Name != "saved" || !after.At.Equal(before.At) {
		t.Errorf("Save, zero defaulted column: %v, stored %+v, want At kept as %v", err, after, before.At)
	}
	// A zero an edit of the loaded row writes over its real value is
	// refused, not left out (ScopeEdit).
	editStored := StoredValues(db.DB, &after)
	edit := after
	edit.Name, edit.At = "edited", time.Time{}
	wantRangeError(t, "Save, zero over a real value in a defaulted column", ScopeEdit(db.DB, &edit, editStored).Save(&edit).Error, "at")

	// NULL is a value, not an unset column: Save clears a defaulted nullable
	// column (#562 round 5).
	_ = db.Migrator().DropTable(&rangedNullable{})
	if err := db.AutoMigrate(&rangedNullable{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Migrator().DropTable(&rangedNullable{}) })
	set := inRange
	nullable := rangedNullable{Opt: &set, NT: sql.NullTime{Time: inRange, Valid: true}}
	if err := db.Create(&nullable).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Save(&rangedNullable{ID: nullable.ID}).Error; err != nil {
		t.Errorf("Save clearing defaulted nullable columns: %v", err)
	}
	var cleared rangedNullable
	if err := db.First(&cleared, nullable.ID).Error; err != nil || cleared.Opt != nil || cleared.NT.Valid {
		t.Errorf("Save clearing defaulted nullable columns: %v, stored %+v, want NULLs", err, cleared)
	}

	paid := inRange
	legacy := row
	legacy.UpdatedAt = yearZero // GORM overwrites it with now on this Save
	for what, err := range map[string]error{
		"Updates(map)":              db.Model(&row).Updates(map[string]any{"due": inRange, "issued": dateFine}).Error,
		"Update(column)":            db.Model(&row).Update("paid", &paid).Error,
		"Update(column, ISO local)": db.Model(&row).Update("due", "2026-10-04T12:00:00").Error,
		// Save selects every column, so it writes CreatedAt as given.
		"Save":                       db.Save(&rangedEvent{ID: row.ID, Name: "saved", Due: inRange, Issued: dateFine, Noted: sql.NullTime{Time: inRange, Valid: true}, CreatedAt: row.CreatedAt}).Error,
		"Save over an old UpdatedAt": db.Save(&legacy).Error,
		"Updates(struct, zero due)":  db.Model(&row).Updates(rangedEvent{Name: "zero due is not written"}).Error,
		"NULL":                       db.Model(&row).Updates(map[string]any{"paid": nil, "noted": sql.NullTime{}}).Error,
		"expression":                 db.Model(&row).Update("due", gorm.Expr("due")).Error,
		// GORM fills a zero auto timestamp and the database a zero defaulted
		// column on a create, whatever Select says.
		"Select(*) Create":            db.Select("*").Create(&rangedEvent{Name: "star", Due: inRange, Issued: dateFine}).Error,
		"Select(CreatedAt) Create":    db.Select("Name", "Due", "Issued", "CreatedAt").Create(&rangedEvent{Name: "named", Due: inRange, Issued: dateFine}).Error,
		"Select(*) Create, defaulted": db.Select("*").Create(&rangedDefault{Name: "star default"}).Error,
	} {
		if err != nil {
			t.Errorf("%s: %v", what, err)
		}
	}
	// The text forms PostgreSQL prints (and SQLite stores as given) pass the
	// check; MySQL's DATETIME refuses offsets and zone names itself.
	if db.Driver() != DriverMySQL {
		for _, form := range []string{"2026-10-04 12:00:00+00", "2026-10-04 12:00:00+0000", "2026-10-04 12:00:00-03", "2026-10-04 12:00:00 UTC"} {
			if err := db.Model(&row).Update("due", form).Error; err != nil {
				t.Errorf("Update(due, %q): %v", form, err)
			}
		}
	}
	for _, form := range []string{"2026-10-04 12:00:00+00", "2026-10-04 12:00:00-0700", "2026-10-04 12:00:00 UTC"} {
		if _, ok := parseAssignedTime(form); !ok {
			t.Errorf("parseAssignedTime(%q) failed", form)
		}
	}
}

// What GORM does write is checked, the zero instant and caller-set auto
// timestamps included (#562 review round 2): 0001-01-01T00:00:00Z is not a
// stored "unset" but a value (MySQL refused it with a 500, the others stored
// it), and a hook or seeder setting CreatedAt/UpdatedAt to year 0 stored the
// row-breaking value. An upsert's DO UPDATE literals are written too, and a
// string no Go time parses (Postgres's 'infinity') is refused, since the row
// could not be read back.
func TestTimeRangeChecksWhatGORMWrites(t *testing.T) {
	testTimeRangeWhatGORMWrites(t, openRangedDB(t))
}

type softEvent struct {
	ID        uint `gorm:"primaryKey"`
	Name      string
	DeletedAt gorm.DeletedAt
}

// Time columns GORM's field permissions keep out of a statement: read-only
// (filled by the database), create-only, update-only.
type permEvent struct {
	ID       uint `gorm:"primaryKey"`
	Name     string
	Computed time.Time `gorm:"->"`
	Pub      time.Time `gorm:"<-:create"`
	Edited   time.Time `gorm:"<-:update"`
}

func testTimeRangeWhatGORMWrites(t *testing.T, db *DB) {
	t.Helper()
	// A column GORM never writes is not checked (#562 round 6): a zero
	// read-only or update-only column on a create, a zero create-only one
	// on an update. Where GORM does write it, a zero is refused.
	_ = db.Migrator().DropTable(&permEvent{})
	if err := db.Exec(map[Driver]string{
		DriverSQLite:   "CREATE TABLE perm_events (id integer PRIMARY KEY, name text, computed datetime, pub datetime, edited datetime)",
		DriverPostgres: "CREATE TABLE perm_events (id serial PRIMARY KEY, name text, computed timestamptz, pub timestamptz, edited timestamptz)",
		DriverMySQL:    "CREATE TABLE perm_events (id bigint AUTO_INCREMENT PRIMARY KEY, name varchar(64), computed datetime(3) NULL, pub datetime(3) NULL, edited datetime(3) NULL)",
	}[db.Driver()]).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Migrator().DropTable(&permEvent{}) })
	perm := permEvent{Name: "p", Pub: inRange}
	if err := db.Create(&perm).Error; err != nil {
		t.Errorf("Create, zero read-only and update-only columns: %v", err)
	}
	perm.Pub = time.Time{}
	perm.Edited = inRange
	if err := db.Save(&perm).Error; err != nil {
		t.Errorf("Save, zero create-only and read-only columns: %v", err)
	}
	wantRangeError(t, "Create, zero create-only column", db.Create(&permEvent{Name: "z"}).Error, "pub")
	wantRangeError(t, "Save, zero update-only column", db.Save(&permEvent{ID: perm.ID, Name: "z", Pub: inRange}).Error, "edited")

	_ = db.Migrator().DropTable(&softEvent{})
	if err := db.AutoMigrate(&softEvent{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Migrator().DropTable(&softEvent{}) })
	row := rangedEvent{Name: "base", Due: inRange, Issued: dateFine}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	wantRangeError(t, "Create zero due", db.Create(&rangedEvent{Name: "z", Issued: dateFine}).Error, "due")
	wantRangeError(t, "Update(column, zero instant)", db.Model(&row).Update("due", "0001-01-01T00:00:00Z").Error, "due")
	wantRangeError(t, "Updates(map, zero time)", db.Model(&row).Updates(map[string]any{"due": time.Time{}}).Error, "due")
	wantRangeError(t, "Select zero due", db.Model(&row).Select("due").Updates(rangedEvent{}).Error, "due")
	wantRangeError(t, "Create CreatedAt year 0", db.Create(&rangedEvent{Name: "c", Due: inRange, Issued: dateFine, CreatedAt: yearZero}).Error, "created_at")
	wantRangeError(t, "Updates(map created_at)", db.Model(&row).Updates(map[string]any{"created_at": yearZero}).Error, "created_at")
	wantRangeError(t, "Updates(map updated_at)", db.Model(&row).Updates(map[string]any{"updated_at": yearZero}).Error, "updated_at")
	wantRangeError(t, "UpdateColumns(struct UpdatedAt)", db.Model(&row).UpdateColumns(rangedEvent{UpdatedAt: yearZero}).Error, "updated_at")
	wantRangeError(t, "upsert DO UPDATE", db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.Assignments(map[string]any{"due": yearZero}),
	}).Create(&rangedEvent{ID: row.ID, Name: "upsert", Due: inRange, Issued: dateFine}).Error, "due")
	// On a create GORM always writes an auto timestamp, Select or not, so a
	// caller-set one is checked even when Select leaves it out (#562 round 3).
	wantRangeError(t, "Select-restricted Create, CreatedAt year 0", db.Select("Name", "Due", "Issued").Create(&rangedEvent{Name: "sel", Due: inRange, Issued: dateFine, CreatedAt: yearZero}).Error, "created_at")
	// An update that names the column writes the zero instant over the row's,
	// however Select spells it (GORM's own select map decides, #562 round 5).
	wantRangeError(t, "Select(CreatedAt) Updates, zero", db.Model(&row).Select("CreatedAt").Updates(rangedEvent{}).Error, "created_at")
	for _, sel := range []string{"ranged_events.due", "`due`", "ranged_events.*"} {
		wantRangeError(t, "Select("+sel+") Updates, zero", db.Model(&row).Select(sel).Updates(rangedEvent{Name: "q"}).Error, "due")
	}
	// Save (Select("*")) of a struct that leaves CreatedAt unset keeps the
	// row's: the zero is left out of the UPDATE rather than written over it
	// or refused, and when no row matches, Save's insert fallback fills it.
	if err := db.Save(&rangedEvent{ID: row.ID, Name: "s", Due: inRange, Issued: dateFine}).Error; err != nil {
		t.Errorf("Save, zero CreatedAt: %v", err)
	}
	upserted := rangedEvent{ID: row.ID + 777777, Name: "new by key", Due: inRange, Issued: dateFine}
	if err := db.Save(&upserted).Error; err != nil {
		t.Errorf("Save of a new row by key, zero CreatedAt: %v", err)
	}
	var inserted rangedEvent
	if err := db.First(&inserted, upserted.ID).Error; err != nil || inserted.CreatedAt.Year() < 2000 {
		t.Errorf("Save's insert fallback: %v, created_at %v", err, inserted.CreatedAt)
	}
	// GORM skips a struct field only when reflect calls it zero, so a zero
	// instant in any other form is written, and refused: a non-pointer time
	// in a Location (what pgx and time.Parse return), a non-nil pointer to
	// the zero time, a valid NullTime holding it, an auto timestamp a caller
	// set to it.
	zero := time.Time{}
	wantRangeError(t, "Updates(struct), zero in Local", db.Model(&row).Updates(rangedEvent{Due: zero.Local()}).Error, "due")
	wantRangeError(t, "Updates(struct), pointer to zero", db.Model(&row).Updates(rangedEvent{Paid: &zero}).Error, "paid")
	wantRangeError(t, "Updates(struct), valid zero NullTime", db.Model(&row).Updates(rangedEvent{Noted: sql.NullTime{Valid: true}}).Error, "noted")
	wantRangeError(t, "Create CreatedAt zero in Local", db.Create(&rangedEvent{Name: "l", Due: inRange, Issued: dateFine, CreatedAt: zero.Local()}).Error, "created_at")
	wantRangeError(t, "Create DeletedAt year 0", db.Create(&softEvent{Name: "s", DeletedAt: gorm.DeletedAt{Time: yearZero, Valid: true}}).Error, "deleted_at")
	soft := softEvent{Name: "soft"}
	if err := db.Create(&soft).Error; err != nil {
		t.Fatal(err)
	}
	wantRangeError(t, "Unscoped Update(deleted_at) year 0", db.Unscoped().Model(&soft).Update("deleted_at", yearZero).Error, "deleted_at")
	wantRangeError(t, "Update(column, infinity)", db.Model(&row).Update("due", "infinity").Error, "due")
	wantRangeError(t, "Update(column, BC)", db.Model(&row).Update("issued", "0001-01-01 BC").Error, "issued_on")

	var stored rangedEvent
	if err := db.First(&stored, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !stored.Due.Equal(inRange) || stored.CreatedAt.Year() < 2000 || stored.UpdatedAt.Year() < 2000 {
		t.Fatalf("a refused write changed the row: %+v", stored)
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

// A row stored before this check with the zero instant in a non-pointer
// column (what an unset field became on SQLite and PostgreSQL; MySQL refused
// it) keeps its other columns editable through writes that leave the column
// out: Updates of a struct skips the zero field, and the admin data plane
// omits a date column the request does not set. A write of the whole row
// (Save, Select("*").Updates) writes the zero instant back and is refused on
// every driver: the column needs a NULL (make the field a pointer) or a real
// value first (#562 round 4).
func TestTimeRangeStoredZeroRows(t *testing.T) {
	testTimeRangeStoredZero(t, openRangedDB(t))
}

func testTimeRangeStoredZero(t *testing.T, db *DB) {
	t.Helper()
	row := rangedEvent{Name: "legacy", Due: inRange, Issued: dateFine}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("UPDATE ranged_events SET due = ? WHERE id = ?", time.Time{}, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	var loaded rangedEvent
	if err := db.First(&loaded, row.ID).Error; err != nil || !loaded.Due.IsZero() {
		t.Fatalf("fixture: %v, due %v", err, loaded.Due)
	}
	// GORM's own choice decides what Updates(&loaded) writes: SQLite returns
	// the stored zero as time.Time{}, which it skips, and pgx returns it in
	// Local, which it writes back (and is refused). A partial struct or a
	// column update leaves the column out on every driver.
	if err := db.Model(&loaded).Updates(rangedEvent{Name: "renamed"}).Error; err != nil {
		t.Errorf("Updates(struct) of a row storing the zero instant: %v", err)
	}
	if err := db.Model(&loaded).Update("name", "renamed again").Error; err != nil {
		t.Errorf("Update(column) of a row storing the zero instant: %v", err)
	}
	whole := loaded
	whole.Name = "whole"
	if err := db.Model(&whole).Updates(&whole).Error; db.Driver() == DriverPostgres {
		wantRangeError(t, "Updates(&row) on PostgreSQL", err, "due")
	} else if err != nil {
		t.Errorf("Updates(&row), zero skipped by GORM: %v", err)
	}
	loaded.Name = "saved"
	wantRangeError(t, "Save of a row storing the zero instant", db.Save(&loaded).Error, "due")
	wantRangeError(t, "Select(*).Updates of a row storing the zero instant", db.Select("*").Updates(&loaded).Error, "due")
	wantRangeError(t, "Update(column, zero)", db.Model(&loaded).Update("due", time.Time{}).Error, "due")
	wantRangeError(t, "Select(due) zero", db.Model(&loaded).Select("due").Updates(rangedEvent{}).Error, "due")
	// ScopeEdit (what the admin's PATCH uses) leaves out a column that
	// still holds the stored zero after the hooks, for that model only, and
	// one the edit sets to the zero instant is refused.
	kept := StoredZeroColumns(db.DB, &loaded)
	if len(kept) != 1 || kept[0] != "due" {
		t.Fatalf("StoredZeroColumns = %v, want [due]", kept)
	}
	stored := StoredValues(db.DB, &loaded)
	loaded.Name = "kept"
	if err := ScopeEdit(db.DB, &loaded, stored).Save(&loaded).Error; err != nil {
		t.Errorf("Save keeping the stored zero: %v", err)
	}
	// The deprecated KeepStoredZeros still scopes the same way.
	if err := KeepStoredZeros(db.DB, &loaded, kept, nil).Save(&loaded).Error; err != nil {
		t.Errorf("KeepStoredZeros: %v", err)
	}
	q := ScopeEdit(db.DB, &loaded, stored).Model(&loaded).Select("*")
	if err := q.Updates(&loaded).Error; err != nil {
		t.Errorf("Select(*).Updates keeping the stored zero: %v", err)
	}
	if len(q.Statement.Omits) != 0 {
		t.Errorf("the left-out column stayed in the chain's Omits: %v", q.Statement.Omits)
	}
	wantRangeError(t, "ScopeEdit for another model", ScopeEdit(db.DB, &rangedDefault{}, stored).Save(&loaded).Error, "due")
	// The scope is the row passed to it: another row written on the same DB
	// with a zero set on purpose is refused (#562 round 6).
	other := rangedEvent{Name: "other", Due: inRange, Issued: dateFine}
	if err := db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	other.Due = time.Time{}
	wantRangeError(t, "ScopeEdit for another row", ScopeEdit(db.DB, &loaded, stored).Save(&other).Error, "due")
	var reread rangedEvent
	if err := db.First(&reread, loaded.ID).Error; err != nil || reread.Name != "kept" || !reread.Due.IsZero() {
		t.Fatalf("kept save: %v, stored %+v", err, reread)
	}
	// The documented repair: give the column a real value, after which Save
	// writes the row again.
	if err := db.Model(&loaded).Update("due", inRange).Error; err != nil {
		t.Fatal(err)
	}
	loaded.Due = inRange
	if err := db.Save(&loaded).Error; err != nil {
		t.Errorf("Save after the repair: %v", err)
	}
}

type plainRow struct {
	ID   uint `gorm:"primaryKey"`
	Name string
	Qty  int
}

// A model with no timestamp or date column costs the check nothing: its
// targets are cached per schema and the walk returns before reading anything.
func TestTimeRangeCostsNothingWithoutRangedColumns(t *testing.T) {
	db := openRangedDB(t)
	if err := db.AutoMigrate(&plainRow{}); err != nil {
		t.Fatal(err)
	}
	row := plainRow{Name: "x", Qty: 1}
	tx := db.Model(&row)
	if err := tx.Statement.Parse(&row); err != nil {
		t.Fatal(err)
	}
	tx.Statement.Dest = &row
	g := &timeRangeGuard{}
	g.run(tx, true) // warm the per-schema cache
	if allocs := testing.AllocsPerRun(100, func() { g.run(tx, true) }); allocs != 0 {
		t.Fatalf("runTimeRangeCheck on a model without ranged columns allocates %.0f times", allocs)
	}
}

type stampString string

// An upsert's DO UPDATE that refers to the inserted row (UpdateAll,
// AssignmentColumns: excluded.x) carries no value of its own; the row is
// checked as the Dest. A named string type is read as its string.
func TestTimeRangeUpsertReferencesAndNamedStrings(t *testing.T) {
	db := openRangedDB(t)
	row := rangedEvent{Name: "base", Due: inRange, Issued: dateFine}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	upsert := rangedEvent{ID: row.ID, Name: "upserted", Due: inRange, Issued: dateFine, CreatedAt: row.CreatedAt}
	if err := db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&upsert).Error; err != nil {
		t.Fatalf("UpdateAll upsert of an in-range row: %v", err)
	}
	if err := db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"due", "name"}),
	}).Create(&rangedEvent{ID: row.ID, Name: "again", Due: inRange, Issued: dateFine}).Error; err != nil {
		t.Fatalf("AssignmentColumns upsert of an in-range row: %v", err)
	}
	wantRangeError(t, "named string", db.Model(&row).Update("due", stampString("0000-01-01T00:00:00Z")).Error, "due")
	if err := db.Model(&row).Update("due", stampString("2026-10-04T00:00:00Z")).Error; err != nil {
		t.Errorf("an in-range named string: %v", err)
	}
}

type allocStamped struct {
	ID        uint `gorm:"primaryKey"`
	Name      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// The check adds no allocation to an ordinary write of a model whose only
// time columns are GORM's timestamps, which is nearly every model (#562
// round 3): the walker lives on the stack, reads fields in place, and builds
// nothing unless a value is out of range.
func TestTimeRangeAddsNoAllocationsToOrdinaryWrites(t *testing.T) {
	open := func(withCheck bool) *DB {
		db, err := Open(config.DatabaseConfig{Driver: config.DatabaseDriverSQLite, DSN: "file:" + filepath.Join(t.TempDir(), "alloc.db")})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if !withCheck {
			_ = db.Callback().Create().Remove("gombit:timerange")
			_ = db.Callback().Update().Remove("gombit:timerange")
		}
		if err := db.AutoMigrate(&allocStamped{}); err != nil {
			t.Fatal(err)
		}
		return db
	}
	measure := func(db *DB) map[string]float64 {
		row := allocStamped{Name: "x"}
		db.Create(&row)
		return map[string]float64{
			"Create":          testing.AllocsPerRun(50, func() { db.Create(&allocStamped{Name: "x"}) }),
			"Save":            testing.AllocsPerRun(50, func() { db.Save(&row) }),
			"Updates(map)":    testing.AllocsPerRun(50, func() { db.Model(&row).Updates(map[string]any{"name": "y"}) }),
			"Updates(struct)": testing.AllocsPerRun(50, func() { db.Model(&row).Updates(allocStamped{Name: "z"}) }),
		}
	}
	without, with := measure(open(false)), measure(open(true))
	for op, base := range without {
		if with[op] > base+0.5 {
			t.Errorf("%s: %.1f allocs with the check, %.1f without", op, with[op], base)
		}
	}
}
