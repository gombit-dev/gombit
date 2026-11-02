package auth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/internal/faulttest"
)

// TestFault_HTTP_AuthDatabaseFailureIs500: the database fails while a login
// looks the user up, or while the auth gate loads the token's user or its
// session row. Expected outcome: a 500, not the 401 of wrong credentials or a
// dead token, which tells the user their password is wrong, makes the SPAs
// drop the session, and hides the outage from 5xx alerting; no failed login
// is audited; and once the database is back, the same session works (issue
// #460). Both auth modes.
func TestFault_HTTP_AuthDatabaseFailureIs500(t *testing.T) {
	gin.SetMode(gin.TestMode)
	faulttest.ForEachDB(t, faultDBs, func(t *testing.T, kind database.Driver, dsn string) {
		// table is the table whose SELECTs fail while the fault is armed.
		var table atomic.Value
		table.Store("users")
		selects := func(query string) bool {
			unquoted := strings.NewReplacer("`", "", `"`, "").Replace(query)
			return strings.HasPrefix(strings.TrimSpace(strings.ToUpper(query)), "SELECT") &&
				strings.Contains(unquoted, "FROM "+table.Load().(string))
		}

		t.Run("bearer", func(t *testing.T) {
			faults := &faulttest.DBFaults{Statement: faulttest.FailAlways(faulttest.ErrInjected), Match: selects}
			db := openFaultedAuthDB(t, kind, dsn, faults)
			app := newAuthAppWithDB(t, db)
			registerUser(t, app, "blip@example.com", testPassword)
			pair := loginUser(t, app, "blip@example.com", testPassword)

			for _, tbl := range []string{"users", "refresh_tokens"} {
				table.Store(tbl)
				faults.Arm()
				if tbl == "users" {
					rec := postJSON(t, app, "/api/v1/auth/login", `{"email":"blip@example.com","password":"`+testPassword+`"}`)
					assertError(t, rec, http.StatusInternalServerError, "internal")
				}
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
				req.Header.Set("Authorization", "Bearer "+pair.AccessToken)
				app.Router().ServeHTTP(rec, req)
				assertError(t, rec, http.StatusInternalServerError, "internal")
				if got := rec.Header().Get("WWW-Authenticate"); got != "" {
					t.Errorf("%s failing: WWW-Authenticate = %q on a 500", tbl, got)
				}
				faults.Disarm()
			}
			faulttest.Idle(t, db)

			if status := getMe(t, app, pair.AccessToken); status != http.StatusOK {
				t.Fatalf("GET /me once the database is back = %d, want 200: the session must survive", status)
			}
			var failed int64
			if err := db.Model(&auth.AuditEvent{}).Where("action = ?", auth.AuditLoginFailed).Count(&failed).Error; err != nil {
				t.Fatal(err)
			}
			if failed != 0 {
				t.Fatalf("%d failed logins audited, want 0: the database failed, not the credentials", failed)
			}
		})

		t.Run("cookie", func(t *testing.T) {
			table.Store("users")
			faults := &faulttest.DBFaults{Statement: faulttest.FailAlways(faulttest.ErrInjected), Match: selects}
			db, app, jar := newFaultedCookieSession(t, kind, dsn, faults, "blip-cookie@example.com")
			for _, tbl := range []string{"users", "refresh_tokens"} {
				table.Store(tbl)
				other := fetchCSRF(t, app) // a second browser, signing in during the failure
				faults.Arm()
				if tbl == "users" {
					rec := loginCookieUser(t, app, other, "blip-cookie@example.com", testPassword)
					assertError(t, rec, http.StatusInternalServerError, "internal")
				}
				if status := getMeCookie(app, jar); status != http.StatusInternalServerError {
					t.Errorf("%s failing: cookie GET /me = %d, want 500", tbl, status)
				}
				faults.Disarm()
			}
			faulttest.Idle(t, db)
			if status := getMeCookie(app, jar); status != http.StatusOK {
				t.Fatalf("cookie GET /me once the database is back = %d, want 200: the session must survive", status)
			}
		})
	})
}

// openFaultedAuthDB opens dsn through faults (disarmed) with empty auth
// tables, dropped again at cleanup.
func openFaultedAuthDB(t *testing.T, kind database.Driver, dsn string, faults *faulttest.DBFaults) *database.DB {
	t.Helper()
	faults.Disarm()
	db, err := faulttest.OpenDB(kind, dsn, faults)
	if err != nil {
		t.Fatal(err)
	}
	dropAuthTables := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		m := db.WithContext(ctx).Migrator()
		_ = m.DropTable("auth_user_permissions", "auth_user_groups", "auth_group_permissions")
		_ = m.DropTable(auth.Models()...)
	}
	t.Cleanup(func() {
		faults.Disarm()
		dropAuthTables()
		_ = db.Close()
	})
	dropAuthTables()
	return db
}

// TestMissingSessionRowIs401: an access token whose refresh token row is
// gone (an operator purged old rows) is a dead token, a 401, not the 500 of
// a database failure: the only not-found the gate maps to 401 besides the
// user's own row (#460).
func TestMissingSessionRowIs401(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newAuthApp(t)
	registerUser(t, app, "purged@example.com", testPassword)
	pair := loginUser(t, app, "purged@example.com", testPassword)
	if err := app.DB().Where("id = ?", accessClaimsOf(t, pair.AccessToken).Rid).Delete(&auth.RefreshToken{}).Error; err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+pair.AccessToken)
	app.Router().ServeHTTP(rec, req)
	assertError(t, rec, http.StatusUnauthorized, "authentication")
}
