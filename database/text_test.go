package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/contract"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type textItem struct {
	ID    uint   `gorm:"primaryKey"`
	Name  string `gorm:"size:10"`
	Notes string `gorm:"type:text"`
	Code  *string
	Alias sql.NullString `gorm:"size:5"`
	Free  string
	Raw   []byte
	// Tag has no size but an index, so MySQL makes it varchar(191).
	Tag string `gorm:"index"`
	// Hex and Meta write something other than the Go string (a Valuer's
	// output, a serializer's JSON), which is not checked.
	Hex  hexText
	Meta string `gorm:"serializer:json"`
}

// hexText writes its string hex-encoded.
type hexText string

func (h hexText) Value() (driver.Value, error) { return hex.EncodeToString([]byte(h)), nil }

func (h *hexText) Scan(src any) error {
	var s string
	switch v := src.(type) {
	case string:
		s = v
	case []byte:
		s = string(v)
	}
	b, err := hex.DecodeString(s)
	*h = hexText(b)
	return err
}

func openTextDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(config.DatabaseConfig{
		Driver: config.DatabaseDriverSQLite,
		DSN:    "file:" + filepath.Join(t.TempDir(), "text.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func migrateText(t *testing.T, db *DB) {
	t.Helper()
	_ = db.Migrator().DropTable(&textItem{})
	if err := db.AutoMigrate(&textItem{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Migrator().DropTable(&textItem{}) })
}

// A string no supported driver stores as given is a 422 naming the field on
// every write path and every driver (issue #444): PostgreSQL refused a NUL
// byte or invalid UTF-8 with a 500 and SQLite stored it; MySQL refused text
// over 65,535 bytes with a 500 and the others stored it; PostgreSQL and MySQL
// refused more characters than varchar(n) holds and SQLite stored them.
func TestTextWritesAreRefusedWhereNoDriverStoresThem(t *testing.T) {
	db := openTextDB(t)
	testTextWrites(t, db)
}

func testTextWrites(t *testing.T, db *DB) {
	t.Helper()
	migrateText(t, db)
	row := textItem{Name: "ok", Notes: "n"}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	nul, badUTF8 := "a\x00b", "a\xffb"
	code := "c\x00"
	for what, c := range map[string]struct {
		err   error
		field string
	}{
		"Create NUL":                {db.Create(&textItem{Name: nul}).Error, "name"},
		"Create invalid UTF-8":      {db.Create(&textItem{Name: "x", Notes: badUTF8}).Error, "notes"},
		"Create NUL, unlimited":     {db.Create(&textItem{Name: "x", Free: nul}).Error, "free"},
		"Create NUL, *string":       {db.Create(&textItem{Name: "x", Code: &code}).Error, "code"},
		"Create over size:10":       {db.Create(&textItem{Name: "elevenchars"}).Error, "name"},
		"Create over text":          {db.Create(&textItem{Name: "x", Notes: strings.Repeat("a", TextMaxBytes+1)}).Error, "notes"},
		"Create NullString over":    {db.Create(&textItem{Name: "x", Alias: sql.NullString{String: "sixsix", Valid: true}}).Error, "alias"},
		"Updates(map) NUL":          {db.Model(&row).Updates(map[string]any{"name": nul}).Error, "name"},
		"Update(column) over text":  {db.Model(&row).Update("notes", strings.Repeat("é", TextMaxBytes/2+1)).Error, "notes"},
		"Updates(struct) NUL":       {db.Model(&row).Updates(textItem{Free: nul}).Error, "free"},
		"Save invalid UTF-8":        {db.Save(&textItem{ID: row.ID, Name: badUTF8}).Error, "name"},
		"upsert DO UPDATE NUL":      {db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, DoUpdates: clause.Assignments(map[string]any{"name": nul})}).Create(&textItem{ID: row.ID, Name: "u"}).Error, "name"},
		"Updates(map) over size:10": {db.Model(&row).Updates(map[string]any{"name": "elevenchars"}).Error, "name"},
		"Create indexed over 191":   {db.Create(&textItem{Name: "x", Tag: strings.Repeat("t", 192)}).Error, "tag"},
	} {
		wantTextError(t, what, c.err, c.field)
	}

	// What every driver stores is written: the capacity of varchar(n) is n
	// characters, not bytes; text holds 65,535 bytes; a column with no
	// declared size or type has no limit the drivers disagree on; NULL and
	// bytes are not text.
	ten := strings.Repeat("é", 10)
	full := textItem{
		Name:  ten,
		Notes: strings.Repeat("a", TextMaxBytes),
		Alias: sql.NullString{String: "five5", Valid: true},
		Free:  strings.Repeat("f", 70000),
		Raw:   []byte{0, 0xff},
		Tag:   strings.Repeat("t", 191),
		Hex:   hexText("a\x00b"),
		Meta:  "a\x00b",
	}
	if err := db.Create(&full).Error; err != nil {
		t.Fatalf("Create within every limit: %v", err)
	}
	var stored textItem
	if err := db.First(&stored, full.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Name != ten || len(stored.Notes) != TextMaxBytes || len(stored.Free) != 70000 || stored.Alias.String != "five5" ||
		len(stored.Tag) != 191 || stored.Hex != "a\x00b" || stored.Meta != "a\x00b" {
		t.Fatalf("stored name %q, notes %d bytes, free %d bytes, alias %q, tag %d, hex %q, meta %q",
			stored.Name, len(stored.Notes), len(stored.Free), stored.Alias.String, len(stored.Tag), stored.Hex, stored.Meta)
	}
	if err := db.Model(&stored).Updates(map[string]any{"code": nil, "alias": sql.NullString{}}).Error; err != nil {
		t.Errorf("NULLs: %v", err)
	}
	if err := db.Model(&stored).Update("name", "renamed").Error; err != nil {
		t.Errorf("an ordinary update: %v", err)
	}
}

func wantTextError(t *testing.T, what string, err error, field string) {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Fields[field]) == 0 {
		t.Errorf("%s: err = %v, want a *ValidationError on %q", what, err, field)
		return
	}
	var env *contract.ErrorEnvelope
	if !errors.As(MapPersistError(context.Background(), err, "conflict", "internal"), &env) || env.GetStatus() != http.StatusUnprocessableEntity {
		t.Errorf("%s: MapPersistError = %+v, want a 422", what, env)
	}
}

// TextProblem is the one rule for text a client sends: the write check,
// FilterEq, Search and the admin all apply it.
func TestTextProblem(t *testing.T) {
	for s, want := range map[string]bool{
		"":            true,
		"plain":       true,
		"ünïcødé ✓":   true,
		"a\x00b":      false,
		"\x00":        false,
		"a\xffb":      false,
		"\xc3\x28":    false, // truncated sequence
		"tab\tnew\nl": true,
	} {
		if got := TextProblem(s) == ""; got != want {
			t.Errorf("TextProblem(%q) ok = %v, want %v", s, got, want)
		}
	}
}

// textLimitOf reads what a column holds on every driver from its declared
// type, else its size.
func TestTextLimitOf(t *testing.T) {
	type limits struct {
		Plain    string
		Sized    string `gorm:"size:255"`
		Text     string `gorm:"type:text"`
		Tiny     string `gorm:"type:tinytext"`
		Medium   string `gorm:"type:mediumtext"`
		Long     string `gorm:"type:longtext"`
		Varchar  string `gorm:"type:varchar(40)"`
		Char     string `gorm:"type:char(36)"`
		VaryingN string `gorm:"type:character varying(12)"`
		Indexed  string `gorm:"index"`
		Unique   string `gorm:"unique"`
		Default  string `gorm:"default:x"`
		Key      string `gorm:"primaryKey"`
		Typed    string `gorm:"type:string;size:5"`
	}
	db := openTextDB(t)
	stmt := &gorm.Statement{DB: db.DB}
	if err := stmt.Parse(&limits{}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]textLimit{
		"Plain": {}, "Sized": {chars: 255}, "Text": {bytes: TextMaxBytes}, "Tiny": {bytes: 255},
		"Medium": {bytes: 1<<24 - 1}, "Long": {}, "Varchar": {chars: 40}, "Char": {chars: 36}, "VaryingN": {chars: 12},
		"Indexed": {chars: 191}, "Unique": {chars: 191}, "Default": {chars: 191}, "Key": {chars: 191}, "Typed": {chars: 5},
	} {
		if got := textLimitOf(stmt.Schema.LookUpField(name)); got != want {
			t.Errorf("%s: limit %+v, want %+v", name, got, want)
		}
	}
}

// A value the database itself refuses as data is the client's to fix: 422,
// keyed on the column when the driver names it (issue #444), on the write
// and the read path alike. A server fault stays a 500.
func TestDataExceptionsAreValidationErrors(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		err   error
		field string
	}{
		"pg numeric overflow":   {&pgconn.PgError{Code: "22003", Message: "numeric field overflow"}, ""},
		"pg NUL byte":           {&pgconn.PgError{Code: "22021", Message: `invalid byte sequence for encoding "UTF8": 0x00`}, ""},
		"pg value too long":     {&pgconn.PgError{Code: "22001", Message: "value too long for type character varying(10)"}, ""},
		"mysql out of range":    {&mysqldriver.MySQLError{Number: 1264, Message: "Out of range value for column 'amount' at row 1"}, "amount"},
		"mysql data too long":   {&mysqldriver.MySQLError{Number: 1406, Message: "Data too long for column 'notes' at row 1"}, "notes"},
		"mysql incorrect value": {&mysqldriver.MySQLError{Number: 1292, Message: "Incorrect datetime value: '0000-00-00' for column 'due' at row 1"}, "due"},
	} {
		for path, mapped := range map[string]error{
			"persist": MapPersistError(ctx, c.err, "conflict", "internal"),
			"load":    MapLoadError(ctx, c.err, "not found", "internal"),
		} {
			var env *contract.ErrorEnvelope
			if !errors.As(mapped, &env) || env.GetStatus() != http.StatusUnprocessableEntity {
				t.Errorf("%s (%s): %+v, want a 422", name, path, mapped)
				continue
			}
			if c.field != "" && len(env.Body.Fields[c.field]) == 0 {
				t.Errorf("%s (%s): fields %v, want %q", name, path, env.Body.Fields, c.field)
			}
		}
	}
	for name, err := range map[string]error{
		// Class 22 outside what a bound client value causes: server-side SQL.
		"pg division by zero":    &pgconn.PgError{Code: "22012"},
		"pg cast in server SQL":  &pgconn.PgError{Code: "22P02"},
		"mysql expression trunc": &mysqldriver.MySQLError{Number: 1292, Message: "Truncated incorrect DOUBLE value: 'x'"},
		"pg syntax error":        &pgconn.PgError{Code: "42601"},
		"pg undefined col":       &pgconn.PgError{Code: "42703"},
		"mysql lock wait":        &mysqldriver.MySQLError{Number: 1205},
		"mysql bad field":        &mysqldriver.MySQLError{Number: 1054, Message: "Unknown column 'x' in 'field list'"},
		"pg connection err":      &pgconn.PgError{Code: "08006"},
	} {
		var env *contract.ErrorEnvelope
		if mapped := MapPersistError(ctx, err, "conflict", "internal"); !errors.As(mapped, &env) || env.GetStatus() != http.StatusInternalServerError {
			t.Errorf("%s: %+v, want a 500", name, mapped)
		}
	}
}

// The text check adds no allocation to an ordinary write of a model with
// text columns.
func TestTextCheckAddsNoAllocationsToOrdinaryWrites(t *testing.T) {
	open := func(withCheck bool) *DB {
		db := openTextDB(t)
		if !withCheck {
			_ = db.Callback().Create().Remove("gombit:text")
			_ = db.Callback().Update().Remove("gombit:text")
		}
		if err := db.AutoMigrate(&textItem{}); err != nil {
			t.Fatal(err)
		}
		return db
	}
	measure := func(db *DB) map[string]float64 {
		row := textItem{Name: "x", Notes: "n"}
		db.Create(&row)
		return map[string]float64{
			"Create":          testing.AllocsPerRun(50, func() { db.Create(&textItem{Name: "x", Notes: "notes"}) }),
			"Save":            testing.AllocsPerRun(50, func() { db.Save(&row) }),
			"Updates(map)":    testing.AllocsPerRun(50, func() { db.Model(&row).Updates(map[string]any{"name": "y"}) }),
			"Updates(struct)": testing.AllocsPerRun(50, func() { db.Model(&row).Updates(textItem{Name: "z"}) }),
		}
	}
	without, with := measure(open(false)), measure(open(true))
	for op, base := range without {
		if with[op] > base+0.5 {
			t.Errorf("%s: %.1f allocs with the check, %.1f without", op, with[op], base)
		}
	}
}
