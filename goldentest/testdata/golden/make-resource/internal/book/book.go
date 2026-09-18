package book

import "gorm.io/gorm"

// Book is the feature-package GORM model — the human-owned source of truth.
// Edit its fields and their gombit:"..." policy, then run `gombit generate`
// to re-derive the DTOs, mappers, and handler (*.gen.go).
type Book struct {
	gorm.Model
	Title string `gorm:"size:255;not null"`
}
