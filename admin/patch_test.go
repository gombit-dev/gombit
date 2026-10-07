package admin_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gombit-dev/gombit/admin"
	"github.com/gombit-dev/gombit/database"
	"gorm.io/gorm"
)

type ppRow struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Name      string    `json:"name"`
	Price     int       `json:"price"`
	Note      string    `json:"note"`
	Slug      string    `json:"slug"`
	UpdatedAt time.Time `json:"updated_at"`
}

// BeforeSave derives Slug from Name: a column the PATCH does not send, which
// the hook changes, is written.
func (r *ppRow) BeforeSave(*gorm.DB) error {
	r.Slug = strings.ToLower(strings.ReplaceAll(r.Name, " ", "-"))
	return nil
}

type ppChild struct {
	ID       uint   `gorm:"primaryKey" json:"id"`
	Name     string `json:"name"`
	ParentID uint   `json:"parent_id"`
}

type ppParent struct {
	ID       uint      `gorm:"primaryKey" json:"id"`
	Name     string    `json:"name"`
	Children []ppChild `gorm:"foreignKey:ParentID" json:"children"`
}

// An admin PATCH writes the columns it (or a model hook) changes, and nothing
// else (issue #450): a concurrent change to another column is not reverted, a
// row deleted meanwhile is a 404 rather than re-inserted, and has_many
// children are never written back.
func TestAdminPatchWritesOnlyWhatChanged(t *testing.T) {
	runPartialPatchAdmin(t, openSQLite(t))
}

func runPartialPatchAdmin(t *testing.T, db *database.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	models := []any{&ppRow{}, &ppChild{}, &ppParent{}}
	_ = db.Migrator().DropTable(models...)
	if err := db.AutoMigrate(models...); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Migrator().DropTable(models...) })
	app := newCookieAppWithDB(t, db)
	if err := admin.Register(app, ppRow{}, admin.Options{Slug: "pp-rows"}); err != nil {
		t.Fatal(err)
	}
	if err := admin.Register(app, ppParent{}, admin.Options{Slug: "pp-parents"}); err != nil {
		t.Fatal(err)
	}
	jar := loginSuperuser(t, app)

	// concurrently runs sql once, between the PATCH's load and its write.
	registered := 0
	concurrently := func(t *testing.T, table, sql string, args ...any) {
		t.Helper()
		registered++
		name := fmt.Sprintf("test:concurrent_%d", registered)
		fired := false
		if err := db.Callback().Update().Before("gorm:update").Register(name, func(tx *gorm.DB) {
			if fired || tx.Statement.Table != table {
				return
			}
			fired = true
			if err := tx.Session(&gorm.Session{NewDB: true}).Exec(sql, args...).Error; err != nil {
				t.Errorf("concurrent %s: %v", sql, err)
			}
		}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Callback().Update().Remove(name) })
	}

	// (a) A concurrent price change survives a PATCH of the note; the
	// response is the row as stored. The hook-derived slug is written.
	row := ppRow{Name: "First Row", Price: 100}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&row).UpdateColumn("updated_at", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)).Error; err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/v1/admin/resources/pp-rows/%d", row.ID)
	t.Run("concurrent column", func(t *testing.T) {
		concurrently(t, "pp_rows", "UPDATE pp_rows SET price = 500 WHERE id = ?", row.ID)
		res := doRequest(app, jar, http.MethodPatch, path, `{"note":"only the note","name":"Renamed Row"}`)
		if res.Code != http.StatusOK {
			t.Fatalf("PATCH: %d %s", res.Code, res.Body.String())
		}
		var stored ppRow
		mustFirst(t, db, &stored, row.ID)
		if stored.Price != 500 || stored.Note != "only the note" || stored.Slug != "renamed-row" || stored.UpdatedAt.Year() < 2021 {
			t.Errorf("stored %+v, want price 500 kept, note and hook slug written, updated_at advanced", stored)
		}
		if !strings.Contains(res.Body.String(), `"price":500`) {
			t.Errorf("response is not the stored row: %s", res.Body.String())
		}
	})

	// An unchanged PATCH is a 200: MySQL reports no row affected for it.
	t.Run("unchanged", func(t *testing.T) {
		if res := doRequest(app, jar, http.MethodPatch, path, `{"note":"only the note"}`); res.Code != http.StatusOK {
			t.Fatalf("unchanged PATCH: %d %s", res.Code, res.Body.String())
		}
	})

	// (b) A row deleted between the load and the write is a 404, not
	// re-inserted.
	t.Run("concurrent delete", func(t *testing.T) {
		victim := ppRow{Name: "victim"}
		if err := db.Create(&victim).Error; err != nil {
			t.Fatal(err)
		}
		concurrently(t, "pp_rows", "DELETE FROM pp_rows WHERE id = ?", victim.ID)
		res := doRequest(app, jar, http.MethodPatch, fmt.Sprintf("/api/v1/admin/resources/pp-rows/%d", victim.ID), `{"note":"edited"}`)
		assertError(t, res, http.StatusNotFound, "not_found")
		var n int64
		if err := db.Model(&ppRow{}).Where("id = ?", victim.ID).Count(&n).Error; err != nil || n != 0 {
			t.Fatalf("rows with the deleted id: %d (%v), want 0", n, err)
		}
	})

	// (c) has_many children are not written back: one moved to another
	// parent and one deleted meanwhile stay that way.
	t.Run("has_many children", func(t *testing.T) {
		p1, p2 := ppParent{Name: "p1"}, ppParent{Name: "p2"}
		if err := db.Create(&p1).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&p2).Error; err != nil {
			t.Fatal(err)
		}
		kids := []ppChild{{Name: "moved", ParentID: p1.ID}, {Name: "deleted", ParentID: p1.ID}}
		if err := db.Create(&kids).Error; err != nil {
			t.Fatal(err)
		}
		concurrently(t, "pp_parents", "UPDATE pp_children SET parent_id = ? WHERE id = ?", p2.ID, kids[0].ID)
		concurrently(t, "pp_parents", "DELETE FROM pp_children WHERE id = ?", kids[1].ID)
		res := doRequest(app, jar, http.MethodPatch, fmt.Sprintf("/api/v1/admin/resources/pp-parents/%d", p1.ID), `{"name":"p1-renamed"}`)
		if res.Code != http.StatusOK {
			t.Fatalf("PATCH parent: %d %s", res.Code, res.Body.String())
		}
		var children []ppChild
		if err := db.Order("id").Find(&children).Error; err != nil {
			t.Fatal(err)
		}
		if len(children) != 1 || children[0].ID != kids[0].ID || children[0].ParentID != p2.ID {
			t.Errorf("children after the PATCH: %+v, want only the moved one, under p2", children)
		}
	})
}
