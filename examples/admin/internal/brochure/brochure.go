package brochure

import (
	"time"

	"github.com/gombit-dev/gombit/types"
)

// Brochure has storage-backed fields: a required PDF and an optional cover
// image. Each column holds the object key under the prefix its field owns
// (the storage tag); the admin uploads to it, previews the image, and
// deletes the replaced or removed file after the change commits (through
// storage/claims, whose table main migrates).
type Brochure struct {
	ID        uint         `gorm:"primaryKey" json:"id"`
	Title     string       `gorm:"size:255;not null" json:"title"`
	PDF       types.File   `gorm:"size:512;not null;uniqueIndex" json:"pdf" storage:"prefix=brochures/pdf/;max_bytes=5242880;types=application/pdf"`
	Cover     *types.Image `gorm:"size:512;uniqueIndex" json:"cover" storage:"prefix=brochures/cover/;max_bytes=1048576"`
	CreatedAt time.Time    `json:"created_at"`
	UpdatedAt time.Time    `json:"updated_at"`
}
