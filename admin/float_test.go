package admin_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gombit-dev/gombit/admin"
	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/database"
)

type floatRatio struct {
	ID    uint    `gorm:"primaryKey" json:"id"`
	Name  string  `json:"name"`
	Ratio float64 `json:"ratio"`
	Small float32 `json:"small"`
}

// A non-finite float is a 422 on that field, not a stored value that makes
// every response holding the row fail to encode, the list page included
// (issue #449).
func TestAdminFloatsMustBeFinite(t *testing.T) {
	runFloatAdmin(t, openSQLite(t))
}

func runFloatAdmin(t *testing.T, db *database.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	_ = db.Migrator().DropTable(&floatRatio{})
	if err := db.AutoMigrate(&floatRatio{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Migrator().DropTable(&floatRatio{}) })
	app := newCookieAppWithDB(t, db)
	if err := admin.Register(app, floatRatio{}, admin.Options{Slug: "ratios"}); err != nil {
		t.Fatal(err)
	}
	jar := loginSuperuser(t, app)
	const base = "/api/v1/admin/resources/ratios"
	for body, field := range map[string]string{
		`{"name":"a","ratio":"Inf"}`:  "ratio",
		`{"name":"a","ratio":"-Inf"}`: "ratio",
		`{"name":"a","ratio":"NaN"}`:  "ratio",
		`{"name":"a","small":1e300}`:  "small", // overflows float32 to +Inf
	} {
		res := doRequest(app, jar, http.MethodPost, base, body)
		assertError(t, res, http.StatusUnprocessableEntity, contract.CodeValidationError)
		if env := decodeError(t, res); len(env.Fields[field]) == 0 {
			t.Errorf("%s: fields %v, want %q", body, env.Fields, field)
		}
	}
	created := doRequest(app, jar, http.MethodPost, base, `{"name":"ok","ratio":"1.25","small":3.5}`)
	if created.Code != http.StatusOK {
		t.Fatalf("finite create: %d %s", created.Code, created.Body.String())
	}
	var row floatRatio
	if err := db.Where("name = ?", "ok").First(&row).Error; err != nil {
		t.Fatal(err)
	}
	patch := doRequest(app, jar, http.MethodPatch, fmt.Sprintf("%s/%d", base, row.ID), `{"ratio":"Infinity"}`)
	assertError(t, patch, http.StatusUnprocessableEntity, contract.CodeValidationError)
	if list := doRequest(app, jar, http.MethodGet, base, ""); list.Code != http.StatusOK {
		t.Fatalf("list: %d %s", list.Code, list.Body.String())
	}
	var count int64
	if err := db.Model(&floatRatio{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("rows = %d (%v), want only the finite one", count, err)
	}
}
