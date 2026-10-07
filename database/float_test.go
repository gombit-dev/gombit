package database

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/contract"
	"gorm.io/gorm/clause"
)

type floatItem struct {
	ID    uint `gorm:"primaryKey"`
	Name  string
	Ratio float64
	Small float32
	Opt   *float64
	NF    sql.NullFloat64
}

func openFloatDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(config.DatabaseConfig{Driver: config.DatabaseDriverSQLite, DSN: "file:" + filepath.Join(t.TempDir(), "float.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// A float a write sets to +Inf, -Inf or NaN is a 422 naming the field on every
// write path and driver (issue #449): PostgreSQL and SQLite stored it, MySQL
// refused it with a 500, and a stored one has no JSON form, so every response
// holding the row failed to encode.
func TestFloatWritesMustBeFinite(t *testing.T) {
	testFloatWrites(t, openFloatDB(t))
}

func testFloatWrites(t *testing.T, db *DB) {
	t.Helper()
	_ = db.Migrator().DropTable(&floatItem{})
	if err := db.AutoMigrate(&floatItem{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Migrator().DropTable(&floatItem{}) })
	row := floatItem{Name: "ok", Ratio: 1.5}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	inf, nan := math.Inf(1), math.NaN()
	for what, c := range map[string]struct {
		err   error
		field string
	}{
		"Create +Inf":                     {db.Create(&floatItem{Ratio: inf}).Error, "ratio"},
		"Create -Inf":                     {db.Create(&floatItem{Ratio: math.Inf(-1)}).Error, "ratio"},
		"Create NaN":                      {db.Create(&floatItem{Ratio: nan}).Error, "ratio"},
		"Create float32 +Inf":             {db.Create(&floatItem{Small: float32(math.Inf(1))}).Error, "small"},
		"Create *float64 NaN":             {db.Create(&floatItem{Opt: &nan}).Error, "opt"},
		"Create NullFloat64 Inf":          {db.Create(&floatItem{NF: sql.NullFloat64{Float64: inf, Valid: true}}).Error, "nf"},
		"Updates(map) string Inf":         {db.Model(&row).Updates(map[string]any{"ratio": "Inf"}).Error, "ratio"},
		"Updates(map) string NaN":         {db.Model(&row).Updates(map[string]any{"ratio": " NaN "}).Error, "ratio"},
		"Update overflowing 1e400":        {db.Model(&row).Update("ratio", "1e400").Error, "ratio"},
		"Update(column) NaN":              {db.Model(&row).Update("ratio", nan).Error, "ratio"},
		"Updates(struct) -Inf":            {db.Model(&row).Updates(floatItem{Ratio: math.Inf(-1)}).Error, "ratio"},
		"Save +Inf":                       {db.Save(&floatItem{ID: row.ID, Name: "s", Ratio: inf}).Error, "ratio"},
		"Updates(map) float32 overflow":   {db.Model(&row).Updates(map[string]any{"small": 1e39}).Error, "small"},
		"Update(column) float32 overflow": {db.Model(&row).Update("small", "1e39").Error, "small"},
		"upsert DO UPDATE NaN": {db.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "id"}},
			DoUpdates: clause.Assignments(map[string]any{"ratio": nan}),
		}).Create(&floatItem{ID: row.ID, Name: "u"}).Error, "ratio"},
	} {
		var ve *ValidationError
		if !errors.As(c.err, &ve) || len(ve.Fields[c.field]) == 0 {
			t.Errorf("%s: err = %v, want a *ValidationError on %q", what, c.err, c.field)
			continue
		}
		var env *contract.ErrorEnvelope
		if !errors.As(MapPersistError(context.Background(), c.err, "conflict", "internal"), &env) || env.GetStatus() != http.StatusUnprocessableEntity {
			t.Errorf("%s: MapPersistError = %+v, want a 422", what, env)
		}
	}

	// Finite values, NULL, and the extremes that are finite are written.
	big, opt := math.MaxFloat64, -0.25
	if err := db.Create(&floatItem{Name: "fine", Ratio: big, Small: math.MaxFloat32, Opt: &opt, NF: sql.NullFloat64{Float64: 2, Valid: true}}).Error; err != nil {
		t.Errorf("finite create: %v", err)
	}
	if err := db.Model(&row).Updates(map[string]any{"opt": nil, "nf": sql.NullFloat64{}, "ratio": "2.5"}).Error; err != nil {
		t.Errorf("NULLs and a finite string: %v", err)
	}
	var stored floatItem
	if err := db.First(&stored, row.ID).Error; err != nil || stored.Ratio != 2.5 || stored.Name != "ok" {
		t.Fatalf("a refused write changed the row: %+v (%v)", stored, err)
	}
}

// The check adds no allocation to an ordinary write of a model with float
// columns.
func TestFloatCheckAddsNoAllocationsToOrdinaryWrites(t *testing.T) {
	open := func(withCheck bool) *DB {
		db := openFloatDB(t)
		if !withCheck {
			_ = db.Callback().Create().Remove("gombit:float")
			_ = db.Callback().Update().Remove("gombit:float")
		}
		if err := db.AutoMigrate(&floatItem{}); err != nil {
			t.Fatal(err)
		}
		return db
	}
	measure := func(db *DB) map[string]float64 {
		row := floatItem{Name: "x", Ratio: 1}
		db.Create(&row)
		return map[string]float64{
			"Create":          testing.AllocsPerRun(50, func() { db.Create(&floatItem{Name: "x", Ratio: 2}) }),
			"Save":            testing.AllocsPerRun(50, func() { db.Save(&row) }),
			"Updates(map)":    testing.AllocsPerRun(50, func() { db.Model(&row).Updates(map[string]any{"ratio": 3.5}) }),
			"Updates(struct)": testing.AllocsPerRun(50, func() { db.Model(&row).Updates(floatItem{Ratio: 4}) }),
		}
	}
	without, with := measure(open(false)), measure(open(true))
	for op, base := range without {
		if with[op] > base+0.5 {
			t.Errorf("%s: %.1f allocs with the check, %.1f without", op, with[op], base)
		}
	}
}
