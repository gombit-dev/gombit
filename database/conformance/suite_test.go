//go:build conformance

package conformance_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/database/conformance/models"
	"github.com/gombit-dev/gombit/types"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

func TestConformanceSuite(t *testing.T) {
	h := newHarness(t)
	db := h.openDB()

	t.Run("migrate_up", func(t *testing.T) {
		if !db.Migrator().HasTable(&models.Item{}) {
			t.Fatal("expected items table after Atlas migrate apply")
		}
		if !db.Migrator().HasTable("framework_migrations") {
			t.Fatal("expected framework_migrations after migrate")
		}
		if !db.Migrator().HasTable("atlas_schema_revisions") {
			t.Fatal("expected atlas_schema_revisions after Atlas apply")
		}
	})

	t.Run("indexes", func(t *testing.T) {
		indexes, err := db.Migrator().GetIndexes(&models.Item{})
		if err != nil {
			t.Fatalf("GetIndexes: %v", err)
		}
		var hasUniqueCode, hasNameIndex bool
		for _, idx := range indexes {
			cols := idx.Columns()
			unique, _ := idx.Unique()
			if len(cols) == 1 && cols[0] == "code" && unique {
				hasUniqueCode = true
			}
			if len(cols) == 1 && cols[0] == "name" {
				hasNameIndex = true
			}
		}
		if !hasUniqueCode {
			t.Fatalf("expected unique index on code; indexes=%#v", indexes)
		}
		if !hasNameIndex {
			t.Fatalf("expected index on name; indexes=%#v", indexes)
		}
	})

	t.Run("timestamps", func(t *testing.T) {
		item := models.Item{
			Code:  "ts-1",
			Name:  "timestamps",
			Price: decimal.RequireFromString("1.0000"),
		}
		if err := db.Create(&item).Error; err != nil {
			t.Fatalf("Create: %v", err)
		}
		// Reload so CreatedAt/UpdatedAt match driver storage precision
		// (Postgres timestamptz drops Go's nanosecond remainder).
		var before models.Item
		if err := db.First(&before, item.ID).Error; err != nil {
			t.Fatalf("First after create: %v", err)
		}
		if before.CreatedAt.IsZero() || before.UpdatedAt.IsZero() {
			t.Fatalf("timestamps not set: created=%v updated=%v", before.CreatedAt, before.UpdatedAt)
		}
		createdAt := before.CreatedAt
		updatedAt := before.UpdatedAt
		// MySQL DATETIME is second-precision by default; wait past one second so
		// GORM's auto UpdatedAt is guaranteed to advance on every driver.
		time.Sleep(1100 * time.Millisecond)
		before.Name = "timestamps-updated"
		if err := db.Save(&before).Error; err != nil {
			t.Fatalf("Save: %v", err)
		}
		var after models.Item
		if err := db.First(&after, before.ID).Error; err != nil {
			t.Fatalf("First after update: %v", err)
		}
		if !after.CreatedAt.Equal(createdAt) {
			t.Fatalf("CreatedAt changed: got %v want %v", after.CreatedAt, createdAt)
		}
		if !after.UpdatedAt.After(updatedAt) {
			t.Fatalf("UpdatedAt not bumped: before=%v after=%v", updatedAt, after.UpdatedAt)
		}
	})

	t.Run("nullable", func(t *testing.T) {
		nullItem := models.Item{
			Code:  "null-1",
			Name:  "nullable-null",
			Price: decimal.RequireFromString("2.5000"),
		}
		if err := db.Create(&nullItem).Error; err != nil {
			t.Fatalf("Create null notes: %v", err)
		}
		var loaded models.Item
		if err := db.First(&loaded, nullItem.ID).Error; err != nil {
			t.Fatalf("First: %v", err)
		}
		if loaded.Notes != nil {
			t.Fatalf("Notes = %v, want nil", loaded.Notes)
		}

		note := "hello"
		withNotes := models.Item{
			Code:  "null-2",
			Name:  "nullable-set",
			Notes: &note,
			Price: decimal.RequireFromString("3.2500"),
		}
		if err := db.Create(&withNotes).Error; err != nil {
			t.Fatalf("Create with notes: %v", err)
		}
		loaded = models.Item{}
		if err := db.First(&loaded, withNotes.ID).Error; err != nil {
			t.Fatalf("First with notes: %v", err)
		}
		if loaded.Notes == nil || *loaded.Notes != note {
			t.Fatalf("Notes = %v, want %q", loaded.Notes, note)
		}
	})

	t.Run("unique", func(t *testing.T) {
		first := models.Item{
			Code:  "unique-1",
			Name:  "unique-a",
			Price: decimal.RequireFromString("4.0000"),
		}
		if err := db.Create(&first).Error; err != nil {
			t.Fatalf("Create first: %v", err)
		}
		dup := models.Item{
			Code:  "unique-1",
			Name:  "unique-b",
			Price: decimal.RequireFromString("5.0000"),
		}
		if err := db.Create(&dup).Error; err == nil {
			t.Fatal("Create duplicate unique code error = nil, want error")
		}
	})

	t.Run("decimal", func(t *testing.T) {
		want := decimal.RequireFromString("19.9900")
		wantDiscount := types.Decimal{Decimal: decimal.RequireFromString("2.5000")}
		item := models.Item{
			Code:     "dec-1",
			Name:     "decimal",
			Price:    want,
			Discount: wantDiscount,
		}
		if err := db.Create(&item).Error; err != nil {
			t.Fatalf("Create: %v", err)
		}
		var loaded models.Item
		if err := db.First(&loaded, item.ID).Error; err != nil {
			t.Fatalf("First: %v", err)
		}
		if !loaded.Price.Equal(want) {
			t.Fatalf("Price = %s, want %s", loaded.Price.StringFixed(4), want.StringFixed(4))
		}
		// types.Decimal (#222) must round-trip through the driver like the bare
		// shopspring decimal does.
		if !loaded.Discount.Equal(wantDiscount.Decimal) {
			t.Fatalf("Discount = %s, want %s", loaded.Discount.StringFixed(4), wantDiscount.StringFixed(4))
		}
	})

	// #440: a value that fits decimal(19,4) round-trips exactly on PostgreSQL
	// and MySQL. SQLite has no fixed-point type, so it refuses one with more
	// than database.SQLiteDecimalDigits significant digits instead of storing
	// it changed. Every driver refuses one over the column's scale rather than
	// rounding it.
	t.Run("decimal_precision", func(t *testing.T) {
		sqlite := db.Driver() == database.DriverSQLite
		for i, c := range []struct {
			value        string
			refusedLite  bool
			refusedOther bool
		}{
			{"99999999999.9999", false, false},
			{"99999999999999.9999", true, false},
			{"123456789012345.1234", true, false},
			{"1.00005", true, true},
		} {
			want := decimal.RequireFromString(c.value)
			item := models.Item{
				Code:     "dec-precision-" + string(rune('a'+i)),
				Name:     "decimal precision",
				Price:    want,
				Discount: types.Decimal{Decimal: want},
			}
			err := db.Create(&item).Error
			refused := c.refusedOther
			if sqlite {
				refused = c.refusedLite
			}
			if refused {
				var ve *database.ValidationError
				if !errors.As(err, &ve) {
					t.Fatalf("Create %s on %s: error = %v, want a *database.ValidationError", c.value, db.Driver(), err)
				}
				continue
			}
			if err != nil {
				t.Fatalf("Create %s on %s: %v", c.value, db.Driver(), err)
			}
			var loaded models.Item
			if err := db.First(&loaded, item.ID).Error; err != nil {
				t.Fatalf("First: %v", err)
			}
			if !loaded.Price.Equal(want) || !loaded.Discount.Equal(want) {
				t.Fatalf("%s on %s stored as %s / %s, want it exactly", c.value, db.Driver(), loaded.Price, loaded.Discount)
			}
		}
	})

	t.Run("crud", func(t *testing.T) {
		item := models.Item{
			Code:  "crud-1",
			Name:  "create",
			Price: decimal.RequireFromString("10.0000"),
		}
		if err := db.Create(&item).Error; err != nil {
			t.Fatalf("Create: %v", err)
		}
		var loaded models.Item
		if err := db.First(&loaded, "code = ?", "crud-1").Error; err != nil {
			t.Fatalf("Read: %v", err)
		}
		loaded.Name = "updated"
		if err := db.Save(&loaded).Error; err != nil {
			t.Fatalf("Update: %v", err)
		}
		if err := db.Delete(&loaded).Error; err != nil {
			t.Fatalf("Delete: %v", err)
		}
		var soft models.Item
		if err := db.First(&soft, loaded.ID).Error; err == nil {
			t.Fatal("soft-deleted row still visible to default First")
		}
		if err := db.Unscoped().First(&soft, loaded.ID).Error; err != nil {
			t.Fatalf("Unscoped First after soft delete: %v", err)
		}
		if !soft.DeletedAt.Valid {
			t.Fatal("DeletedAt not set after Delete")
		}
	})

	t.Run("tx", func(t *testing.T) {
		err := db.Transaction(func(tx *gorm.DB) error {
			return tx.Create(&models.Item{
				Code:  "tx-commit",
				Name:  "commit",
				Price: decimal.RequireFromString("11.0000"),
			}).Error
		})
		if err != nil {
			t.Fatalf("commit tx: %v", err)
		}
		var count int64
		if err := db.Model(&models.Item{}).Where("code = ?", "tx-commit").Count(&count).Error; err != nil {
			t.Fatalf("Count committed: %v", err)
		}
		if count != 1 {
			t.Fatalf("committed count = %d, want 1", count)
		}

		err = db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Create(&models.Item{
				Code:  "tx-rollback",
				Name:  "rollback",
				Price: decimal.RequireFromString("12.0000"),
			}).Error; err != nil {
				return err
			}
			return errors.New("force rollback")
		})
		if err == nil {
			t.Fatal("rollback tx error = nil, want error")
		}
		count = 0
		if err := db.Model(&models.Item{}).Where("code = ?", "tx-rollback").Count(&count).Error; err != nil {
			t.Fatalf("Count rolled back: %v", err)
		}
		if count != 0 {
			t.Fatalf("rolled-back count = %d, want 0", count)
		}
	})

	t.Run("pagination", func(t *testing.T) {
		for i := 1; i <= 5; i++ {
			item := models.Item{
				Code:  "page-" + decimal.NewFromInt(int64(i)).String(),
				Name:  "page",
				Price: decimal.NewFromInt(int64(i)),
			}
			if err := db.Create(&item).Error; err != nil {
				t.Fatalf("Create page item %d: %v", i, err)
			}
		}
		var page []models.Item
		if err := db.Where("name = ?", "page").Order("code asc").Offset(2).Limit(2).Find(&page).Error; err != nil {
			t.Fatalf("Find page: %v", err)
		}
		if len(page) != 2 {
			t.Fatalf("page len = %d, want 2", len(page))
		}
		if page[0].Code != "page-3" || page[1].Code != "page-4" {
			t.Fatalf("page codes = [%s, %s], want [page-3, page-4]", page[0].Code, page[1].Code)
		}
	})

	// Gombit's deletion semantics are the database's (ADR-019, #312):
	// database.Delete issues a real DELETE, so each ON DELETE policy does what
	// it declares, on every dialect, including for a gorm.Model parent.
	t.Run("relation_deletion", func(t *testing.T) {
		db := h.openDB()
		defer func() { _ = db.Close() }()
		ctx := context.Background()
		gdb := db.DB

		owner := models.Owner{Name: "restrict"}
		mustCreate(t, gdb, &owner)
		mustCreate(t, gdb, &models.RestrictedChild{OwnerID: owner.ID})
		if _, err := database.Delete(ctx, gdb, &models.Owner{}, owner.ID); !errors.Is(err, database.ErrReferenced) {
			t.Fatalf("RESTRICT: Delete() = %v, want ErrReferenced", err)
		}
		if n := countWhere(t, gdb, &models.Owner{}, "id = ?", owner.ID); n != 1 {
			t.Fatalf("RESTRICT: owner rows = %d, want the refused parent kept", n)
		}

		owner = models.Owner{Name: "cascade"}
		mustCreate(t, gdb, &owner)
		mustCreate(t, gdb, &models.CascadedChild{OwnerID: owner.ID})
		if n, err := database.Delete(ctx, gdb, &models.Owner{}, owner.ID); err != nil || n != 1 {
			t.Fatalf("CASCADE: Delete() = %d, %v", n, err)
		}
		if n := countWhere(t, gdb, &models.CascadedChild{}, "owner_id = ?", owner.ID); n != 0 {
			t.Fatalf("CASCADE: children left = %d, want 0", n)
		}

		owner = models.Owner{Name: "set null"}
		mustCreate(t, gdb, &owner)
		child := models.NulledChild{OwnerID: &owner.ID}
		mustCreate(t, gdb, &child)
		if _, err := database.Delete(ctx, gdb, &models.Owner{}, owner.ID); err != nil {
			t.Fatalf("SET NULL: Delete() = %v", err)
		}
		var got models.NulledChild
		if err := gdb.First(&got, child.ID).Error; err != nil || got.OwnerID != nil {
			t.Fatalf("SET NULL: child = %+v (%v), want owner_id NULL", got, err)
		}

		soft := models.SoftOwner{Name: "legacy"}
		mustCreate(t, gdb, &soft)
		mustCreate(t, gdb, &models.SoftOwnerChild{SoftOwnerID: soft.ID})
		if _, err := database.Delete(ctx, gdb, &models.SoftOwner{}, soft.ID); !errors.Is(err, database.ErrReferenced) {
			t.Fatalf("gorm.Model parent: Delete() = %v, want ErrReferenced (a real DELETE, not a soft delete)", err)
		}
		if err := gdb.Delete(&models.SoftOwnerChild{}, "soft_owner_id = ?", soft.ID).Error; err != nil {
			t.Fatal(err)
		}
		if n, err := database.Delete(ctx, gdb, &models.SoftOwner{}, soft.ID); err != nil || n != 1 {
			t.Fatalf("gorm.Model parent without children: Delete() = %d, %v", n, err)
		}
		if n := countWhere(t, gdb.Unscoped(), &models.SoftOwner{}, "id = ?", soft.ID); n != 0 {
			t.Fatalf("gorm.Model parent rows (unscoped) = %d, want the row physically gone", n)
		}
	})

	t.Run("migrate_down", func(t *testing.T) {
		h.rollback()
		check, err := database.Open(h.cfg)
		if err != nil {
			t.Fatalf("reopen after rollback: %v", err)
		}
		defer func() { _ = check.Close() }()
		if check.Migrator().HasTable(&models.Item{}) {
			t.Fatal("items table should be gone after rollback")
		}
		var revCount int64
		if err := check.Table("framework_migrations").Count(&revCount).Error; err != nil {
			t.Fatalf("count framework_migrations: %v", err)
		}
		if revCount != 0 {
			t.Fatalf("framework_migrations count = %d, want 0", revCount)
		}
	})
}

func mustCreate(t *testing.T, db *gorm.DB, value any) {
	t.Helper()
	if err := db.Create(value).Error; err != nil {
		t.Fatalf("create %T: %v", value, err)
	}
}

func countWhere(t *testing.T, db *gorm.DB, model any, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.Model(model).Where(query, args...).Count(&n).Error; err != nil {
		t.Fatalf("count %T: %v", model, err)
	}
	return n
}
