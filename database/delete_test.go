package database

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	"gorm.io/gorm"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/contract"
)

type delOwner struct {
	ID   uint `gorm:"primaryKey"`
	Name string
}

type delRestricted struct {
	ID      uint `gorm:"primaryKey"`
	OwnerID uint
	Owner   delOwner `gorm:"constraint:OnDelete:RESTRICT;"`
}

type delCascaded struct {
	ID      uint `gorm:"primaryKey"`
	OwnerID uint
	Owner   delOwner `gorm:"constraint:OnDelete:CASCADE;"`
}

type delNulled struct {
	ID      uint `gorm:"primaryKey"`
	OwnerID *uint
	Owner   *delOwner `gorm:"constraint:OnDelete:SET NULL;"`
}

// delSoftOwner is a legacy gorm.Model parent: GORM's own Delete only sets
// deleted_at on it.
type delSoftOwner struct {
	gorm.Model
	Name string
}

type delSoftChild struct {
	ID             uint `gorm:"primaryKey"`
	DelSoftOwnerID uint
	DelSoftOwner   delSoftOwner `gorm:"constraint:OnDelete:RESTRICT;"`
}

func openDeleteDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := Open(config.DatabaseConfig{Driver: config.DatabaseDriverSQLite, DSN: "file:" + filepath.Join(t.TempDir(), "delete.db") + "?_fk=1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.AutoMigrate(&delOwner{}, &delRestricted{}, &delCascaded{}, &delNulled{}, &delSoftOwner{}, &delSoftChild{}); err != nil {
		t.Fatal(err)
	}
	return db.DB
}

func TestDeleteFollowsTheForeignKeys(t *testing.T) {
	ctx := context.Background()
	db := openDeleteDB(t)

	t.Run("RESTRICT refuses and maps to 409", func(t *testing.T) {
		owner := delOwner{Name: "r"}
		db.Create(&owner)
		db.Create(&delRestricted{OwnerID: owner.ID})
		_, err := Delete(ctx, db, &delOwner{}, owner.ID)
		if !errors.Is(err, ErrReferenced) {
			t.Fatalf("Delete() = %v, want ErrReferenced", err)
		}
		var ce *contract.ErrorEnvelope
		if mapped := MapDeleteError(ctx, err, "still referenced", "delete"); !errors.As(mapped, &ce) || ce.GetStatus() != http.StatusConflict {
			t.Fatalf("MapDeleteError() = %v, want a 409 conflict", mapped)
		}
		var count int64
		db.Model(&delOwner{}).Where("id = ?", owner.ID).Count(&count)
		if count != 1 {
			t.Fatal("the refused parent is gone")
		}
	})

	t.Run("CASCADE removes the children", func(t *testing.T) {
		owner := delOwner{Name: "c"}
		db.Create(&owner)
		db.Create(&delCascaded{OwnerID: owner.ID})
		if n, err := Delete(ctx, db, &delOwner{}, owner.ID); err != nil || n != 1 {
			t.Fatalf("Delete() = %d, %v", n, err)
		}
		var count int64
		db.Model(&delCascaded{}).Where("owner_id = ?", owner.ID).Count(&count)
		if count != 0 {
			t.Fatalf("cascaded children left: %d", count)
		}
	})

	t.Run("SET NULL clears the child key", func(t *testing.T) {
		owner := delOwner{Name: "n"}
		db.Create(&owner)
		child := delNulled{OwnerID: &owner.ID}
		db.Create(&child)
		if _, err := Delete(ctx, db, &delOwner{}, owner.ID); err != nil {
			t.Fatal(err)
		}
		var got delNulled
		db.First(&got, child.ID)
		if got.OwnerID != nil {
			t.Fatalf("child owner_id = %v, want NULL", *got.OwnerID)
		}
	})

	t.Run("a gorm.Model parent is deleted physically", func(t *testing.T) {
		owner := delSoftOwner{Name: "s"}
		db.Create(&owner)
		db.Create(&delSoftChild{DelSoftOwnerID: owner.ID})
		// GORM's own Delete only stamps deleted_at: RESTRICT never fires.
		if err := db.Delete(&delSoftOwner{}, owner.ID).Error; err != nil {
			t.Fatalf("soft delete: %v", err)
		}
		// database.Delete issues a real DELETE, which the database refuses.
		if _, err := Delete(ctx, db, &delSoftOwner{}, owner.ID); !errors.Is(err, ErrReferenced) {
			t.Fatalf("Delete() of a referenced gorm.Model row = %v, want ErrReferenced", err)
		}
	})
}
