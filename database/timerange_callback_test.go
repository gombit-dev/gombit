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
	if err := db.Create(&rangedDefault{Name: "defaulted"}).Error; err != nil {
		t.Errorf("a zero column with a default: %v", err)
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

func testTimeRangeWhatGORMWrites(t *testing.T, db *DB) {
	t.Helper()
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
// column (what an unset field became on SQLite and PostgreSQL) stays editable:
// Save and the admin data plane's Select("*").Updates write the whole row back,
// and the stored zero is not the caller's to fix (#562 round 3).
func TestTimeRangeKeepsRowsWithAStoredZeroEditable(t *testing.T) {
	db := openRangedDB(t)
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
	loaded.Name = "renamed"
	if err := db.Save(&loaded).Error; err != nil {
		t.Errorf("Save of a row storing the zero instant: %v", err)
	}
	loaded.Name = "renamed again"
	if err := db.Select("*").Updates(&loaded).Error; err != nil {
		t.Errorf("Select(*).Updates of a row storing the zero instant: %v", err)
	}
	// A new zero written on purpose is still refused.
	wantRangeError(t, "Update(column, zero)", db.Model(&loaded).Update("due", time.Time{}).Error, "due")
	wantRangeError(t, "Select(due) zero", db.Model(&loaded).Select("due").Updates(rangedEvent{}).Error, "due")
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
