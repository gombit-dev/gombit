package admin_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gombit-dev/gombit/admin"
	"github.com/gombit-dev/gombit/database"
	"gorm.io/gorm"
)

// slashTag has a string primary key, like the tags model in #130/#695.
type slashTag struct {
	Code string `gorm:"primaryKey;size:120" json:"code"`
	Name string `json:"name"`
}

// defaultTag's primary key carries a field default: the value a create stores
// when the request does not name the key at all.
type defaultTag struct {
	Code string `gorm:"primaryKey;size:120" json:"code"`
	Name string `json:"name"`
}

// hookTag derives its primary key in a BeforeCreate hook, so the key is still
// empty when applyWrite reads it and only the check after Create can see the
// value that reaches the row.
type hookTag struct {
	Path  string `gorm:"primaryKey;size:120" json:"path"`
	Title string `json:"title"`
}

func (p *hookTag) BeforeCreate(*gorm.DB) error {
	if p.Path == "" {
		p.Path = strings.ToLower(p.Title)
	}
	return nil
}

// The admin names a row by the {id} path segment, and the router matches on
// the decoded path. An id containing "/" therefore arrives as two segments,
// falls through to NoRoute, and leaves a row that can be listed but never
// opened, edited or deleted. "." and ".." are one segment, but a URL parser
// removes dot segments before sending, so no client can address those either.
// The admin refuses to create such a row rather than leave one it cannot
// address (#695).
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

	refused := []struct{ body, want string }{
		{`{"code":"foo/bar","name":"first"}`, "single URL path segment"},
		{`{"code":".","name":"dot"}`, "a URL parser removes that segment"},
		{`{"code":"..","name":"dotdot"}`, "a URL parser removes that segment"},
	}
	for _, c := range refused {
		rec := doRequest(app, jar, http.MethodPost, base, c.body)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("create %s: status %d, body %s", c.body, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), c.want) {
			t.Fatalf("create %s: field error %q is missing, body %s", c.body, c.want, rec.Body.String())
		}
	}

	// No row was stored, so the listing cannot show one either.
	var stored int64
	if err := db.Model(&slashTag{}).Count(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored != 0 {
		t.Fatalf("the refused creates stored %d row(s)", stored)
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

// A key the request leaves out and a field default supplies is judged as the
// value the row would be created with, not as the empty body value.
func TestAdminRefusesDefaultStringKeyWithSlash(t *testing.T) {
	runDefaultKeyRefusal(t, openSQLite(t))
}

func runDefaultKeyRefusal(t *testing.T, db *database.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	_ = db.Migrator().DropTable(&defaultTag{})
	if err := db.AutoMigrate(&defaultTag{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Migrator().DropTable(&defaultTag{}) })
	app := newCookieAppWithDB(t, db)
	opts := admin.Options{Slug: "default-tags", Fields: []admin.Field{
		{Name: "code", Type: admin.TypeString, Default: "a/b"},
		{Name: "name", Type: admin.TypeString},
	}}
	if err := admin.Register(app, defaultTag{}, opts); err != nil {
		t.Fatal(err)
	}
	jar := loginSuperuser(t, app)
	const base = "/api/v1/admin/resources/default-tags"

	rec := doRequest(app, jar, http.MethodPost, base, `{"name":"first"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create with a default key: status %d, body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "single URL path segment") {
		t.Fatalf("create with a default key: the field error is missing, body %s", rec.Body.String())
	}
	var stored int64
	if err := db.Model(&defaultTag{}).Count(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored != 0 {
		t.Fatalf("create with a default key stored %d row(s)", stored)
	}
}

// A key a model hook writes is the key the INSERT stores, so it is judged as
// well: the request names no key here, and the hook is what puts the "/" in.
func TestAdminRefusesHookStringKeyWithSlash(t *testing.T) {
	runHookKeyRefusal(t, openSQLite(t))
}

func runHookKeyRefusal(t *testing.T, db *database.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	_ = db.Migrator().DropTable(&hookTag{})
	if err := db.AutoMigrate(&hookTag{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Migrator().DropTable(&hookTag{}) })
	app := newCookieAppWithDB(t, db)
	opts := admin.Options{Slug: "hook-tags", Fields: []admin.Field{
		{Name: "path", Type: admin.TypeString},
		{Name: "title", Type: admin.TypeString, Required: true},
	}}
	if err := admin.Register(app, hookTag{}, opts); err != nil {
		t.Fatal(err)
	}
	jar := loginSuperuser(t, app)
	const base = "/api/v1/admin/resources/hook-tags"

	rec := doRequest(app, jar, http.MethodPost, base, `{"title":"Docs/Intro"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create with a hook key: status %d, body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "single URL path segment") {
		t.Fatalf("create with a hook key: the field error is missing, body %s", rec.Body.String())
	}
	var stored int64
	if err := db.Model(&hookTag{}).Count(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored != 0 {
		t.Fatalf("create with a hook key stored %d row(s)", stored)
	}
	// The row the hook would have written is the one the admin cannot reach.
	if rec := doRequest(app, jar, http.MethodGet, base+"/docs%2Fintro", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("GET the refused row: status %d, body %s", rec.Code, rec.Body.String())
	}

	// A hook key the admin can address is still accepted.
	if rec := doRequest(app, jar, http.MethodPost, base, `{"title":"Changelog"}`); rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("create with an addressable hook key: status %d, body %s", rec.Code, rec.Body.String())
	}
	if rec := doRequest(app, jar, http.MethodGet, base+"/changelog", ""); rec.Code != http.StatusOK {
		t.Fatalf("GET the hook-keyed row: status %d, body %s", rec.Code, rec.Body.String())
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
