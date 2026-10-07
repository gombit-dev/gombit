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
	legacy := strings.Repeat("b", database.TextMaxBytes+10)
	if db.Driver() == database.DriverMySQL {
		legacy = "legacy\x00body"
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
	res := doRequest(app, jar, http.MethodPatch, path, `{"body":"a\u0000b"}`)
	assertError(t, res, http.StatusUnprocessableEntity, contract.CodeValidationError)
	var stored legacyNote
	if err := db.First(&stored, row.ID).Error; err != nil || stored.Title != "renamed" || stored.Body != legacy {
		t.Fatalf("stored title %q, body kept %v: %v", stored.Title, stored.Body == legacy, err)
	}
}

type legacyNote struct {
	ID    uint   `gorm:"primaryKey" json:"id"`
	Title string `json:"title"`
	Body  string `gorm:"type:text" json:"body"`
}
