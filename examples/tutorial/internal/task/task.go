package task

import "gorm.io/gorm"

// Task is the tutorial's feature-package GORM model — the human-owned source of
// truth, exactly as `gombit make resource Task title:string:required done:bool`
// scaffolds it (model-first: no "do not edit" banner; the DTOs and handler are
// generated from it). This copy is hand-maintained so the tutorial compiles.
type Task struct {
	gorm.Model
	Title string `gorm:"size:255;not null"`
	Done  bool
}
