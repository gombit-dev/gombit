package database

import (
	"errors"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

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
	ID     uint            `gorm:"primaryKey"`
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

	// The message says what was wrong.
	err := db.Create(&decimalRow{Amount: dec("99999999999999.9999")}).Error
	var ve *ValidationError
	if !errors.As(err, &ve) || !strings.Contains(strings.Join(ve.Fields["amount"], " "), "SQLite") {
		t.Fatalf("SQLite refusal = %v, want a message naming the SQLite limit", err)
	}
}
