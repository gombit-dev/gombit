package database

import (
	"database/sql"
	"math"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/types"
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

	// PostgreSQL stores NaN: one left as it is is no change, so a concurrent
	// change to the column is kept rather than reverted to the NaN.
	if db.Driver() == DriverPostgres {
		_ = db.Migrator().DropTable(&nanRow{})
		if err := db.AutoMigrate(&nanRow{}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Migrator().DropTable(&nanRow{}) })
		created := nanRow{Name: "nan"}
		if err := db.Create(&created).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("UPDATE nan_rows SET score = 'NaN' WHERE id = ?", created.ID).Error; err != nil {
			t.Fatal(err)
		}
		var nl nanRow
		if err := db.First(&nl, created.ID).Error; err != nil || !math.IsNaN(nl.Score) {
			t.Fatalf("NaN fixture: %v, %v", err, nl.Score)
		}
		ns := StoredValues(db.DB, &nl)
		if err := db.Exec("UPDATE nan_rows SET score = 42 WHERE id = ?", created.ID).Error; err != nil {
			t.Fatal(err)
		}
		nl.Name = "nan edited"
		if err := ScopeEdit(db.DB, &nl, ns).Model(&nl).Select("*").Updates(&nl).Error; err != nil {
			t.Fatalf("scoped Updates of a NaN row: %v", err)
		}
		var after nanRow
		if err := db.First(&after, created.ID).Error; err != nil || after.Score != 42 || after.Name != "nan edited" {
			t.Errorf("NaN row after the edit: %+v (%v), want score 42 kept", after, err)
		}
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

type nanRow struct {
	ID    uint `gorm:"primaryKey"`
	Name  string
	Score float64
}

// valuesEqual compares column values, not Go representations.
func TestValuesEqual(t *testing.T) {
	at := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	nan := math.NaN()
	one, other := "a", "a"
	dec1, _ := types.NewDecimalFromString("1.5")
	dec2, _ := types.NewDecimalFromString("1.50")
	for name, c := range map[string]struct {
		a, b any
		want bool
	}{
		"NaN equals NaN":           {nan, nan, true},
		"NaN is not 1":             {nan, 1.0, false},
		"same instant, other zone": {at, at.In(time.FixedZone("x", -3*3600)), true},
		"other instant":            {at, at.Add(time.Second), false},
		"NullTime, other zone":     {sql.NullTime{Time: at, Valid: true}, sql.NullTime{Time: at.Local(), Valid: true}, true},
		"NullFloat64 NaN":          {sql.NullFloat64{Float64: nan, Valid: true}, sql.NullFloat64{Float64: nan, Valid: true}, true},
		"pointers by target":       {&one, &other, true},
		"nil pointer and value":    {(*string)(nil), &one, false},
		"decimal by value":         {dec1, dec2, true},
		"slices":                   {[]string{"a"}, []string{"a"}, true},
		"slices differ":            {[]string{"a"}, []string{"b"}, false},
	} {
		if got := valuesEqual(reflect.ValueOf(c.a), reflect.ValueOf(c.b)); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}
