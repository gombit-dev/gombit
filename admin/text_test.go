package admin_test

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gombit-dev/gombit/admin"
	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/framework"
)

// Text no driver stores or compares as given (a NUL byte) is a 422 naming the
// field on the admin's writes, filters and search, on every driver; on
// PostgreSQL each was a 500 (issue #444).
func TestAdminRefusesTextNoDriverHolds(t *testing.T) {
	runTextAdmin(t, openSQLite(t))
}

func runTextAdmin(t *testing.T, db *database.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	app := newCookieAppWithDB(t, db)
	registerWidgets(t, app)
	jar := loginSuperuser(t, app)

	for what, c := range map[string]struct {
		method, path, body, field string
	}{
		"create":  {http.MethodPost, "/api/v1/admin/resources/widgets", `{"name":"a\u0000b"}`, "name"},
		"search":  {http.MethodGet, "/api/v1/admin/resources/widgets?search=a%00b", "", "search"},
		"filter":  {http.MethodGet, "/api/v1/admin/resources/widgets?sku=a%00b", "", "sku"},
		"invalid": {http.MethodGet, "/api/v1/admin/resources/widgets?search=a%FFb", "", "search"},
	} {
		res := doRequest(app, jar, c.method, c.path, c.body)
		assertError(t, res, http.StatusUnprocessableEntity, contract.CodeValidationError)
		if env := decodeError(t, res); len(env.Fields[c.field]) == 0 {
			t.Errorf("%s: fields %v, want %q", what, env.Fields, c.field)
		}
	}
	runTextAdminUnchecked(t, db, app, jar)
	created := doRequest(app, jar, http.MethodPost, "/api/v1/admin/resources/widgets", `{"name":"ok","note":"fine"}`)
	if created.Code != http.StatusOK {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}

	// A row storing text from before the check (more than a text column now
	// holds; a NUL byte MySQL kept) stays editable: a PATCH checks only what
	// it sets (#564 review). Setting the column to refused text is a 422.
	if err := db.AutoMigrate(&legacyNote{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Migrator().DropTable(&legacyNote{}) })
	if err := admin.Register(app, legacyNote{}, admin.Options{Slug: "legacy-notes"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// A NUL byte where the driver stored one (SQLite, MySQL); PostgreSQL
	// refused it, so there the legacy value is text over 65,535 bytes.
	legacy := "legacy\x00body"
	if db.Driver() == database.DriverPostgres {
		legacy = strings.Repeat("b", database.TextMaxBytes+10)
	}
	row := legacyNote{Title: "t", Body: "b"}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("UPDATE legacy_notes SET body = ? WHERE id = ?", legacy, row.ID).Error; err != nil {
		t.Fatalf("store the legacy body: %v", err)
	}
	path := fmt.Sprintf("/api/v1/admin/resources/legacy-notes/%d", row.ID)
	if res := doRequest(app, jar, http.MethodPatch, path, `{"title":"renamed"}`); res.Code != http.StatusOK {
		t.Fatalf("title-only PATCH of a row storing legacy text: %d %s", res.Code, res.Body.String())
	}
	// The admin's form sends every writable field back: the legacy body,
	// unchanged, is not judged again (#564 review round 2).
	form, err := json.Marshal(map[string]any{"title": "from the form", "body": legacy})
	if err != nil {
		t.Fatal(err)
	}
	if res := doRequest(app, jar, http.MethodPatch, path, string(form)); res.Code != http.StatusOK {
		t.Fatalf("full-form PATCH of a row storing legacy text: %d %s", res.Code, res.Body.String())
	}
	res := doRequest(app, jar, http.MethodPatch, path, `{"body":"a\u0000b"}`)
	assertError(t, res, http.StatusUnprocessableEntity, contract.CodeValidationError)
	var stored legacyNote
	if err := db.First(&stored, row.ID).Error; err != nil || stored.Title != "from the form" || stored.Body != legacy {
		t.Fatalf("stored title %q, body kept %v: %v", stored.Title, stored.Body == legacy, err)
	}
}

// txLabel is a string type with its own driver.Valuer, which the database's
// text check does not see as text: the admin's own check covers it.
type txLabel string

func (l txLabel) Value() (driver.Value, error) { return string(l), nil }

type txTag struct {
	Code string `gorm:"primaryKey;size:20" json:"code"`
	Name string `json:"name"`
}

type txPost struct {
	ID    uint    `gorm:"primaryKey" json:"id"`
	Title string  `json:"title"`
	Label txLabel `json:"label"`
	Tags  []txTag `gorm:"many2many:tx_post_tags;" json:"tags"`
}

// A NUL byte where the database's text check does not look is still a 422
// on every driver (#564 review round 3): a many-to-many id, which is compared
// (a PostgreSQL 500), and a string type with its own Valuer (stored on SQLite
// and MySQL). A value such a field already stores, sent back unchanged, is
// kept.
func runTextAdminUnchecked(t *testing.T, db *database.DB, app *framework.App, jar *cookieJar) {
	t.Helper()
	models := []any{&txTag{}, &txPost{}}
	_ = db.Migrator().DropTable("tx_post_tags", &txPost{}, &txTag{})
	if err := db.AutoMigrate(models...); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Migrator().DropTable("tx_post_tags", &txPost{}, &txTag{}) })
	if err := admin.Register(app, txTag{}, admin.Options{Slug: "tx-tags", Fields: []admin.Field{
		{Name: "code", Type: admin.TypeString, Required: true},
		{Name: "name", Type: admin.TypeString},
	}}); err != nil {
		t.Fatalf("Register tags: %v", err)
	}
	if err := admin.Register(app, txPost{}, admin.Options{Slug: "tx-posts", Fields: []admin.Field{
		{Name: "id", Type: admin.TypeInteger, ReadOnly: true},
		{Name: "title", Type: admin.TypeString, Required: true},
		{Name: "label", Type: admin.TypeString},
		{Name: "tags", Type: admin.TypeRelation, Related: &admin.Relation{Kind: admin.RelManyToMany, Slug: "tx-tags", LabelField: "name"}},
	}}); err != nil {
		t.Fatalf("Register posts: %v", err)
	}
	post := txPost{Title: "p", Label: "l"}
	if err := db.Create(&post).Error; err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/v1/admin/resources/tx-posts/%d", post.ID)
	for what, c := range map[string]struct{ method, path, body, field string }{
		"m2m id":       {http.MethodPatch, path, `{"tags":["a\u0000b"]}`, "tags"},
		"Valuer patch": {http.MethodPatch, path, `{"label":"x\u0000y"}`, "label"},
		"Valuer post":  {http.MethodPost, "/api/v1/admin/resources/tx-posts", `{"title":"c","label":"c\u0000v"}`, "label"},
	} {
		res := doRequest(app, jar, c.method, c.path, c.body)
		assertError(t, res, http.StatusUnprocessableEntity, contract.CodeValidationError)
		if env := decodeError(t, res); !strings.Contains(strings.Join(env.Fields[c.field], " "), "NUL") {
			t.Errorf("%s: fields %v, want %q refused for its NUL byte", what, env.Fields, c.field)
		}
	}
	if db.Driver() == database.DriverPostgres {
		return // PostgreSQL never stored a NUL byte
	}
	if err := db.Exec("UPDATE tx_posts SET label = ? WHERE id = ?", "l\x00l", post.ID).Error; err != nil {
		t.Fatalf("store the legacy label: %v", err)
	}
	if res := doRequest(app, jar, http.MethodPatch, path, `{"title":"t2","label":"l\u0000l"}`); res.Code != http.StatusOK {
		t.Fatalf("PATCH sending the stored label back: %d %s", res.Code, res.Body.String())
	}
}

type legacyNote struct {
	ID    uint   `gorm:"primaryKey" json:"id"`
	Title string `json:"title"`
	Body  string `gorm:"type:text" json:"body"`
}
