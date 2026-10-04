package database

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"

	"github.com/gombit-dev/gombit/types"
)

// TestDecimalStorageProblem pins the #440 contract. Every driver rejects a
// value that does not fit the declared decimal(p,s) (PostgreSQL and MySQL would
// round or refuse it, SQLite silently change it). SQLite also rejects any value
// with more than SQLiteDecimalDigits significant digits, the most a REAL round
// trip keeps, rather than storing it changed.
func TestDecimalStorageProblem(t *testing.T) {
	cases := []struct {
		value            string
		precision, scale int
		sqlite, other    bool // whether a problem is reported on SQLite / elsewhere
	}{
		{"99999999999999.9999", 19, 4, true, false},
		{"123456789012345.1234", 19, 4, true, false},
		{"1.00005", 19, 4, true, true},
		{"99999999999.9999", 19, 4, false, false},
		{"100000000000000", 19, 4, false, false}, // 15 digits
		{"1000000000000000", 19, 4, true, true},  // 16 whole digits: over decimal(19,4)
		{"9007199254740993", 30, 4, true, false}, // 16 digits: conservative SQLite limit
		{"-12.5000", 5, 1, false, false},         // trailing zeros are not stored digits
		{"0.0001", 19, 4, false, false},
		{"12345.6", 5, 1, true, true},             // 5 whole digits > 5-1
		{"1234567890123456.5", 0, 0, true, false}, // no declared precision: SQLite limit only
	}
	for _, c := range cases {
		d := decimal.RequireFromString(c.value)
		if got := DecimalStorageProblem(d, c.precision, c.scale, true); (got != "") != c.sqlite {
			t.Errorf("SQLite %s in decimal(%d,%d): problem %q, want problem=%v", c.value, c.precision, c.scale, got, c.sqlite)
		}
		if got := DecimalStorageProblem(d, c.precision, c.scale, false); (got != "") != c.other {
			t.Errorf("PostgreSQL/MySQL %s in decimal(%d,%d): problem %q, want problem=%v", c.value, c.precision, c.scale, got, c.other)
		}
	}
}

type decimalRow struct {
	ID     uint `gorm:"primaryKey"`
	Note   string
	Amount types.Decimal   `gorm:"type:decimal(19,4)" json:"amount"`
	Tip    *types.Decimal  `gorm:"type:decimal(19,4)" json:"tip"`
	Raw    decimal.Decimal `gorm:"type:decimal(10,2)"`
}

// TestDecimalCallbackOnSQLite drives the check through every write path on
// SQLite: a value inside the contract round-trips exactly, and one outside it
// is refused before the SQL with a *ValidationError naming the field, so
// MapPersistError answers it with a 422. Nothing is stored for a refused write.
func TestDecimalCallbackOnSQLite(t *testing.T) {
	db := openSQLite(t)
	if err := db.AutoMigrate(&decimalRow{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	dec := func(s string) types.Decimal { return types.MustDecimal(s) }

	// Fifteen significant digits round-trip exactly.
	row := decimalRow{Amount: dec("99999999999.9999"), Raw: decimal.RequireFromString("12345678.91")}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("Create(15 digits) error = %v, want nil", err)
	}
	var stored decimalRow
	if err := db.First(&stored, row.ID).Error; err != nil {
		t.Fatalf("First: %v", err)
	}
	if !stored.Amount.Equal(row.Amount.Decimal) || !stored.Raw.Equal(row.Raw) {
		t.Fatalf("stored = %s / %s, want %s / %s exactly", stored.Amount, stored.Raw, row.Amount, row.Raw)
	}

	assertRefused := func(label string, err error, field string) {
		t.Helper()
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("%s: error = %v (%T), want *ValidationError", label, err, err)
		}
		if len(ve.Fields[field]) == 0 {
			t.Fatalf("%s: fields = %v, want a message for %q", label, ve.Fields, field)
		}
	}
	var before int64
	db.Model(&decimalRow{}).Count(&before)

	assertRefused("create over the SQLite limit",
		db.Create(&decimalRow{Amount: dec("99999999999999.9999")}).Error, "amount")
	assertRefused("create over the scale",
		db.Create(&decimalRow{Amount: dec("1.00005")}).Error, "amount")
	tip := dec("123456789012345.1234")
	assertRefused("create with a pointer field over the limit",
		db.Create(&decimalRow{Amount: dec("1"), Tip: &tip}).Error, "tip")
	assertRefused("create with a plain shopspring field over its scale",
		db.Create(&decimalRow{Amount: dec("1"), Raw: decimal.RequireFromString("1.005")}).Error, "raw")
	assertRefused("batch create",
		db.Create(&[]decimalRow{{Amount: dec("1")}, {Amount: dec("99999999999999.9999")}}).Error, "amount")

	stored.Amount = dec("123456789012345.1234")
	assertRefused("save", db.Save(&stored).Error, "amount")
	assertRefused("updates(map)",
		db.Model(&decimalRow{ID: row.ID}).Updates(map[string]any{"amount": dec("99999999999999.9999")}).Error, "amount")
	assertRefused("update(column)",
		db.Model(&decimalRow{ID: row.ID}).Update("amount", dec("1.00005")).Error, "amount")
	assertRefused("updates(struct)",
		db.Model(&decimalRow{ID: row.ID}).Updates(decimalRow{Amount: dec("99999999999999.9999")}).Error, "amount")

	var after int64
	db.Model(&decimalRow{}).Count(&after)
	if after != before {
		t.Fatalf("rows = %d after refused writes, want %d", after, before)
	}
	if err := db.First(&stored, row.ID).Error; err != nil || !stored.Amount.Equal(row.Amount.Decimal) {
		t.Fatalf("stored amount = %s (%v), want the original %s untouched", stored.Amount, err, row.Amount)
	}

	// Plain values bound for a decimal column are coerced and checked too:
	// Update(column, value) and Updates(map) carry them as given.
	assertRefused("update(column, string)",
		db.Model(&decimalRow{ID: row.ID}).Update("amount", "1.00005").Error, "amount")
	assertRefused("update(column, float64)",
		db.Model(&decimalRow{ID: row.ID}).Update("amount", 3.00005).Error, "amount")
	assertRefused("updates(map) with a string",
		db.Model(&decimalRow{ID: row.ID}).Updates(map[string]any{"amount": "99999999999999.9999"}).Error, "amount")
	assertRefused("updates(map) with a json.Number",
		db.Model(&decimalRow{ID: row.ID}).Updates(map[string]any{"amount": json.Number("1.00005")}).Error, "amount")
	// A struct of another type writes its fields by column name.
	type amountDTO struct{ Amount types.Decimal }
	assertRefused("updates(non-model struct)",
		db.Model(&decimalRow{ID: row.ID}).Updates(amountDTO{Amount: dec("2.00005")}).Error, "amount")
	// A SQL expression is the database's to compute.
	if err := db.Model(&decimalRow{ID: row.ID}).Update("amount", gorm.Expr("amount + ?", 1)).Error; err != nil {
		t.Fatalf("update(column, gorm.Expr) error = %v, want nil", err)
	}

	// The message says what was wrong.
	err := db.Create(&decimalRow{Amount: dec("99999999999999.9999")}).Error
	var ve *ValidationError
	if !errors.As(err, &ve) || !strings.Contains(strings.Join(ve.Fields["amount"], " "), "SQLite") {
		t.Fatalf("SQLite refusal = %v, want a message naming the SQLite limit", err)
	}
}

// TestDecimalCallbackChecksOnlyWrittenColumns: an update is judged by what it
// writes, not by the row's old values. A row stored before #440 can hold a
// SQLite REAL with more digits than the limit; it stays updatable on its other
// columns through the ordinary Model(&row) idiom, while a write that does
// assign the decimal (Save, unless the column is omitted) is still checked.
func TestDecimalCallbackChecksOnlyWrittenColumns(t *testing.T) {
	db := openSQLite(t)
	if err := db.AutoMigrate(&decimalRow{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	// Written the way pre-#440 code stored it: through float64, 17 digits.
	if err := db.Exec("INSERT INTO decimal_rows (note, amount, raw) VALUES ('a', 123456789012345.12, 0)").Error; err != nil {
		t.Fatal(err)
	}
	var row decimalRow
	if err := db.Last(&row).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&row).Update("note", "b").Error; err != nil {
		t.Fatalf("Model(&row).Update(note) error = %v, want nil: it does not write amount", err)
	}
	if err := db.Model(&row).Updates(map[string]any{"note": "c"}).Error; err != nil {
		t.Fatalf("Model(&row).Updates(map{note}) error = %v, want nil", err)
	}
	if err := db.Omit("amount").Save(&row).Error; err != nil {
		t.Fatalf("Omit(amount).Save error = %v, want nil: amount is not written", err)
	}
	var ve *ValidationError
	if err := db.Save(&row).Error; !errors.As(err, &ve) || len(ve.Fields["amount"]) == 0 {
		t.Fatalf("Save error = %v, want a refusal of amount, which Save writes", err)
	}
}

// TestDecimalPrecisionScaleUsesTheDriverDefault: a decimal column declared
// without (p,s), including an untagged types.Decimal, is DECIMAL(10,0) on MySQL,
// so 1.5 would be rounded to 2 there; PostgreSQL makes it an unbounded numeric
// and SQLite has no fixed precision.
func TestDecimalPrecisionScaleUsesTheDriverDefault(t *testing.T) {
	type loose struct {
		ID       uint
		Untagged types.Decimal
		Bare     types.Decimal `gorm:"type:decimal"`
		Numeric  types.Decimal `gorm:"type:NUMERIC"`
		Pinned   types.Decimal `gorm:"type:decimal(19,4)"`
		Tagged   types.Decimal `gorm:"precision:8;scale:2"`
	}
	s, err := schema.Parse(&loose{}, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		field         string
		driver        Driver
		precision, sc int
	}{
		{"Untagged", DriverMySQL, 10, 0},
		{"Bare", DriverMySQL, 10, 0},
		{"Numeric", DriverMySQL, 10, 0},
		{"Untagged", DriverPostgres, 0, 0},
		{"Bare", DriverSQLite, 0, 0},
		{"Pinned", DriverMySQL, 19, 4},
		{"Pinned", DriverSQLite, 19, 4},
		{"Tagged", DriverPostgres, 8, 2},
	} {
		p, sc := decimalPrecisionScale(s.LookUpField(c.field), c.driver)
		if p != c.precision || sc != c.sc {
			t.Errorf("%s on %s = (%d,%d), want (%d,%d)", c.field, c.driver, p, sc, c.precision, c.sc)
		}
	}
	if got := DecimalStorageProblem(decimal.RequireFromString("1.5"), 10, 0, false); got == "" {
		t.Error("1.5 in MySQL's default DECIMAL(10,0): no problem reported, want a refusal (it would be stored as 2)")
	}
}
