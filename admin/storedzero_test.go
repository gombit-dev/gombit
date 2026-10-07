package admin_test

import (
	"database/sql/driver"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gombit-dev/gombit/admin"
	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/types"
	"gorm.io/gorm"
)

// Models whose rows store the zero instant from before the database's time
// range check (issue #443): an unset non-pointer field on SQLite and
// PostgreSQL, '0000-00-00' on MySQL.
type szDerived struct {
	ID        uint       `gorm:"primaryKey" json:"id"`
	Title     string     `json:"title"`
	Due       time.Time  `json:"due"`
	Issued    types.Date `json:"issued"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

type szUnlisted struct { // registered with explicit Fields: id, title
	ID    uint      `gorm:"primaryKey" json:"id"`
	Title string    `json:"title"`
	Due   time.Time `json:"due"`
}

type szServer struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	Title       string    `json:"title"`
	ProcessedAt time.Time `gombit:"server" json:"processed_at"`
}

type szHidden struct {
	ID    uint      `gorm:"primaryKey" json:"id"`
	Title string    `json:"title"`
	Due   time.Time `gombit:"-" json:"-"`
}

type szStamp time.Time

func (s szStamp) Value() (driver.Value, error) { return time.Time(s), nil }

func (s *szStamp) Scan(src any) error {
	t, ok := src.(time.Time)
	if !ok && src != nil {
		return fmt.Errorf("szStamp: cannot scan %T", src)
	}
	*s = szStamp(t)
	return nil
}

type szNamed struct {
	ID    uint    `gorm:"primaryKey" json:"id"`
	Title string  `json:"title"`
	At    szStamp `json:"-"`
}

type szHook struct {
	ID    uint      `gorm:"primaryKey" json:"id"`
	Title string    `json:"title"`
	Due   time.Time `json:"due"`
}

var szRepaired = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

// BeforeSave fills a zero Due, the natural cleanup of such rows: the admin
// must write what the hook set.
func (h *szHook) BeforeSave(*gorm.DB) error {
	if h.Due.IsZero() {
		h.Due = szRepaired
	}
	return nil
}

// szDefault has a time column with a default, which Save leaves out when the
// struct leaves it unset, and a nullable one.
type szDefault struct {
	ID    uint       `gorm:"primaryKey" json:"id"`
	Title string     `json:"title"`
	At    time.Time  `gorm:"default:null" json:"at"`
	Opt   *time.Time `gorm:"default:null" json:"opt"`
}

// szReadOnly has a time column the database fills (gorm:"->"), which GORM
// never writes: creating a row is not a 422 on it.
type szReadOnly struct {
	ID       uint      `gorm:"primaryKey" json:"id"`
	Title    string    `json:"title"`
	Computed time.Time `gorm:"->" json:"computed"`
}

type szVersioned struct {
	ID      uint      `gorm:"primaryKey" json:"id"`
	Title   string    `json:"title"`
	Due     time.Time `json:"due"`
	Version int64     `json:"version"`
}

// A row storing the zero instant stays editable through the admin, whatever
// the column is to the admin (mapped, outside Fields, server-set, hidden, a
// named time type, on a versioned model): a PATCH that does not set it does
// not write it back, nor does one that sends it back unchanged. A hook that
// repairs it is written; a PATCH writing the zero instant over a real value is
// refused; UpdatedAt advances (#562 rounds 4-5, #564 round 2).
func TestResourcePatchKeepsStoredZeroTimestampsEditable(t *testing.T) {
	runStoredZeroAdmin(t, openSQLite(t))
}

func runStoredZeroAdmin(t *testing.T, db *database.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	models := []any{&szDerived{}, &szUnlisted{}, &szServer{}, &szHidden{}, &szNamed{}, &szHook{}, &szVersioned{}, &szDefault{}, &szReadOnly{}}
	_ = db.Migrator().DropTable(models...)
	if err := db.AutoMigrate(models...); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	t.Cleanup(func() { _ = db.Migrator().DropTable(models...) })
	app := newCookieAppWithDB(t, db)
	register := func(model any, opts admin.Options) {
		t.Helper()
		if err := admin.Register(app, model, opts); err != nil {
			t.Fatalf("Register %s: %v", opts.Slug, err)
		}
	}
	register(szDerived{}, admin.Options{Slug: "sz-derived"})
	register(szUnlisted{}, admin.Options{Slug: "sz-unlisted", Fields: []admin.Field{
		{Name: "id", Type: admin.TypeInteger, ReadOnly: true},
		{Name: "title", Type: admin.TypeString, Required: true},
	}})
	register(szServer{}, admin.Options{Slug: "sz-server"})
	register(szHidden{}, admin.Options{Slug: "sz-hidden"})
	register(szNamed{}, admin.Options{Slug: "sz-named"})
	register(szHook{}, admin.Options{Slug: "sz-hook"})
	register(szVersioned{}, admin.Options{Slug: "sz-versioned"})
	register(szDefault{}, admin.Options{Slug: "sz-default"})
	register(szReadOnly{}, admin.Options{Slug: "sz-readonly"})
	jar := loginSuperuser(t, app)

	valid := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		slug, table string
		row         any
		zero        []string // columns made to store the zero instant
		check       func(t *testing.T, id uint)
	}{
		{"sz-derived", "sz_deriveds", &szDerived{Title: "t", Due: valid, Issued: types.NewDate(valid)}, []string{"due", "issued"}, func(t *testing.T, id uint) {
			var r szDerived
			mustFirst(t, db, &r, id)
			if r.Title != "renamed" || !r.Due.IsZero() || !r.Issued.IsZero() || !r.UpdatedAt.After(old) || r.CreatedAt.IsZero() {
				t.Errorf("stored %+v, want the title changed, due and issued kept, updated_at advanced", r)
			}
		}},
		{"sz-unlisted", "sz_unlisteds", &szUnlisted{Title: "t", Due: valid}, []string{"due"}, func(t *testing.T, id uint) {
			var r szUnlisted
			mustFirst(t, db, &r, id)
			if r.Title != "renamed" || !r.Due.IsZero() {
				t.Errorf("stored %+v", r)
			}
		}},
		{"sz-server", "sz_servers", &szServer{Title: "t", ProcessedAt: valid}, []string{"processed_at"}, func(t *testing.T, id uint) {
			var r szServer
			mustFirst(t, db, &r, id)
			if r.Title != "renamed" || !r.ProcessedAt.IsZero() {
				t.Errorf("stored %+v", r)
			}
		}},
		{"sz-hidden", "sz_hiddens", &szHidden{Title: "t", Due: valid}, []string{"due"}, func(t *testing.T, id uint) {
			var r szHidden
			mustFirst(t, db, &r, id)
			if r.Title != "renamed" || !r.Due.IsZero() {
				t.Errorf("stored %+v", r)
			}
		}},
		{"sz-named", "sz_nameds", &szNamed{Title: "t", At: szStamp(valid)}, []string{"at"}, func(t *testing.T, id uint) {
			var r szNamed
			mustFirst(t, db, &r, id)
			if r.Title != "renamed" || !time.Time(r.At).IsZero() {
				t.Errorf("stored %+v", r)
			}
		}},
		{"sz-hook", "sz_hooks", &szHook{Title: "t", Due: valid}, []string{"due"}, func(t *testing.T, id uint) {
			var r szHook
			mustFirst(t, db, &r, id)
			if r.Title != "renamed" || !r.Due.Equal(szRepaired) {
				t.Errorf("stored %+v, want the hook's repair %v written", r, szRepaired)
			}
		}},
		{"sz-versioned", "sz_versioneds", &szVersioned{Title: "t", Due: valid}, []string{"due"}, func(t *testing.T, id uint) {
			var r szVersioned
			mustFirst(t, db, &r, id)
			if r.Title != "renamed" || !r.Due.IsZero() || r.Version != 1 {
				t.Errorf("stored %+v", r)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.slug, func(t *testing.T) {
			if err := db.Create(c.row).Error; err != nil {
				t.Fatalf("create fixture: %v", err)
			}
			id := rowID(c.row)
			storeZeroInstant(t, db, c.table, id, c.zero...)
			if c.slug == "sz-derived" {
				if err := db.Exec("UPDATE sz_deriveds SET updated_at = ? WHERE id = ?", old, id).Error; err != nil {
					t.Fatal(err)
				}
			}
			path := fmt.Sprintf("/api/v1/admin/resources/%s/%d", c.slug, id)
			if res := doRequest(app, jar, http.MethodPatch, path, `{"title":"renamed"}`); res.Code != http.StatusOK {
				t.Fatalf("title-only PATCH: %d %s", res.Code, res.Body.String())
			}
			c.check(t, id)
		})
	}

	// The admin's form sends every field back, so a PATCH that resends the
	// stored zero unchanged keeps it (#564 review round 2); a PATCH that
	// writes the zero instant over a real value is refused, however it is
	// spelled.
	var r szDerived
	if err := db.Where("title = ?", "renamed").First(&r).Error; err != nil {
		t.Fatal(err)
	}
	full := `{"title":"form","due":"0001-01-01T00:00:00Z"}`
	if res := doRequest(app, jar, http.MethodPatch, fmt.Sprintf("/api/v1/admin/resources/sz-derived/%d", r.ID), full); res.Code != http.StatusOK {
		t.Fatalf("full-form PATCH resending the stored zero: %d %s", res.Code, res.Body.String())
	}
	live := szDerived{Title: "live", Due: valid, Issued: types.NewDate(valid)}
	if err := db.Create(&live).Error; err != nil {
		t.Fatal(err)
	}
	for _, due := range []string{"0001-01-01T00:00:00Z", "0001-01-01T00:00:00+00:00"} {
		res := doRequest(app, jar, http.MethodPatch, fmt.Sprintf("/api/v1/admin/resources/sz-derived/%d", live.ID), `{"due":"`+due+`"}`)
		assertError(t, res, http.StatusUnprocessableEntity, contract.CodeValidationError)
	}

	// On a column with a default, which Save leaves out when unset, a zero
	// the PATCH sets is still refused however it is spelled, and null clears
	// a nullable one.
	d := szDefault{Title: "d", At: valid, Opt: &valid}
	if err := db.Create(&d).Error; err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/v1/admin/resources/sz-default/%d", d.ID)
	for _, at := range []string{"0001-01-01T00:00:00Z", "0001-01-01T00:00:00+00:00"} {
		res := doRequest(app, jar, http.MethodPatch, path, `{"at":"`+at+`"}`)
		assertError(t, res, http.StatusUnprocessableEntity, contract.CodeValidationError)
	}
	if res := doRequest(app, jar, http.MethodPatch, path, `{"opt":null}`); res.Code != http.StatusOK {
		t.Fatalf("PATCH opt null: %d %s", res.Code, res.Body.String())
	}
	var stored szDefault
	mustFirst(t, db, &stored, d.ID)
	if !stored.At.Equal(valid) || stored.Opt != nil {
		t.Errorf("stored %+v, want at kept and opt cleared", stored)
	}

	if res := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/sz-readonly", `{"title":"x"}`); res.Code != http.StatusOK {
		t.Errorf("create with an unwritten read-only time column: %d %s", res.Code, res.Body.String())
	}
}

func rowID(row any) uint {
	switch r := row.(type) {
	case *szDerived:
		return r.ID
	case *szUnlisted:
		return r.ID
	case *szServer:
		return r.ID
	case *szHidden:
		return r.ID
	case *szNamed:
		return r.ID
	case *szHook:
		return r.ID
	case *szVersioned:
		return r.ID
	}
	panic(fmt.Sprintf("rowID: %T", row))
}

func mustFirst(t *testing.T, db *database.DB, dest any, id uint) {
	t.Helper()
	if err := db.First(dest, id).Error; err != nil {
		t.Fatalf("read back: %v", err)
	}
}

// storeZeroInstant writes the zero instant to columns of a row, as a write
// from before the range check did: time.Time{} on SQLite and PostgreSQL,
// '0000-00-00' under a permissive sql_mode on MySQL (restored before the
// connection goes back to the pool).
func storeZeroInstant(t *testing.T, db *database.DB, table string, id uint, columns ...string) {
	t.Helper()
	err := db.Transaction(func(tx *gorm.DB) error {
		for _, col := range columns {
			var err error
			if db.Driver() == database.DriverMySQL {
				if err = tx.Exec("SET SESSION sql_mode = ''").Error; err != nil {
					return err
				}
				err = tx.Exec(fmt.Sprintf("UPDATE %s SET %s = '0000-00-00' WHERE id = ?", table, col), id).Error
				if rerr := tx.Exec("SET SESSION sql_mode = @@GLOBAL.sql_mode").Error; err == nil {
					err = rerr
				}
			} else {
				err = tx.Exec(fmt.Sprintf("UPDATE %s SET %s = ? WHERE id = ?", table, col), time.Time{}, id).Error
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("store the zero instant: %v", err)
	}
}
