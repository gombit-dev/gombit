package product

import "time"

// Product is a minimal feature-package model for migration examples, in the
// shape gombit make resource scaffolds: no soft-delete DeletedAt (ADR-019).
type Product struct {
	ID        uint `gorm:"primaryKey" json:"id"`
	CreatedAt time.Time
	UpdatedAt time.Time
	Name      string `gorm:"size:120;not null"`
	Price     int64  `gorm:"not null"`
}
