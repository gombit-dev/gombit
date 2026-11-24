package admin_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gombit-dev/gombit/admin"
	"github.com/gombit-dev/gombit/database"
)

// slashTag has a string primary key, like the tags model in #130/#695.
type slashTag struct {
	Code string `gorm:"primaryKey;size:120" json:"code"`
	Name string `json:"name"`
}

// The admin names a row by the {id} path segment, and the router matches on
// the decoded path. An id containing "/" therefore arrives as two segments,
// falls through to NoRoute, and leaves a row that can be listed but never
// opened, edited or deleted (#695). The admin refuses to create that row rather
// than leave one it cannot address.
func TestAdminRefusesStringKeyWithSlash(t *testing.T) {
	runSlashKeyRefusal(t, openSQLite(t))
}

func runSlashKeyRefusal(t *testing.T, db *database.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	_ = db.Migrator().DropTable(&slashTag{})
	if err := db.AutoMigrate(&slashTag{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Migrator().DropTable(&slashTag{}) })
	app := newCookieAppWithDB(t, db)
	if err := admin.Register(app, slashTag{}, admin.Options{Slug: "slash-tags"}); err != nil {
		t.Fatal(err)
	}
	jar := loginSuperuser(t, app)
	const base = "/api/v1/admin/resources/slash-tags"

	rec := doRequest(app, jar, http.MethodPost, base, `{"code":"foo/bar","name":"first"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create with a slash: status %d, body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "must not contain") {
		t.Fatalf("create with a slash: the field error is missing, body %s", rec.Body.String())
	}

	// No row was stored, so the listing cannot show one either.
	var stored int64
	if err := db.Model(&slashTag{}).Count(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored != 0 {
		t.Fatalf("create with a slash stored %d row(s)", stored)
	}
	if list := doRequest(app, jar, http.MethodGet, base, ""); list.Code != http.StatusOK || strings.Contains(list.Body.String(), "foo/bar") {
		t.Fatalf("list after a refused create: status %d, body %s", list.Code, list.Body.String())
	}

	// An id without a separator still goes through the plain way.
	if rec := doRequest(app, jar, http.MethodPost, base, `{"code":"plain","name":"plain"}`); rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("create plain: status %d, body %s", rec.Code, rec.Body.String())
	}
	if rec := doRequest(app, jar, http.MethodGet, base+"/plain", ""); rec.Code != http.StatusOK {
		t.Fatalf("GET plain: status %d, body %s", rec.Code, rec.Body.String())
	}
}

// A "+" in a string key is not a separator and needs no escaping trick: the
// SPA sends "foo%2Bbar", routing keeps it one segment, and the form decoder
// that a raw-path router would use is not in the picture. This is the case a
// routing-wide change would have broken, so it is pinned here.
func TestAdminStringKeyWithPlusIsAddressable(t *testing.T) {
	runPlusKeyRoundTrip(t, openSQLite(t))
}

func runPlusKeyRoundTrip(t *testing.T, db *database.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	_ = db.Migrator().DropTable(&slashTag{})
	if err := db.AutoMigrate(&slashTag{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Migrator().DropTable(&slashTag{}) })
	app := newCookieAppWithDB(t, db)
	if err := admin.Register(app, slashTag{}, admin.Options{Slug: "slash-tags"}); err != nil {
		t.Fatal(err)
	}
	jar := loginSuperuser(t, app)
	const base = "/api/v1/admin/resources/slash-tags"
	const id = base + "/foo%2Bbar"

	if rec := doRequest(app, jar, http.MethodPost, base, `{"code":"foo+bar","name":"first"}`); rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("create foo+bar: status %d, body %s", rec.Code, rec.Body.String())
	}
	if rec := doRequest(app, jar, http.MethodGet, id, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"foo+bar"`) {
		t.Fatalf("GET %s: status %d, body %s", id, rec.Code, rec.Body.String())
	}
	if rec := doRequest(app, jar, http.MethodPatch, id, `{"name":"renamed"}`); rec.Code != http.StatusOK {
		t.Fatalf("PATCH %s: status %d, body %s", id, rec.Code, rec.Body.String())
	}
	if rec := doRequest(app, jar, http.MethodGet, id, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "renamed") {
		t.Fatalf("GET after PATCH %s: status %d, body %s", id, rec.Code, rec.Body.String())
	}
	if rec := doRequest(app, jar, http.MethodDelete, id, ""); rec.Code != http.StatusOK {
		t.Fatalf("DELETE %s: status %d, body %s", id, rec.Code, rec.Body.String())
	}
}
