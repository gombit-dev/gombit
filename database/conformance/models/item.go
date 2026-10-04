// Package models holds GORM fixtures for the multi-DB conformance suite.
package models

import (
	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/gombit-dev/gombit/types"
)

// Item exercises portable schema features required by M2-4 conformance:
// timestamps (via gorm.Model), nullable columns, unique + non-unique indexes,
// and decimal storage (Loose with no declared precision). Discount uses the framework types.Decimal wrapper (#222)
// so the shared money type is covered across the SQLite/PostgreSQL/MySQL matrix.
type Item struct {
	gorm.Model
	Code     string          `gorm:"size:64;uniqueIndex;not null"`
	Name     string          `gorm:"size:120;index;not null"`
	Notes    *string         `gorm:"size:255"`
	Price    decimal.Decimal `gorm:"type:decimal(19,4);not null"`
	Discount types.Decimal   `gorm:"type:decimal(19,4);not null"`
	// Loose declares no precision, as an untagged types.Decimal does: an
	// unbounded numeric on PostgreSQL, DECIMAL(10,0) on MySQL (#440).
	Loose *types.Decimal
}

// TableName returns the stable table name for Atlas-generated migrations.
func (Item) TableName() string {
	return "items"
}
