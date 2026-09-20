package task

import "gorm.io/gorm"

// Task is the tutorial's feature-package GORM model. It matches what
// `gombit make resource Task title:string:required done:bool` emits, minus
// the "do not edit" banner: this copy is hand-maintained so the tutorial has
// something to compile against.
type Task struct {
	gorm.Model
	Title string `gorm:"size:255;not null"`
	Done  bool
}
