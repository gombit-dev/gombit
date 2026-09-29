package document

import (
	"time"

	"github.com/gombit-dev/gombit/types"
)

// Document is a model with storage-backed fields, as `gombit make resource
// Document title:string:required attachment:file:required cover:image`
// scaffolds it: each file column holds an object key, under a prefix the
// field owns (its storage tag), with a unique index so one record owns each
// file. The generated handler grants direct uploads for the fields, accepts
// only uploads that pass the policy, and returns file objects on reads.
type Document struct {
	ID         uint `gorm:"primaryKey" json:"id"`
	CreatedAt  time.Time
	UpdatedAt  time.Time
	Title      string       `gorm:"size:255;not null"`
	Attachment types.File   `gorm:"size:512;not null;uniqueIndex" storage:"prefix=document/attachment/;max_bytes=10485760;types=application/pdf,image/png,image/jpeg,text/plain"`
	Cover      *types.Image `gorm:"size:512;uniqueIndex" storage:"prefix=document/cover/;max_bytes=2097152"`
}
