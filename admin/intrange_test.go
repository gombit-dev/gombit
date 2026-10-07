package admin_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gombit-dev/gombit/admin"
	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/database"
)

// intDeclared declares integer Go fields as other admin types: the value must
// still fit the Go field (#448 review).
type intDeclared struct {
	ID      uint   `gorm:"primaryKey" json:"id"`
	AsFloat int8   `json:"as_float"`
	AsJSON  int8   `json:"as_json"`
	Wide    uint64 `json:"wide"`
}

type intRange struct {
	ID    uint   `gorm:"primaryKey" json:"id"`
	Name  string `json:"name"`
	Small int8   `json:"small"`
	Mid   int32  `json:"mid"`
	Qty   uint   `json:"qty"`
	Tiny  uint8  `json:"tiny"`
	Big   int64  `json:"big"`
	Opt   *int16 `json:"opt"`
}

// An integer the field's Go type cannot hold is a 422 naming the field, not a
// wrapped value: 300 into an int8 was stored as 44, -5 into a uint as
// 18446744073709551611, after which the list endpoint could not read the row
// back (a 500 for everyone). A JSON number past 2^53 has already lost digits,
// so it is refused too; the same value sent as a string is exact (issue #448).
func TestAdminIntegersMustFitTheirField(t *testing.T) {
	runIntRangeAdmin(t, openSQLite(t))
}

func runIntRangeAdmin(t *testing.T, db *database.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	_ = db.Migrator().DropTable(&intRange{})
	if err := db.AutoMigrate(&intRange{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Migrator().DropTable(&intRange{}) })
	app := newCookieAppWithDB(t, db)
	if err := admin.Register(app, intRange{}, admin.Options{Slug: "int-ranges"}); err != nil {
		t.Fatal(err)
	}
	jar := loginSuperuser(t, app)
	const base = "/api/v1/admin/resources/int-ranges"

	for body, field := range map[string]string{
		`{"name":"a","small":300}`:                 "small",
		`{"name":"a","small":-129}`:                "small",
		`{"name":"b","mid":3000000000}`:            "mid",
		`{"name":"c","tiny":-5}`:                   "tiny",
		`{"name":"c","tiny":257}`:                  "tiny",
		`{"name":"c","tiny":"256"}`:                "tiny",
		`{"name":"d","big":1e30}`:                  "big",
		`{"name":"d","big":9007199254740993}`:      "big",
		`{"name":"d","big":"9223372036854775808"}`: "big",
		`{"name":"e","qty":-5}`:                    "qty",
		`{"name":"e","qty":"-5"}`:                  "qty",
		`{"name":"f","opt":40000}`:                 "opt",
	} {
		res := doRequest(app, jar, http.MethodPost, base, body)
		assertError(t, res, http.StatusUnprocessableEntity, contract.CodeValidationError)
		if env := decodeError(t, res); len(env.Fields[field]) == 0 {
			t.Errorf("%s: fields %v, want %q", body, env.Fields, field)
		}
	}

	// The bounds themselves, and a large integer sent as a string, are stored
	// exactly.
	ok := doRequest(app, jar, http.MethodPost, base,
		`{"name":"bounds","small":-128,"mid":2147483647,"qty":0,"tiny":255,"big":"9007199254740993","opt":-32768}`)
	if ok.Code != http.StatusOK {
		t.Fatalf("in-range create: %d %s", ok.Code, ok.Body.String())
	}
	if !strings.Contains(ok.Body.String(), `"big":9007199254740993`) {
		t.Errorf("big sent as a string was not stored exactly: %s", ok.Body.String())
	}
	var row intRange
	if err := db.Where("name = ?", "bounds").First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Small != -128 || row.Mid != 2147483647 || row.Tiny != 255 || row.Big != 9007199254740993 || row.Opt == nil || *row.Opt != -32768 {
		t.Errorf("stored %+v", row)
	}
	patch := doRequest(app, jar, http.MethodPatch, fmt.Sprintf("%s/%d", base, row.ID), `{"small":128}`)
	assertError(t, patch, http.StatusUnprocessableEntity, contract.CodeValidationError)

	if err := db.AutoMigrate(&intDeclared{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Migrator().DropTable(&intDeclared{}) })
	if err := admin.Register(app, intDeclared{}, admin.Options{Slug: "int-declared", Fields: []admin.Field{
		{Name: "id", Type: admin.TypeInteger, ReadOnly: true},
		{Name: "as_float", Type: admin.TypeFloat},
		{Name: "as_json", Type: admin.TypeJSON},
		{Name: "wide", Type: admin.TypeInteger},
	}}); err != nil {
		t.Fatal(err)
	}
	for body, field := range map[string]string{
		`{"as_float":300}`:                "as_float",
		`{"as_float":3.7}`:                "as_float",
		`{"as_json":300}`:                 "as_json",
		`{"wide":"18446744073709551615"}`: "wide",
		`{"wide":-1}`:                     "wide",
	} {
		res := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/int-declared", body)
		assertError(t, res, http.StatusUnprocessableEntity, contract.CodeValidationError)
		if env := decodeError(t, res); len(env.Fields[field]) == 0 {
			t.Errorf("%s: fields %v, want %q", body, env.Fields, field)
		}
	}
	// The message names the admin's limit, not the int64 range: a uint64
	// takes no negative value (#568 review).
	wide := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/int-declared", `{"wide":"18446744073709551615"}`)
	if env := decodeError(t, wide); strings.Join(env.Fields["wide"], " ") != "must be at most 9223372036854775807" {
		t.Errorf("wide: fields %v", env.Fields)
	}
	if res := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/int-declared", `{"as_float":-128,"as_json":127,"wide":"9223372036854775807"}`); res.Code != http.StatusOK {
		t.Fatalf("in-range declared create: %d %s", res.Code, res.Body.String())
	}

	// The admin's form sends a stored integer beyond 2^53 back as a rounded
	// JSON number: that keeps the stored value; another rounded number is
	// still refused (#568 review).
	formPath := fmt.Sprintf("%s/%d", base, row.ID)
	if res := doRequest(app, jar, http.MethodPatch, formPath, `{"name":"from the form","big":9007199254740992}`); res.Code != http.StatusOK {
		t.Fatalf("form resending the stored big integer: %d %s", res.Code, res.Body.String())
	}
	var kept intRange
	if err := db.First(&kept, row.ID).Error; err != nil || kept.Big != 9007199254740993 || kept.Name != "from the form" {
		t.Fatalf("stored %+v (%v), want big 9007199254740993 kept and the name written", kept, err)
	}
	res := doRequest(app, jar, http.MethodPatch, formPath, `{"big":9007199254740996}`)
	assertError(t, res, http.StatusUnprocessableEntity, contract.CodeValidationError)

	// Nothing refused was written, and the list still reads every row.
	if list := doRequest(app, jar, http.MethodGet, base, ""); list.Code != http.StatusOK {
		t.Fatalf("list: %d %s", list.Code, list.Body.String())
	}
	var count int64
	if err := db.Model(&intRange{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("rows = %d (%v), want only the in-range one", count, err)
	}
}
