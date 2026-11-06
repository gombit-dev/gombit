package admin_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	_ "time/tzdata" // the zones below, wherever the test runs

	"github.com/gin-gonic/gin"
	"github.com/gombit-dev/gombit/admin"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/types"
)

// datedVisit has a required and an optional calendar date (#478).
type datedVisit struct {
	ID   uint        `gorm:"primaryKey" json:"id"`
	Name string      `json:"name"`
	Day  types.Date  `gorm:"type:date;not null" json:"day"`
	Opt  *types.Date `gorm:"type:date" json:"opt"`
}

// TestAdminDateDriverLocationSQLite: mattn/go-sqlite3 returns a date column
// in its _loc, so west of UTC the admin read every date a day early (#478).
func TestAdminDateDriverLocationSQLite(t *testing.T) {
	for _, c := range []struct{ name, param string }{
		{"no _loc", ""},
		{"_loc west of UTC", "&_loc=America%2FNew_York"},
		{"_loc east of UTC", "&_loc=Asia%2FTokyo"},
	} {
		t.Run(c.name, func(t *testing.T) {
			db, err := database.Open(config.DatabaseConfig{
				Driver: config.DatabaseDriverSQLite,
				DSN:    "file:" + filepath.Join(t.TempDir(), "admin.db") + "?cache=shared&_fk=1" + c.param,
			})
			if err != nil {
				t.Fatalf("database.Open() error = %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			runDateLocationAdmin(t, db, dateEdges)
		})
	}
}

// dateEdges are the calendar edges a shift by the driver's time zone moves
// into another year or month.
var dateEdges = []string{"2024-01-01", "2024-02-29", "2024-12-31"}

// runDateLocationAdmin creates a row per day through the admin and checks
// that the row holds the day, and that a read, an edit of another field and
// an edit of the date keep it.
func runDateLocationAdmin(t *testing.T, db *database.DB, days []string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	_ = db.Migrator().DropTable(&datedVisit{})
	if err := db.AutoMigrate(&datedVisit{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Migrator().DropTable(&datedVisit{}) })
	app := newCookieAppWithDB(t, db)
	if err := admin.Register(app, datedVisit{}, admin.Options{Slug: "visits"}); err != nil {
		t.Fatal(err)
	}
	jar := loginSuperuser(t, app)
	const base = "/api/v1/admin/resources/visits"
	read := func(id uint) (day, opt any) {
		t.Helper()
		rec := doRequest(app, jar, http.MethodGet, fmt.Sprintf("%s/%d", base, id), "")
		var env struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("GET %d status = %d, err %v; body: %s", id, rec.Code, err, rec.Body.String())
		}
		return env.Data["day"], env.Data["opt"]
	}
	for i, day := range days {
		name := fmt.Sprintf("v%d", i)
		created := doRequest(app, jar, http.MethodPost, base, fmt.Sprintf(`{"name":%q,"day":%q,"opt":%q}`, name, day, day))
		if created.Code != http.StatusOK {
			t.Fatalf("create %s: %d %s", day, created.Code, created.Body.String())
		}
		var row datedVisit
		if err := db.Where("name = ?", name).First(&row).Error; err != nil {
			t.Fatal(err)
		}
		if row.Day.String() != day || row.Opt == nil || row.Opt.String() != day {
			t.Errorf("%s: created row holds day %s, opt %v", day, row.Day, row.Opt)
		}
		if d, o := read(row.ID); d != day || o != day {
			t.Errorf("%s: GET reads day %v, opt %v", day, d, o)
		}
		for _, body := range []string{`{"name":"edited"}`, fmt.Sprintf(`{"day":%q,"opt":%q}`, day, day)} {
			if rec := doRequest(app, jar, http.MethodPatch, fmt.Sprintf("%s/%d", base, row.ID), body); rec.Code != http.StatusOK {
				t.Fatalf("PATCH %s: %d %s", body, rec.Code, rec.Body.String())
			}
			if d, o := read(row.ID); d != day || o != day {
				t.Errorf("%s: after PATCH %s, GET reads day %v, opt %v", day, body, d, o)
			}
		}
		var stored datedVisit
		if err := db.First(&stored, row.ID).Error; err != nil {
			t.Fatal(err)
		}
		if stored.Day.String() != day || stored.Opt == nil || stored.Opt.String() != day {
			t.Errorf("%s: after the edits the row holds day %s, opt %v", day, stored.Day, stored.Opt)
		}
	}
}
