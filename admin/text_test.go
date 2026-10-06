package admin_test

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
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
}
