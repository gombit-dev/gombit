package task

import "time"

// Task is the tutorial's feature-package GORM model — the human-owned source of
// truth, exactly as `gombit make resource Task title:string:required done:bool`
// scaffolds it (model-first: no "do not edit" banner). The request/response DTOs
// and CRUD handler are derived from it into the committed dto.gen.go /
// handler.gen.go, which TestGeneratedFilesAreFresh re-derives and byte-compares
// so this model and its generated files never drift. It has no soft-delete
// DeletedAt: Gombit deletes rows physically (ADR-019).
type Task struct {
	ID        uint `gorm:"primaryKey" json:"id"`
	CreatedAt time.Time
	UpdatedAt time.Time
	Title     string `gorm:"size:255;not null"`
	Done      bool
}
