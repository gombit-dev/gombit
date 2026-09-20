package task

import "gorm.io/gorm"

// Task is the tutorial's feature-package GORM model — the human-owned source of
// truth, exactly as `gombit make resource Task title:string:required done:bool`
// scaffolds it (model-first: no "do not edit" banner). The request/response DTOs
// and CRUD handler are derived from it into the committed dto.gen.go /
// handler.gen.go, which TestGeneratedFilesAreFresh re-derives and byte-compares
// so this model and its generated files never drift.
type Task struct {
	gorm.Model
	Title string `gorm:"size:255;not null"`
	Done  bool
}
