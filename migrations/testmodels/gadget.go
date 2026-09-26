package testmodels

import "gorm.io/gorm"

// Gadget has a NOT NULL column with a database default, used by the lint and
// gate parity tests: changing the default makes Atlas rebuild a SQLite table
// with an IFNULL copy.
type Gadget struct {
	gorm.Model
	Name string `gorm:"not null;default:'changed'"`
}
