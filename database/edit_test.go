package database

import (
	"sort"
	"testing"

	"gorm.io/gorm"
)

type editRow struct {
	ID    uint `gorm:"primaryKey"`
	Name  string
	Price int
	Note  string
	Ptr   *string
	Blob  []byte
	Tags  []string       `gorm:"serializer:json"`
	Meta  map[string]any `gorm:"serializer:json"`
}

// BeforeSave edits a slice and a map in place when Name is "hook": the
// comparison must still see the change.
func (r *editRow) BeforeSave(*gorm.DB) error {
	if r.Name == "hook" {
		sort.Strings(r.Tags)
		if r.Meta != nil {
			r.Meta["touched"] = true
		}
	}
	return nil
}

// An update scoped to a loaded row (ScopeEdit) writes only the columns the
// edit changed, compared after the hooks, so a concurrent change to another
// column is kept (issue #450). A pointer's target or a byte slice changed in
// place counts as a change; an edit that changes nothing writes no column but
// the key; the chain's Select is restored for reuse.
func TestScopeEditWritesOnlyChangedColumns(t *testing.T) {
	testScopeEditColumns(t, openTextDB(t))
}

func testScopeEditColumns(t *testing.T, db *DB) {
	t.Helper()
	_ = db.Migrator().DropTable(&editRow{})
	if err := db.AutoMigrate(&editRow{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Migrator().DropTable(&editRow{}) })
	ptr := "p"
	row := editRow{Name: "n", Price: 100, Ptr: &ptr, Blob: []byte("blob")}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	var loaded editRow
	if err := db.First(&loaded, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	stored := StoredValues(db.DB, &loaded)

	// Another writer changes the price after the load.
	if err := db.Exec("UPDATE edit_rows SET price = 500 WHERE id = ?", row.ID).Error; err != nil {
		t.Fatal(err)
	}
	loaded.Note = "edited"
	*loaded.Ptr = "in place"
	loaded.Blob[0] = 'B'
	q := ScopeEdit(db.DB, &loaded, stored).Model(&loaded).Select("*")
	if err := q.Updates(&loaded).Error; err != nil {
		t.Fatalf("scoped Updates: %v", err)
	}
	if len(q.Statement.Selects) != 1 || q.Statement.Selects[0] != "*" {
		t.Errorf("the chain's Select was not restored: %v", q.Statement.Selects)
	}
	var got editRow
	if err := db.First(&got, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Price != 500 || got.Note != "edited" || got.Ptr == nil || *got.Ptr != "in place" || string(got.Blob) != "Blob" {
		t.Errorf("stored %+v (ptr %v, blob %q), want price 500 kept and the edits written", got, got.Ptr, got.Blob)
	}

	// Save narrows the same way.
	again := StoredValues(db.DB, &got)
	if err := db.Exec("UPDATE edit_rows SET price = 700 WHERE id = ?", row.ID).Error; err != nil {
		t.Fatal(err)
	}
	got.Name = "saved"
	if err := ScopeEdit(db.DB, &got, again).Save(&got).Error; err != nil {
		t.Fatalf("scoped Save: %v", err)
	}
	var final editRow
	if err := db.First(&final, row.ID).Error; err != nil || final.Price != 700 || final.Name != "saved" {
		t.Errorf("after the scoped Save: %+v (%v), want price 700 kept and the name written", final, err)
	}

	// A hook that edits a slice or a map in place changes the row: the
	// snapshot is a deep copy, so the comparison sees it.
	hooked := editRow{Name: "plain", Tags: []string{"b", "a"}, Meta: map[string]any{"k": "v"}}
	if err := db.Create(&hooked).Error; err != nil {
		t.Fatal(err)
	}
	var hl editRow
	if err := db.First(&hl, hooked.ID).Error; err != nil {
		t.Fatal(err)
	}
	hs := StoredValues(db.DB, &hl)
	hl.Name = "hook"
	if err := ScopeEdit(db.DB, &hl, hs).Model(&hl).Select("*").Updates(&hl).Error; err != nil {
		t.Fatalf("hooked Updates: %v", err)
	}
	var hr editRow
	if err := db.First(&hr, hooked.ID).Error; err != nil {
		t.Fatal(err)
	}
	if len(hr.Tags) != 2 || hr.Tags[0] != "a" || hr.Meta["touched"] != true {
		t.Errorf("hook's in-place edits were not written: tags %v, meta %v", hr.Tags, hr.Meta)
	}

	// An edit that changes nothing writes no column: a concurrent change is
	// kept.
	none := StoredValues(db.DB, &final)
	if err := db.Exec("UPDATE edit_rows SET price = 900 WHERE id = ?", row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := ScopeEdit(db.DB, &final, none).Model(&final).Select("*").Updates(&final).Error; err != nil {
		t.Errorf("unchanged scoped Updates: %v", err)
	}
	var kept editRow
	if err := db.First(&kept, row.ID).Error; err != nil || kept.Price != 900 {
		t.Errorf("an unchanged scoped Updates wrote the row: price %d (%v), want 900", kept.Price, err)
	}
}
