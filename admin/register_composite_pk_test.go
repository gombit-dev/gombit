package admin_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gombit-dev/gombit/admin"
)

// membership is the #453 shape: a table keyed by (org_id, user_id). Both
// columns are primary and neither auto-increments.
type membership struct {
	OrgID  uint   `gorm:"primaryKey;autoIncrement:false" json:"org_id"`
	UserID uint   `gorm:"primaryKey;autoIncrement:false" json:"user_id"`
	Role   string `json:"role"`
}

func (membership) TableName() string { return "memberships" }

// TestRegisterRejectsCompositePrimaryKey covers the narrow contract: Register
// refuses the model with a clear error, the way resourcegen already does for
// the same shape. An explicit PK must not reopen the hole — naming one key
// column still leaves the row address incomplete.
func TestRegisterRejectsCompositePrimaryKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name string
		opts admin.Options
	}{
		{"derived", admin.Options{Slug: "memberships"}},
		{"explicit pk", admin.Options{Slug: "memberships", PK: "org_id"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := newCookieApp(t)
			err := admin.Register(app, membership{}, tc.opts)
			if err == nil || !strings.Contains(err.Error(), "composite primary key") {
				t.Fatalf("Register() error = %v, want composite primary key", err)
			}
		})
	}
}

// TestCompositePrimaryKeyIsNotAddressable is the data-plane guarantee: the
// rejected model never enters the registry, so no admin route can address it
// and the table stays empty. Before the fix a detail request served the first
// of several rows sharing org_id, a PATCH of the second key column inserted a
// new row, and a DELETE removed whichever row the single key column matched.
func TestCompositePrimaryKeyIsNotAddressable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	if err := app.DB().AutoMigrate(&membership{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	if err := admin.Register(app, membership{}, admin.Options{Slug: "memberships"}); err == nil {
		t.Fatalf("admin.Register accepted a composite primary key")
	}
	jar := loginSuperuser(t, app)

	rec := doRequest(app, jar, http.MethodGet, "/api/v1/admin/meta/memberships", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /admin/meta/memberships status = %d, want 404 (model not registered)", rec.Code)
	}
	rec = doRequest(app, jar, http.MethodGet, "/api/v1/admin/meta", "")
	if strings.Contains(rec.Body.String(), "memberships") {
		t.Errorf("GET /admin/meta lists the rejected slug: %s", rec.Body.String())
	}

	// No create, no detail, no update, no delete: none of the misdirected
	// writes this bug produced can be issued.
	for _, tc := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/api/v1/admin/resources/memberships", `{"org_id":1,"user_id":10,"role":"owner"}`},
		{http.MethodGet, "/api/v1/admin/resources/memberships/1", ""},
		{http.MethodPatch, "/api/v1/admin/resources/memberships/1", `{"user_id":99,"role":"new"}`},
		{http.MethodDelete, "/api/v1/admin/resources/memberships/1", ""},
	} {
		rec := doRequest(app, jar, tc.method, tc.path, tc.body)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s status = %d, want 404", tc.method, tc.path, rec.Code)
		}
	}

	var rows []membership
	if err := app.DB().Order("user_id asc").Find(&rows).Error; err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("rejected model wrote %d rows: %+v", len(rows), rows)
	}
}
