package models

import "gorm.io/gorm"

// Owner is the parent every relation-deletion fixture references (#312).
type Owner struct {
	ID   uint   `gorm:"primaryKey"`
	Name string `gorm:"size:64;not null"`
}

// TableName returns the stable table name for Atlas-generated migrations.
func (Owner) TableName() string { return "owners" }

// RestrictedChild references Owner with ON DELETE RESTRICT.
type RestrictedChild struct {
	ID      uint  `gorm:"primaryKey"`
	OwnerID uint  `gorm:"not null;index"`
	Owner   Owner `gorm:"constraint:OnDelete:RESTRICT;"`
}

// TableName returns the stable table name for Atlas-generated migrations.
func (RestrictedChild) TableName() string { return "restricted_children" }

// CascadedChild references Owner with ON DELETE CASCADE.
type CascadedChild struct {
	ID      uint  `gorm:"primaryKey"`
	OwnerID uint  `gorm:"not null;index"`
	Owner   Owner `gorm:"constraint:OnDelete:CASCADE;"`
}

// TableName returns the stable table name for Atlas-generated migrations.
func (CascadedChild) TableName() string { return "cascaded_children" }

// NulledChild references Owner with ON DELETE SET NULL.
type NulledChild struct {
	ID      uint   `gorm:"primaryKey"`
	OwnerID *uint  `gorm:"index"`
	Owner   *Owner `gorm:"constraint:OnDelete:SET NULL;"`
}

// TableName returns the stable table name for Atlas-generated migrations.
func (NulledChild) TableName() string { return "nulled_children" }

// SoftOwner is a legacy gorm.Model parent: GORM's own Delete only sets
// deleted_at, so no foreign key fires. database.Delete removes it for real.
type SoftOwner struct {
	gorm.Model
	Name string `gorm:"size:64;not null"`
}

// TableName returns the stable table name for Atlas-generated migrations.
func (SoftOwner) TableName() string { return "soft_owners" }

// SoftOwnerChild references SoftOwner with ON DELETE RESTRICT.
type SoftOwnerChild struct {
	ID          uint      `gorm:"primaryKey"`
	SoftOwnerID uint      `gorm:"not null;index"`
	SoftOwner   SoftOwner `gorm:"constraint:OnDelete:RESTRICT;"`
}

// TableName returns the stable table name for Atlas-generated migrations.
func (SoftOwnerChild) TableName() string { return "soft_owner_children" }
