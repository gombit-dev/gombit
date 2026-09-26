package testmodels

import "gorm.io/gorm"

// Item is Product renamed (table items), used by the table-rename tests.
type Item struct {
	gorm.Model
	Name  string `gorm:"size:120;not null"`
	Price int64  `gorm:"not null"`
}
