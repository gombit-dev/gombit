package book

import "time"

// Book is the feature-package GORM model — the human-owned source of truth.
// Edit its fields and their gombit:"..." policy, then run `gombit generate`
// to re-derive the DTOs, mappers, and handler (*.gen.go).
type Book struct {
	ID        uint `gorm:"primaryKey" json:"id"`
	CreatedAt time.Time
	UpdatedAt time.Time
	Title     string `gorm:"size:255;not null"`
}
