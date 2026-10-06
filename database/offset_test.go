package database

import (
	"math"
	"path/filepath"
	"testing"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/contract"
)

type pagedRow struct {
	ID   uint `gorm:"primaryKey"`
	Name string
}

// testFarPageIsEmpty proves on a real driver that the offset PageOffset
// saturates to for a page far past the end selects nothing. A wrapped,
// negative offset is "no offset" to GORM and used to return the first page's
// rows (issue #441); math.MaxInt must be accepted and empty everywhere.
func testFarPageIsEmpty(t *testing.T, db *DB) {
	t.Helper()
	if err := db.AutoMigrate(&pagedRow{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Migrator().DropTable(&pagedRow{}) })
	for _, name := range []string{"a", "b", "c"} {
		if err := db.Create(&pagedRow{Name: name}).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, page := range []int{math.MaxInt, math.MaxInt/contract.MaxPerPage + 2} {
		page, perPage := contract.ClampPage(page, contract.MaxPerPage)
		var rows []pagedRow
		if err := db.Order("id").Offset(contract.PageOffset(page, perPage)).Limit(perPage).Find(&rows).Error; err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if len(rows) != 0 {
			t.Fatalf("page %d returned %d rows, want none: %+v", page, len(rows), rows)
		}
	}
}

func TestFarPageIsEmptyOnSQLite(t *testing.T) {
	db, err := Open(config.DatabaseConfig{
		Driver: config.DatabaseDriverSQLite,
		DSN:    "file:" + filepath.Join(t.TempDir(), "paged.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	testFarPageIsEmpty(t, db)
}
