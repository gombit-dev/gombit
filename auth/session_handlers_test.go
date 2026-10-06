package auth_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gin-gonic/gin"

	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/framework"
)

type sessionView struct {
	ID              string `json:"id"`
	LastRefreshedAt string `json:"last_refreshed_at"`
	ExpiresAt       string `json:"expires_at"`
	Current         bool   `json:"current"`
}

func bearerRequest(app *framework.App, method, path, access string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	if access != "" {
		req.Header.Set("Authorization", "Bearer "+access)
	}
	app.Router().ServeHTTP(rec, req)
	return rec
}

func decodeSessions(t *testing.T, rec *httptest.ResponseRecorder) []sessionView {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /auth/sessions status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var envelope dataBody[[]sessionView]
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode sessions: %v; body: %s", err, rec.Body.String())
	}
	return envelope.Data
}

// splitCurrent returns the current session's id and the other sessions' ids,
// failing t unless exactly one session is current.
func splitCurrent(t *testing.T, sessions []sessionView) (string, []string) {
	t.Helper()
	var current string
	var others []string
	for _, s := range sessions {
		if s.ID == "" || s.LastRefreshedAt == "" || s.ExpiresAt == "" {
			t.Fatalf("session missing fields: %+v", s)
		}
		if !s.Current {
			others = append(others, s.ID)
			continue
		}
		if current != "" {
			t.Fatalf("two sessions marked current: %+v", sessions)
		}
		current = s.ID
	}
	if current == "" {
		t.Fatalf("no session marked current: %+v", sessions)
	}
	return current, others
}

func TestSessionsBearer(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newAuthApp(t)
	runBearerSessions(t, app)
}

// runBearerSessions exercises the sessions API end to end in Bearer mode; the
// integration tests run it against PostgreSQL and MySQL.
func runBearerSessions(t *testing.T, app *framework.App) {
	t.Helper()
	const sessions = "/api/v1/auth/sessions"
	registerUser(t, app, "sessions@example.com", testPassword)
	laptop := loginUser(t, app, "sessions@example.com", testPassword)
	phone := loginUser(t, app, "sessions@example.com", testPassword)
	registerUser(t, app, "bystander@example.com", testPassword)
	bystander := loginUser(t, app, "bystander@example.com", testPassword)

	listed := decodeSessions(t, bearerRequest(app, http.MethodGet, sessions, laptop.AccessToken))
	if len(listed) != 2 {
		t.Fatalf("listed %d sessions, want 2 (another user's session must not appear): %+v", len(listed), listed)
	}
	laptopID, others := splitCurrent(t, listed)
	phoneID := others[0]
	if again, _ := splitCurrent(t, decodeSessions(t, bearerRequest(app, http.MethodGet, sessions, phone.AccessToken))); again != phoneID {
		t.Fatalf("phone sees itself as %q, laptop listed it as %q", again, phoneID)
	}

	t.Run("unauthenticated", func(t *testing.T) {
		assertError(t, bearerRequest(app, http.MethodGet, sessions, ""), http.StatusUnauthorized, "authentication")
		assertError(t, bearerRequest(app, http.MethodDelete, sessions+"?scope=all", ""), http.StatusUnauthorized, "authentication")
	})

	t.Run("scope is required and closed", func(t *testing.T) {
		for _, path := range []string{sessions, sessions + "?scope=everyone"} {
			assertError(t, bearerRequest(app, http.MethodDelete, path, laptop.AccessToken), http.StatusUnprocessableEntity, "validation_error")
		}
	})

	t.Run("unknown and foreign ids are not found", func(t *testing.T) {
		bystanderID, _ := splitCurrent(t, decodeSessions(t, bearerRequest(app, http.MethodGet, sessions, bystander.AccessToken)))
		for _, id := range []string{"00000000000000000000000000000000", bystanderID} {
			assertError(t, bearerRequest(app, http.MethodDelete, sessions+"/"+id, laptop.AccessToken), http.StatusNotFound, "not_found")
		}
		if status := getMe(t, app, bystander.AccessToken); status != http.StatusOK {
			t.Fatalf("bystander GET /me = %d, want 200", status)
		}
	})

	t.Run("revoke one", func(t *testing.T) {
		rec := bearerRequest(app, http.MethodDelete, sessions+"/"+phoneID, laptop.AccessToken)
		if rec.Code != http.StatusOK {
			t.Fatalf("DELETE session status = %d; body: %s", rec.Code, rec.Body.String())
		}
		if status := getMe(t, app, phone.AccessToken); status != http.StatusUnauthorized {
			t.Fatalf("revoked phone GET /me = %d, want 401", status)
		}
		// The revoked device refreshing is a dead credential, not reuse: it
		// must not end the laptop's session.
		assertError(t, postJSON(t, app, "/api/v1/auth/refresh", `{"refresh_token":"`+phone.RefreshToken+`"}`), http.StatusUnauthorized, "authentication")
		if status := getMe(t, app, laptop.AccessToken); status != http.StatusOK {
			t.Fatalf("laptop GET /me after the phone's refresh attempt = %d, want 200", status)
		}
		assertError(t, bearerRequest(app, http.MethodDelete, sessions+"/"+phoneID, laptop.AccessToken), http.StatusNotFound, "not_found")
	})

	t.Run("revoke others", func(t *testing.T) {
		tablet := loginUser(t, app, "sessions@example.com", testPassword)
		rec := bearerRequest(app, http.MethodDelete, sessions+"?scope=others", laptop.AccessToken)
		if rec.Code != http.StatusOK {
			t.Fatalf("DELETE scope=others status = %d; body: %s", rec.Code, rec.Body.String())
		}
		if status := getMe(t, app, tablet.AccessToken); status != http.StatusUnauthorized {
			t.Fatalf("tablet GET /me = %d, want 401", status)
		}
		remaining := decodeSessions(t, bearerRequest(app, http.MethodGet, sessions, laptop.AccessToken))
		if current, others := splitCurrent(t, remaining); current != laptopID || len(others) != 0 {
			t.Fatalf("after scope=others: current %q others %v, want only %q", current, others, laptopID)
		}
	})

	t.Run("revoke the current session", func(t *testing.T) {
		own := loginUser(t, app, "sessions@example.com", testPassword)
		ownID, _ := splitCurrent(t, decodeSessions(t, bearerRequest(app, http.MethodGet, sessions, own.AccessToken)))
		rec := bearerRequest(app, http.MethodDelete, sessions+"/"+ownID, own.AccessToken)
		if rec.Code != http.StatusOK {
			t.Fatalf("DELETE own session status = %d; body: %s", rec.Code, rec.Body.String())
		}
		if status := getMe(t, app, own.AccessToken); status != http.StatusUnauthorized {
			t.Fatalf("GET /me after revoking the session = %d, want 401", status)
		}
		assertError(t, postJSON(t, app, "/api/v1/auth/refresh", `{"refresh_token":"`+own.RefreshToken+`"}`), http.StatusUnauthorized, "authentication")
		if status := getMe(t, app, laptop.AccessToken); status != http.StatusOK {
			t.Fatalf("laptop GET /me = %d, want 200: revoking one's own session must not end the others", status)
		}
	})

	t.Run("revoke all", func(t *testing.T) {
		rec := bearerRequest(app, http.MethodDelete, sessions+"?scope=all", laptop.AccessToken)
		if rec.Code != http.StatusOK {
			t.Fatalf("DELETE scope=all status = %d; body: %s", rec.Code, rec.Body.String())
		}
		if status := getMe(t, app, laptop.AccessToken); status != http.StatusUnauthorized {
			t.Fatalf("laptop GET /me after scope=all = %d, want 401", status)
		}
		if status := getMe(t, app, bystander.AccessToken); status != http.StatusOK {
			t.Fatalf("bystander GET /me = %d, want 200", status)
		}
	})
}

func TestSessionsCookie(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runCookieSessions(t, newCookieAuthApp(t))
}

// runCookieSessions exercises the sessions API end to end in cookie mode; the
// integration tests run it against PostgreSQL and MySQL.
func runCookieSessions(t *testing.T, app *framework.App) {
	t.Helper()
	const sessions = "/api/v1/auth/sessions"

	login := func(t *testing.T) *cookieJar {
		t.Helper()
		jar := fetchCSRF(t, app)
		if rec := loginCookieUser(t, app, jar, "cookie-sessions@example.com", testPassword); rec.Code != http.StatusOK {
			t.Fatalf("login status = %d; body: %s", rec.Code, rec.Body.String())
		}
		return jar
	}
	registerCookieUser(t, app, fetchCSRF(t, app), "cookie-sessions@example.com", testPassword)
	laptop := login(t)
	phone := login(t)

	laptopID, others := splitCurrent(t, decodeSessions(t, doRequest(app, laptop, http.MethodGet, sessions, "")))
	if len(others) != 1 {
		t.Fatalf("laptop sees %d other sessions, want 1", len(others))
	}

	t.Run("state-changing calls need the CSRF header", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodDelete, sessions+"?scope=others", nil)
		laptop.attach(req)
		app.Router().ServeHTTP(rec, req)
		assertCSRFRejected(t, rec)
		if status := getMeCookie(app, phone); status != http.StatusOK {
			t.Fatalf("phone GET /me after a CSRF-rejected revoke = %d, want 200", status)
		}
	})

	t.Run("revoking another session keeps the current cookies", func(t *testing.T) {
		tablet := login(t)
		tabletID, _ := splitCurrent(t, decodeSessions(t, doRequest(app, tablet, http.MethodGet, sessions, "")))
		rec := doRequest(app, laptop, http.MethodDelete, sessions+"/"+tabletID, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("DELETE another session status = %d; body: %s", rec.Code, rec.Body.String())
		}
		if got := rec.Result().Cookies(); len(got) != 0 {
			t.Fatalf("revoking another session set cookies %v, want none", got)
		}
		if status := getMeCookie(app, tablet); status != http.StatusUnauthorized {
			t.Fatalf("tablet GET /me = %d, want 401", status)
		}
		if status := getMeCookie(app, laptop); status != http.StatusOK {
			t.Fatalf("laptop GET /me = %d, want 200: it revoked another session, not its own", status)
		}
	})

	t.Run("revoke others keeps the current cookies", func(t *testing.T) {
		rec := doRequest(app, laptop, http.MethodDelete, sessions+"?scope=others", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("DELETE scope=others status = %d; body: %s", rec.Code, rec.Body.String())
		}
		if got := rec.Result().Cookies(); len(got) != 0 {
			t.Fatalf("scope=others set cookies %v, want none", got)
		}
		if status := getMeCookie(app, phone); status != http.StatusUnauthorized {
			t.Fatalf("phone GET /me = %d, want 401", status)
		}
		if status := getMeCookie(app, laptop); status != http.StatusOK {
			t.Fatalf("laptop GET /me = %d, want 200", status)
		}
	})

	t.Run("revoking the current session clears its cookies", func(t *testing.T) {
		rec := doRequest(app, laptop, http.MethodDelete, sessions+"/"+laptopID, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("DELETE own session status = %d; body: %s", rec.Code, rec.Body.String())
		}
		if laptop.value(auth.AccessCookieName) != "" || laptop.value(auth.RefreshCookieName) != "" {
			t.Fatalf("session cookies not cleared: %+v", laptop.cookies)
		}
		if status := getMeCookie(app, laptop); status != http.StatusUnauthorized {
			t.Fatalf("laptop GET /me = %d, want 401", status)
		}
	})

	t.Run("revoke all clears the cookies", func(t *testing.T) {
		tablet := login(t)
		rec := doRequest(app, tablet, http.MethodDelete, sessions+"?scope=all", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("DELETE scope=all status = %d; body: %s", rec.Code, rec.Body.String())
		}
		if tablet.value(auth.AccessCookieName) != "" || tablet.value(auth.RefreshCookieName) != "" {
			t.Fatalf("scope=all did not clear both session cookies: %+v", tablet.cookies)
		}
		cleared := map[string]bool{}
		for _, c := range rec.Result().Cookies() {
			if c.MaxAge < 0 && c.Value == "" {
				cleared[c.Name] = true
			}
		}
		if !cleared[auth.AccessCookieName] || !cleared[auth.RefreshCookieName] {
			t.Fatalf("scope=all Set-Cookie headers %v, want %s and %s both expired", rec.Result().Cookies(), auth.AccessCookieName, auth.RefreshCookieName)
		}
		if status := getMeCookie(app, tablet); status != http.StatusUnauthorized {
			t.Fatalf("tablet GET /me = %d, want 401", status)
		}
	})
}

// TestSessionsOpenAPI: in each auth mode the session operations carry their
// operation IDs and that mode's security scheme (bearerAuth or cookieAuth,
// never both), and DELETE /auth/sessions requires a scope of exactly others
// or all.
func TestSessionExpiryGates(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runSessionExpiryGates(t, openSQLite(t))
}

// runSessionExpiryGates checks the gates every authenticated route sits
// behind, the Bearer one and the cookie one (RequireCookieSession, which the
// admin mounts too): an access token that is still live does not outlive its
// session. Config.Validate accepts an access TTL above the refresh TTL, so
// with a 1h access token and a 1m session, two minutes on the session has
// expired: the gate answers 401 and the route's handler does not run, as
// ParseAccess refuses the token. The integration tests run it against
// PostgreSQL and MySQL.
func runSessionExpiryGates(t *testing.T, db *database.DB) {
	t.Helper()
	tests := []struct {
		name      string
		app       func(*testing.T, *database.DB) *framework.App
		gate      func(*auth.Service) func(huma.Context, func(huma.Context))
		send      func(req *http.Request, access string)
		challenge string
	}{
		{
			name: "bearer",
			app:  newAuthAppWithDB,
			gate: auth.RequireBearer,
			send: func(req *http.Request, access string) {
				req.Header.Set("Authorization", "Bearer "+access)
			},
			challenge: `Bearer realm="api"`,
		},
		{
			name: "cookie",
			app:  newCookieAuthAppWithDB,
			gate: (*auth.Service).RequireCookieSession,
			send: func(req *http.Request, access string) {
				req.AddCookie(&http.Cookie{Name: auth.AccessCookieName, Value: access}) //nolint:gosec // G124: request Cookie header only carries name/value.
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := tt.app(t, db)
			e := newSessionsEnv(t, db, fmt.Sprintf("gate-%s-%d", tt.name, time.Now().UnixNano()), time.Hour, time.Minute)
			clock := &stepClock{now: time.Now()}
			auth.SetClock(e.svc, clock)
			_, pairs := e.user(t, "gate", 1)
			ran := false
			path := "/session-expiry-gate/" + tt.name
			huma.Register(app.API(), huma.Operation{
				OperationID: "session-expiry-gate-" + tt.name,
				Method:      http.MethodGet,
				Path:        path,
				Middlewares: huma.Middlewares{tt.gate(e.svc)},
			}, func(context.Context, *struct{}) (*struct{}, error) {
				ran = true
				return nil, nil
			})
			request := func() *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodGet, path, nil)
				tt.send(req, pairs[0].AccessToken)
				app.Router().ServeHTTP(rec, req)
				return rec
			}

			if rec := request(); rec.Code >= http.StatusMultipleChoices || !ran {
				t.Fatalf("active session: status %d, handler ran %v; want it through; body: %s", rec.Code, ran, rec.Body.String())
			}
			ran = false
			clock.now = clock.now.Add(2 * time.Minute) // the session expires; the access token does not
			rec := request()
			assertError(t, rec, http.StatusUnauthorized, "authentication")
			if ran {
				t.Fatal("the handler ran for an access token whose session has expired")
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != tt.challenge {
				t.Fatalf("WWW-Authenticate = %q, want %q", got, tt.challenge)
			}
		})
	}
}

func TestSessionsOpenAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)
	modes := []struct {
		name   string
		app    func(*testing.T) *framework.App
		scheme string
	}{
		{"bearer", newAuthApp, "bearerAuth"},
		{"cookie", newCookieAuthApp, "cookieAuth"},
	}
	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mode.app(t).Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /openapi.json status = %d; body: %s", rec.Code, rec.Body.String())
			}
			var spec struct {
				Paths map[string]map[string]struct {
					OperationID string                `json:"operationId"`
					Security    []map[string][]string `json:"security"`
					Parameters  []struct {
						Name     string `json:"name"`
						In       string `json:"in"`
						Required bool   `json:"required"`
						Schema   struct {
							Enum []string `json:"enum"`
						} `json:"schema"`
					} `json:"parameters"`
				} `json:"paths"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
				t.Fatalf("decode OpenAPI: %v", err)
			}
			for _, op := range []struct{ path, method, id string }{
				{"/api/v1/auth/sessions", "get", "auth-list-sessions"},
				{"/api/v1/auth/sessions/{id}", "delete", "auth-revoke-session"},
				{"/api/v1/auth/sessions", "delete", "auth-revoke-sessions"},
			} {
				got, ok := spec.Paths[op.path][op.method]
				if !ok {
					t.Fatalf("OpenAPI has no %s %s", op.method, op.path)
				}
				if got.OperationID != op.id {
					t.Errorf("%s %s operationId = %q, want %q", op.method, op.path, got.OperationID, op.id)
				}
				if len(got.Security) != 1 {
					t.Fatalf("%s security = %v, want exactly %s", op.id, got.Security, mode.scheme)
				}
				if _, ok := got.Security[0][mode.scheme]; !ok || len(got.Security[0]) != 1 {
					t.Errorf("%s security = %v, want only %s", op.id, got.Security, mode.scheme)
				}
			}
			revoke := spec.Paths["/api/v1/auth/sessions"]["delete"]
			for _, p := range revoke.Parameters {
				if p.Name != "scope" {
					continue
				}
				if p.In != "query" || !p.Required {
					t.Errorf("scope parameter in=%q required=%v, want a required query parameter", p.In, p.Required)
				}
				if len(p.Schema.Enum) != 2 || p.Schema.Enum[0] != "others" || p.Schema.Enum[1] != "all" {
					t.Errorf("scope enum = %v, want [others all]", p.Schema.Enum)
				}
				return
			}
			t.Fatalf("auth-revoke-sessions has no scope parameter: %+v", revoke.Parameters)
		})
	}
}

// TestSessionsLeaveResourceOperationIDsFree: an app that scaffolds a resource
// named Session (the example `gombit make resource --help` gives) registers
// list-sessions, get-session and create-session on the API auth mounted on,
// since resourcegen/modelhandler.go names a resource's operations
// "list-"+kebab, "get-"+package and "create-"+package. Huma panics on a
// duplicate operation ID, so in either auth mode such an app would not boot if
// the session routes took one of those names.
func TestSessionsLeaveResourceOperationIDsFree(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []struct {
		name string
		app  func(*testing.T) *framework.App
	}{
		{"bearer", newAuthApp},
		{"cookie", newCookieAuthApp},
	} {
		t.Run(mode.name, func(t *testing.T) {
			api := mode.app(t).API()
			for _, op := range []struct{ id, method, path string }{
				{"list-sessions", http.MethodGet, "/api/v1/sessions"},
				{"get-session", http.MethodGet, "/api/v1/sessions/{id}"},
				{"create-session", http.MethodPost, "/api/v1/sessions"},
			} {
				registerResourceOperation(t, api, op.id, op.method, op.path)
			}
		})
	}
}

// registerResourceOperation registers an operation the way a generated
// resource's Register does, failing t instead of panicking.
func registerResourceOperation(t *testing.T, api huma.API, id, method, path string) {
	t.Helper()
	mustRegister(t, "a generated resource's "+id, func() {
		huma.Register(api, huma.Operation{
			OperationID: id,
			Method:      method,
			Path:        path,
			Tags:        []string{"Sessions"},
		}, func(context.Context, *struct{}) (*struct{}, error) { return nil, nil })
	})
}

// Session and RevokeResult stand for an application's own types whose names
// the sessions API's schemas could have taken.
type Session struct {
	Topic string `json:"topic"`
}

type RevokeResult struct {
	Done bool `json:"done"`
}

type appSessionsOutput struct {
	Body contract.Data[[]Session]
}

type appRevokeOutput struct {
	Body contract.Data[RevokeResult]
}

// TestSessionsLeaveAppSchemaNamesFree: Huma names a schema after its Go type,
// without the package, in one registry every operation of the app shares, and
// panics when two different types get the same name. An application with its
// own Session type (a workout session, a chat session) must still boot next
// to the sessions API in either auth mode, also inside the D10 envelope,
// where contract.Data[[]Session] is named DataListSession.
func TestSessionsLeaveAppSchemaNamesFree(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []struct {
		name string
		app  func(*testing.T) *framework.App
	}{
		{"bearer", newAuthApp},
		{"cookie", newCookieAuthApp},
	} {
		t.Run(mode.name, func(t *testing.T) {
			api := mode.app(t).API()
			mustRegister(t, "an app operation returning its own Session list", func() {
				huma.Register(api, huma.Operation{
					OperationID: "list-workout-sessions",
					Method:      http.MethodGet,
					Path:        "/api/v1/workout-sessions",
				}, func(context.Context, *struct{}) (*appSessionsOutput, error) { return nil, nil })
			})
			mustRegister(t, "an app operation returning its own RevokeResult", func() {
				huma.Register(api, huma.Operation{
					OperationID: "revoke-workout-sessions",
					Method:      http.MethodDelete,
					Path:        "/api/v1/workout-sessions",
				}, func(context.Context, *struct{}) (*appRevokeOutput, error) { return nil, nil })
			})
		})
	}
}

// mustRegister runs register, failing t instead of panicking: Huma panics
// when an operation repeats an operation ID or a schema name.
func mustRegister(t *testing.T, what string, register func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("registering %s after auth.Mount: %v", what, r)
		}
	}()
	register()
}
