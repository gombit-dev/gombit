package resourcegen

import (
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// The CHECK clause is the database half of min/max. An insert outside the
// range must fail; a value inside it, including the explicit zero a default
// must not swallow, must persist.
func TestCheckConstraintRejectsOutOfRange(t *testing.T) {
	fields, err := parseFields([]string{"age:int:required,min=0,max=150"}, "person")
	if err != nil {
		t.Fatal(err)
	}
	const check = "check:age >= 0 AND age <= 150"
	if !strings.Contains(fields[0].gormTag(), check) {
		t.Fatalf("gorm tag = %q, want %q", fields[0].gormTag(), check)
	}

	type person struct {
		ID  uint `gorm:"primaryKey"`
		Age int  `gorm:"check:age >= 0 AND age <= 150"`
	}
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "check.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&person{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&person{Age: 200}).Error; err == nil {
		t.Fatal("age 200 must violate the check constraint")
	}
	if err := db.Create(&person{Age: 0}).Error; err != nil {
		t.Fatalf("age 0 is inside the range: %v", err)
	}
}
