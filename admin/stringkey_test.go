package admin_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gombit-dev/gombit/admin"
	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/database"
)

type skShipment struct {
	ID          uint    `gorm:"primaryKey" json:"id"`
	Name        string  `json:"name"`
	CountryCode string  `json:"country_code"`
	AltCode     *string `json:"alt_code"`
	WarehouseID uint    `json:"warehouse_id"`
}

// skProfile shares its string key with the row it extends (one_to_one).
type skProfile struct {
	Code string `gorm:"primaryKey;size:20" json:"code"`
	Bio  string `json:"bio"`
}

// A belongs_to value is coerced as its key column's type (issue #452): a
// string key stays the string sent, byte for byte ("123" was stored as "{",
// "007" as "\a"), on writes and filters alike; an integer key takes the
// admin form's string ("7") and still refuses what is no integer.
func TestAdminStringRelationKeys(t *testing.T) {
	runStringKeyAdmin(t, openSQLite(t))
}

func runStringKeyAdmin(t *testing.T, db *database.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	models := []any{&skShipment{}, &skProfile{}, &relWarehouse{}, &relEngine{}}
	_ = db.Migrator().DropTable("engine_warehouses")
	_ = db.Migrator().DropTable(models...)
	if err := db.AutoMigrate(models...); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Migrator().DropTable("engine_warehouses")
		_ = db.Migrator().DropTable(models...)
	})
	app := newCookieAppWithDB(t, db)
	rel := func(slug string) *admin.Relation {
		return &admin.Relation{Kind: admin.RelBelongsTo, Slug: slug, LabelField: "name"}
	}
	if err := admin.Register(app, skShipment{}, admin.Options{
		Slug: "sk-shipments",
		Fields: []admin.Field{
			{Name: "id", Type: admin.TypeInteger, ReadOnly: true},
			{Name: "name", Type: admin.TypeString},
			{Name: "country_code", Type: admin.TypeRelation, Related: rel("countries")},
			{Name: "alt_code", Type: admin.TypeRelation, Related: rel("countries")},
			{Name: "warehouse_id", Type: admin.TypeRelation, Related: rel("warehouses")},
		},
		Filter: []string{"country_code", "warehouse_id"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := admin.Register(app, skProfile{}, admin.Options{
		Slug: "sk-profiles",
		Fields: []admin.Field{
			{Name: "code", Type: admin.TypeRelation, Related: &admin.Relation{Kind: admin.RelOneToOne, Slug: "countries", LabelField: "name"}},
			{Name: "bio", Type: admin.TypeString},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := admin.Register(app, relWarehouse{}, admin.Options{Slug: "warehouses", Fields: []admin.Field{
		{Name: "id", Type: admin.TypeInteger, ReadOnly: true},
		{Name: "name", Type: admin.TypeString},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := admin.Register(app, relEngine{}, admin.Options{Slug: "engines", Fields: []admin.Field{
		{Name: "id", Type: admin.TypeInteger, ReadOnly: true},
		{Name: "name", Type: admin.TypeString},
		{Name: "warehouses", Type: admin.TypeRelation, Related: &admin.Relation{Kind: admin.RelManyToMany, Slug: "warehouses", LabelField: "name"}},
	}}); err != nil {
		t.Fatal(err)
	}
	jar := loginSuperuser(t, app)
	const base = "/api/v1/admin/resources/sk-shipments"

	for _, c := range []struct{ body, country, alt string }{
		{`{"name":"a","country_code":"US","alt_code":"0042"}`, "US", "0042"},
		{`{"name":"b","country_code":"123"}`, "123", ""},
		{`{"name":"c","country_code":"007","warehouse_id":"7"}`, "007", ""},
	} {
		res := doRequest(app, jar, http.MethodPost, base, c.body)
		if res.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", c.body, res.Code, res.Body.String())
		}
		var created rowEnvelope
		if err := json.Unmarshal(res.Body.Bytes(), &created); err != nil {
			t.Fatal(err)
		}
		var row skShipment
		if err := db.Where("id = ?", asInt(created.Data["id"])).First(&row).Error; err != nil {
			t.Fatal(err)
		}
		alt := ""
		if row.AltCode != nil {
			alt = *row.AltCode
		}
		if row.CountryCode != c.country || alt != c.alt {
			t.Errorf("%s: stored country %q alt %q, want %q %q", c.body, row.CountryCode, alt, c.country, c.alt)
		}
	}
	var withWarehouse skShipment
	if err := db.Where("name = ?", "c").First(&withWarehouse).Error; err != nil || withWarehouse.WarehouseID != 7 {
		t.Errorf("an integer key sent as the form's string: %+v (%v), want warehouse 7", withWarehouse, err)
	}

	// The filter reads a string key as sent.
	list := doRequest(app, jar, http.MethodGet, base+"?country_code=007", "")
	var listed listEnvelope
	if err := json.Unmarshal(list.Body.Bytes(), &listed); err != nil || len(listed.Data) != 1 || listed.Data[0]["name"] != "c" {
		t.Errorf("?country_code=007: %d %s", list.Code, list.Body.String())
	}

	// An integer key refuses what is no integer, rather than reading "1e3"
	// as 1000 or clearing the key for "Infinity".
	for _, bad := range []string{`"1e3"`, `"0x10"`, `"Infinity"`} {
		res := doRequest(app, jar, http.MethodPost, base, `{"name":"x","warehouse_id":`+bad+`}`)
		assertError(t, res, http.StatusUnprocessableEntity, contract.CodeValidationError)
	}
	// A filter on an integer key refuses what is no integer (no database
	// error).
	res := doRequest(app, jar, http.MethodGet, base+"?warehouse_id=abc", "")
	assertError(t, res, http.StatusUnprocessableEntity, contract.CodeValidationError)

	// A string key that is the primary key (a shared-key one_to_one) is
	// stored, read back and edited at the same string.
	if res := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/sk-profiles", `{"code":"007","bio":"spy"}`); res.Code != http.StatusOK {
		t.Fatalf("create profile 007: %d %s", res.Code, res.Body.String())
	}
	if res := doRequest(app, jar, http.MethodGet, "/api/v1/admin/resources/sk-profiles/007", ""); res.Code != http.StatusOK {
		t.Fatalf("GET profile 007: %d %s", res.Code, res.Body.String())
	}
	if res := doRequest(app, jar, http.MethodPatch, "/api/v1/admin/resources/sk-profiles/007", `{"code":"007","bio":"retired"}`); res.Code != http.StatusOK {
		t.Fatalf("PATCH profile 007 (the form sends the key back): %d %s", res.Code, res.Body.String())
	}

	// many_to_many ids arrive as the form's strings for an integer key.
	w1, w2 := relWarehouse{Name: "North"}, relWarehouse{Name: "South"}
	if err := db.Create(&w1).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&w2).Error; err != nil {
		t.Fatal(err)
	}
	engine := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/engines", fmt.Sprintf(`{"name":"V8","warehouses":["%d","%d"]}`, w1.ID, w2.ID))
	if engine.Code != http.StatusOK {
		t.Fatalf("m2m ids as strings: %d %s", engine.Code, engine.Body.String())
	}
	var made rowEnvelope
	if err := json.Unmarshal(engine.Body.Bytes(), &made); err != nil {
		t.Fatal(err)
	}
	if ids := idsOf(t, made.Data["warehouses"]); len(ids) != 2 {
		t.Errorf("m2m ids stored: %v", ids)
	}
	res = doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/engines", `{"name":"V6","warehouses":["abc"]}`)
	assertError(t, res, http.StatusUnprocessableEntity, contract.CodeValidationError)

	// A string key takes a string, as a string field does: a JSON number
	// cannot carry "007"'s zeros, so it is refused rather than guessed.
	res = doRequest(app, jar, http.MethodPost, base, `{"name":"d","country_code":7}`)
	assertError(t, res, http.StatusUnprocessableEntity, contract.CodeValidationError)
}

type skTag struct {
	Code string `gorm:"primaryKey;size:40" json:"code"`
	Name string `json:"name"`
}

// A string primary key preserves leading and trailing whitespace in path IDs
// and filters (issue #694): " sku" is distinct from "sku", and detail,
// update, delete and filter operations target " sku" byte for byte rather
// than trimming it and acting on "sku".
func TestAdminStringKeyWhitespaceInPathIDAndFilter(t *testing.T) {
	runStringKeyWhitespaceAdmin(t, openSQLite(t))
}

func runStringKeyWhitespaceAdmin(t *testing.T, db *database.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	models := []any{&skTag{}}
	_ = db.Migrator().DropTable(models...)
	if err := db.AutoMigrate(models...); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Migrator().DropTable(models...)
	})
	app := newCookieAppWithDB(t, db)
	if err := admin.Register(app, skTag{}, admin.Options{
		Slug: "sk-tags",
		Fields: []admin.Field{
			{Name: "code", Type: admin.TypeString, Required: true},
			{Name: "name", Type: admin.TypeString},
		},
		Filter: []string{"code"},
	}); err != nil {
		t.Fatal(err)
	}
	jar := loginSuperuser(t, app)
	const base = "/api/v1/admin/resources/sk-tags"

	for _, b := range []string{
		`{"code":"sku","name":"exact sku"}`,
		`{"code":" sku","name":"leading space"}`,
		`{"code":"sku ","name":"trailing space"}`,
	} {
		res := doRequest(app, jar, http.MethodPost, base, b)
		if res.Code != http.StatusOK {
			t.Fatalf("POST %s: %d %s", b, res.Code, res.Body.String())
		}
	}

	// Detail (GET) retrieves the exact row matching the encoded string key
	// for leading space (" sku") and trailing space ("sku ").
	for _, tc := range []struct {
		path     string
		wantCode string
		wantName string
	}{
		{"/%20sku", " sku", "leading space"},
		{"/sku%20", "sku ", "trailing space"},
	} {
		res := doRequest(app, jar, http.MethodGet, base+tc.path, "")
		if res.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", tc.path, res.Code, res.Body.String())
		}
		var getEnv rowEnvelope
		if err := json.Unmarshal(res.Body.Bytes(), &getEnv); err != nil {
			t.Fatal(err)
		}
		if getEnv.Data["code"] != tc.wantCode || getEnv.Data["name"] != tc.wantName {
			t.Fatalf("GET %s returned %#v, want code %q name %q", tc.path, getEnv.Data, tc.wantCode, tc.wantName)
		}
	}

	// Filter (?code=) matches the exact row with leading space and trailing space.
	for _, tc := range []struct {
		query    string
		wantCode string
	}{
		{"?code=%20sku", " sku"},
		{"?code=sku%20", "sku "},
	} {
		list := doRequest(app, jar, http.MethodGet, base+tc.query, "")
		var listed listEnvelope
		if err := json.Unmarshal(list.Body.Bytes(), &listed); err != nil || len(listed.Data) != 1 || listed.Data[0]["code"] != tc.wantCode {
			t.Fatalf("GET %s returned %d %s, want only %q", tc.query, list.Code, list.Body.String(), tc.wantCode)
		}
	}

	// Update (PATCH) sends the key back unchanged along with updated fields.
	// On main, this was rejected as a 422 because trimmed lookup loaded "sku".
	// On this head, it must succeed with 200, update " sku" row, and leave "sku" untouched.
	res := doRequest(app, jar, http.MethodPatch, base+"/%20sku", `{"code":" sku","name":"updated leading space"}`)
	if res.Code != http.StatusOK {
		t.Fatalf("PATCH /%%20sku: %d %s", res.Code, res.Body.String())
	}
	var unedited skTag
	if err := db.Where("code = ?", "sku").First(&unedited).Error; err != nil || unedited.Name != "exact sku" {
		t.Fatalf("PATCH /%%20sku inadvertently altered 'sku' row: %+v", unedited)
	}
	var updated skTag
	if err := db.Where("code = ?", " sku").First(&updated).Error; err != nil || updated.Name != "updated leading space" {
		t.Fatalf("PATCH /%%20sku failed to update ' sku' row: %+v", updated)
	}

	// Delete (DELETE) deletes the row with whitespace, leaving "sku" and "sku " intact.
	res = doRequest(app, jar, http.MethodDelete, base+"/%20sku", "")
	if res.Code != http.StatusOK {
		t.Fatalf("DELETE /%%20sku: %d %s", res.Code, res.Body.String())
	}
	var remaining []skTag
	if err := db.Find(&remaining).Error; err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 2 {
		t.Fatalf("expected 2 rows remaining after delete, got %d: %+v", len(remaining), remaining)
	}
	for _, r := range remaining {
		if r.Code == " sku" {
			t.Fatalf("row ' sku' was not deleted: %+v", remaining)
		}
	}
}
