package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/framework"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"
)

// cookieJar is a minimal same-name-wins cookie store for exercising the
// cookie-mode auth surface without pulling in net/http/cookiejar (which
// requires real URLs rather than httptest's in-memory router).
type cookieJar struct {
	cookies map[string]*http.Cookie
}

func newCookieJar() *cookieJar {
	return &cookieJar{cookies: map[string]*http.Cookie{}}
}

func (j *cookieJar) update(rec *httptest.ResponseRecorder) {
	for _, c := range rec.Result().Cookies() {
		j.cookies[c.Name] = c
	}
}

func (j *cookieJar) attach(req *http.Request) {
	for _, c := range j.cookies {
		req.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value}) //nolint:gosec // G124: request Cookie header only carries name/value.
	}
}

func (j *cookieJar) value(name string) string {
	if c, ok := j.cookies[name]; ok {
		return c.Value
	}
	return ""
}

func newCookieAuthApp(t *testing.T) *framework.App {
	t.Helper()
	return newCookieAuthAppWithDB(t, openSQLite(t))
}

func newCookieAuthAppWithDB(t *testing.T, db *database.DB) *framework.App {
	t.Helper()
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatalf("auth.Migrate() error = %v", err)
	}
	cfg := config.DefaultFor(config.EnvironmentTest)
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.Auth.JWTSecret = testJWTSecret
	cfg.Auth.BcryptCost = bcrypt.MinCost
	cfg.Auth.AccessTokenTTL = time.Minute
	cfg.Auth.RefreshTokenTTL = time.Hour
	cfg.Auth.Mode = config.AuthModeCookie
	app, err := framework.New(
		framework.WithConfig(cfg),
		framework.WithDatabase(db),
		framework.WithLogger(zap.NewNop()),
	)
	if err != nil {
		t.Fatalf("framework.New() error = %v", err)
	}
	return app
}

func doRequest(app *framework.App, jar *cookieJar, method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if jar != nil {
		jar.attach(req)
		if csrf := jar.value(auth.CSRFCookieName); csrf != "" {
			req.Header.Set(auth.CSRFHeaderName, csrf)
		}
	}
	app.Router().ServeHTTP(rec, req)
	if jar != nil {
		jar.update(rec)
	}
	return rec
}

func fetchCSRF(t *testing.T, app *framework.App) *cookieJar {
	t.Helper()
	jar := newCookieJar()
	rec := doRequest(app, jar, http.MethodGet, "/api/v1/auth/csrf", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /auth/csrf status = %d; body: %s", rec.Code, rec.Body.String())
	}
	if jar.value(auth.CSRFCookieName) == "" {
		t.Fatalf("GET /auth/csrf did not set %s cookie; headers: %v", auth.CSRFCookieName, rec.Header())
	}
	return jar
}

func csrfBodyToken(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Data struct {
			CSRFToken string `json:"csrf_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode csrf body: %v; body: %s", err, rec.Body.String())
	}
	return env.Data.CSRFToken
}

// TestCSRFBootstrapReusesValidCookie pins the #250 server fix: a re-bootstrap of
// GET /auth/csrf that already carries a valid signed cookie must REUSE that
// token, not rotate it. Rotation is what lets a second tab's bootstrap
// invalidate the token every other tab holds, permanently breaking their writes.
func TestCSRFBootstrapReusesValidCookie(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieAuthApp(t)

	jar := fetchCSRF(t, app)
	first := jar.value(auth.CSRFCookieName)
	if first == "" {
		t.Fatal("first bootstrap set no csrf cookie")
	}

	rec := doRequest(app, jar, http.MethodGet, "/api/v1/auth/csrf", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("re-bootstrap status = %d; body: %s", rec.Code, rec.Body.String())
	}
	if got := jar.value(auth.CSRFCookieName); got != first {
		t.Fatalf("csrf cookie rotated on re-bootstrap: %q -> %q", first, got)
	}
	if tok := csrfBodyToken(t, rec); tok != first {
		t.Fatalf("csrf body token = %q, want reused cookie value %q", tok, first)
	}

	// The reused token still authorizes a write (it matches the shared cookie).
	registerCookieUser(t, app, jar, "csrf-reuse@example.com", testPassword)
	if rec := loginCookieUser(t, app, jar, "csrf-reuse@example.com", testPassword); rec.Code != http.StatusOK {
		t.Fatalf("login with reused csrf token status = %d; body: %s", rec.Code, rec.Body.String())
	}
}

// TestCSRFBootstrapReplacesInvalidCookie verifies a forged/unsigned cookie is not
// trusted: the server mints a fresh signed token rather than echoing the bogus one.
func TestCSRFBootstrapReplacesInvalidCookie(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieAuthApp(t)

	jar := newCookieJar()
	jar.cookies[auth.CSRFCookieName] = &http.Cookie{Name: auth.CSRFCookieName, Value: "forged-not-signed"} //nolint:gosec // G124: test fixture seeds a name/value cookie to exercise the forged-cookie path.

	rec := doRequest(app, jar, http.MethodGet, "/api/v1/auth/csrf", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("bootstrap status = %d; body: %s", rec.Code, rec.Body.String())
	}
	got := jar.value(auth.CSRFCookieName)
	if got == "" || got == "forged-not-signed" {
		t.Fatalf("invalid cookie not replaced: got %q", got)
	}
	if tok := csrfBodyToken(t, rec); tok != got {
		t.Fatalf("body token %q != minted cookie %q", tok, got)
	}

	registerCookieUser(t, app, jar, "csrf-fresh@example.com", testPassword)
	if rec := loginCookieUser(t, app, jar, "csrf-fresh@example.com", testPassword); rec.Code != http.StatusOK {
		t.Fatalf("login after fresh mint status = %d; body: %s", rec.Code, rec.Body.String())
	}
}

func registerCookieUser(t *testing.T, app *framework.App, jar *cookieJar, email, password string) {
	t.Helper()
	body, err := json.Marshal(map[string]string{"email": email, "password": password})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := doRequest(app, jar, http.MethodPost, "/api/v1/auth/register", string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("register status = %d; body: %s", rec.Code, rec.Body.String())
	}
}

func loginCookieUser(t *testing.T, app *framework.App, jar *cookieJar, email, password string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{"email": email, "password": password})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return doRequest(app, jar, http.MethodPost, "/api/v1/auth/login", string(body))
}

func getMeCookie(app *framework.App, jar *cookieJar) int {
	rec := doRequest(app, jar, http.MethodGet, "/api/v1/me", "")
	return rec.Code
}

func TestCookieE2ELoginRefreshLogout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieAuthApp(t)

	jar := fetchCSRF(t, app)
	registerCookieUser(t, app, jar, "cookie-e2e@example.com", testPassword)

	loginRec := loginCookieUser(t, app, jar, "cookie-e2e@example.com", testPassword)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login status = %d; body: %s", loginRec.Code, loginRec.Body.String())
	}
	if jar.value(auth.AccessCookieName) == "" || jar.value(auth.RefreshCookieName) == "" {
		t.Fatalf("login did not set session cookies; jar = %+v", jar.cookies)
	}

	if status := getMeCookie(app, jar); status != http.StatusOK {
		t.Fatalf("GET /me after login status = %d, want 200", status)
	}

	oldAccess := jar.value(auth.AccessCookieName)
	oldRefresh := jar.value(auth.RefreshCookieName)
	refreshRec := doRequest(app, jar, http.MethodPost, "/api/v1/auth/refresh", "")
	if refreshRec.Code != http.StatusOK {
		t.Fatalf("refresh status = %d; body: %s", refreshRec.Code, refreshRec.Body.String())
	}
	if jar.value(auth.AccessCookieName) == oldAccess {
		t.Fatal("access cookie was not rotated")
	}
	if jar.value(auth.RefreshCookieName) == oldRefresh {
		// refresh token rotation always changes the value; this branch
		// should not be reached, but keep the assertion explicit.
		t.Fatal("refresh cookie unexpectedly unchanged")
	}

	if status := getMeCookie(app, jar); status != http.StatusOK {
		t.Fatalf("GET /me after refresh status = %d, want 200", status)
	}

	logoutRec := doRequest(app, jar, http.MethodPost, "/api/v1/auth/logout", "")
	if logoutRec.Code != http.StatusOK {
		t.Fatalf("logout status = %d; body: %s", logoutRec.Code, logoutRec.Body.String())
	}
	if jar.value(auth.AccessCookieName) != "" {
		t.Fatal("logout did not clear access cookie")
	}

	if status := getMeCookie(app, jar); status != http.StatusUnauthorized {
		t.Fatalf("GET /me after logout status = %d, want 401", status)
	}
}

func TestCookieAttributes(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name         string
		sameSite     config.CookieSameSite
		secure       bool
		wantSameSite http.SameSite
	}{
		{name: "lax+secure", sameSite: config.CookieSameSiteLax, secure: true, wantSameSite: http.SameSiteLaxMode},
		{name: "strict+secure", sameSite: config.CookieSameSiteStrict, secure: true, wantSameSite: http.SameSiteStrictMode},
		{name: "lax+insecure", sameSite: config.CookieSameSiteLax, secure: false, wantSameSite: http.SameSiteLaxMode},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openSQLite(t)
			if err := auth.Migrate(db.DB); err != nil {
				t.Fatalf("auth.Migrate() error = %v", err)
			}
			cfg := config.DefaultFor(config.EnvironmentTest)
			cfg.HTTP.Addr = "127.0.0.1:0"
			cfg.Auth.JWTSecret = testJWTSecret
			cfg.Auth.BcryptCost = bcrypt.MinCost
			cfg.Auth.AccessTokenTTL = time.Minute
			cfg.Auth.RefreshTokenTTL = time.Hour
			cfg.Auth.Mode = config.AuthModeCookie
			cfg.Auth.CookieSameSite = tt.sameSite
			cfg.Auth.CookieSecure = tt.secure
			app, err := framework.New(
				framework.WithConfig(cfg),
				framework.WithDatabase(db),
				framework.WithLogger(zap.NewNop()),
			)
			if err != nil {
				t.Fatalf("framework.New() error = %v", err)
			}

			jar := fetchCSRF(t, app)
			registerCookieUser(t, app, jar, "attrs@example.com", testPassword)
			rec := loginCookieUser(t, app, jar, "attrs@example.com", testPassword)
			if rec.Code != http.StatusOK {
				t.Fatalf("login status = %d; body: %s", rec.Code, rec.Body.String())
			}

			cookies := map[string]*http.Cookie{}
			for _, c := range rec.Result().Cookies() {
				cookies[c.Name] = c
			}

			access, ok := cookies[auth.AccessCookieName]
			if !ok {
				t.Fatal("login response missing access cookie")
			}
			if !access.HttpOnly {
				t.Error("access cookie HttpOnly = false, want true")
			}
			if access.Secure != tt.secure {
				t.Errorf("access cookie Secure = %v, want %v", access.Secure, tt.secure)
			}
			if access.SameSite != tt.wantSameSite {
				t.Errorf("access cookie SameSite = %v, want %v", access.SameSite, tt.wantSameSite)
			}

			refresh, ok := cookies[auth.RefreshCookieName]
			if !ok {
				t.Fatal("login response missing refresh cookie")
			}
			if !refresh.HttpOnly {
				t.Error("refresh cookie HttpOnly = false, want true")
			}
			if refresh.Secure != tt.secure {
				t.Errorf("refresh cookie Secure = %v, want %v", refresh.Secure, tt.secure)
			}

			csrf := jar.cookies[auth.CSRFCookieName]
			if csrf == nil {
				t.Fatal("missing csrf cookie in jar")
			}
			if csrf.HttpOnly {
				t.Error("csrf cookie HttpOnly = true, want false (must be JS-readable)")
			}
		})
	}
}

func TestCSRFRejectsStateChangingRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieAuthApp(t)

	t.Run("no csrf cookie or header", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", strings.NewReader(`{"email":"a@example.com","password":"correct-horse"}`))
		req.Header.Set("Content-Type", "application/json")
		app.Router().ServeHTTP(rec, req)
		assertCSRFRejected(t, rec)
	})

	t.Run("csrf cookie without header", func(t *testing.T) {
		jar := fetchCSRF(t, app)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", strings.NewReader(`{"email":"b@example.com","password":"correct-horse"}`))
		req.Header.Set("Content-Type", "application/json")
		jar.attach(req)
		app.Router().ServeHTTP(rec, req)
		assertCSRFRejected(t, rec)
	})

	t.Run("header does not match cookie", func(t *testing.T) {
		jar := fetchCSRF(t, app)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", strings.NewReader(`{"email":"c@example.com","password":"correct-horse"}`))
		req.Header.Set("Content-Type", "application/json")
		jar.attach(req)
		req.Header.Set(auth.CSRFHeaderName, "wrong-value")
		app.Router().ServeHTTP(rec, req)
		assertCSRFRejected(t, rec)
	})

	t.Run("forged unsigned token", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", strings.NewReader(`{"email":"d@example.com","password":"correct-horse"}`))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: "forged-token-not-signed"}) //nolint:gosec // G124: forged CSRF cookie for negative test.
		req.Header.Set(auth.CSRFHeaderName, "forged-token-not-signed")
		app.Router().ServeHTTP(rec, req)
		assertCSRFRejected(t, rec)
	})

	t.Run("valid double-submit token succeeds", func(t *testing.T) {
		jar := fetchCSRF(t, app)
		rec := doRequest(app, jar, http.MethodPost, "/api/v1/auth/register", `{"email":"e@example.com","password":"correct-horse"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("register with valid csrf status = %d, want 200; body: %s", rec.Code, rec.Body.String())
		}
	})
}

// TestCSRFExemptPathThroughNew exercises WithCSRFExemptPaths end-to-end through
// framework.New (not CSRFMiddleware directly), so dropping app.csrfExemptPaths
// at the newRouter -> runtimeMiddlewareStack seam would fail this test.
func TestCSRFExemptPathThroughNew(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := openSQLite(t)
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatalf("auth.Migrate() error = %v", err)
	}
	cfg := config.DefaultFor(config.EnvironmentTest)
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.Auth.JWTSecret = testJWTSecret
	cfg.Auth.Mode = config.AuthModeCookie

	app, err := framework.New(
		framework.WithConfig(cfg),
		framework.WithDatabase(db),
		framework.WithLogger(zap.NewNop()),
		framework.WithCSRFExemptPaths("/api/v1/webhooks/github"),
	)
	if err != nil {
		t.Fatalf("framework.New() error = %v", err)
	}
	// An exempt webhook and a non-exempt neighbor, both registered on the raw
	// engine (as an app would via app.Router()).
	ok := func(c *gin.Context) { c.Status(http.StatusOK) }
	app.Router().POST("/api/v1/webhooks/github", ok)
	app.Router().POST("/api/v1/webhooks/other", ok)

	t.Run("exempt path skips CSRF", func(t *testing.T) {
		rec := doRequest(app, nil, http.MethodPost, "/api/v1/webhooks/github", "{}")
		if rec.Code != http.StatusOK {
			t.Fatalf("exempt POST status = %d, want 200; body: %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("non-exempt neighbor still enforced", func(t *testing.T) {
		rec := doRequest(app, nil, http.MethodPost, "/api/v1/webhooks/other", "{}")
		assertCSRFRejected(t, rec)
	})
}

// TestRawBodyPathThroughNew verifies WithRawBodyPaths end to end: a raw-body
// path is CSRF-exempt (no token needed) AND its JSON body reaches the handler
// unmodified — the XSS sanitizer, which otherwise re-encodes JSON bodies, is
// skipped — so a webhook can verify a signature over the raw body.
func TestRawBodyPathThroughNew(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := openSQLite(t)
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatalf("auth.Migrate() error = %v", err)
	}
	cfg := config.DefaultFor(config.EnvironmentTest)
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.Auth.JWTSecret = testJWTSecret
	cfg.Auth.Mode = config.AuthModeCookie
	// Input sanitization is opt-in (issue #271); enable it here so this test
	// genuinely exercises the raw-body bypass — otherwise the default pipeline
	// has no sanitizer to skip and the assertion would hold vacuously.
	cfg.Security.SanitizeInput = true

	app, err := framework.New(
		framework.WithConfig(cfg),
		framework.WithDatabase(db),
		framework.WithLogger(zap.NewNop()),
		framework.WithRawBodyPaths("/api/v1/webhooks/github"),
	)
	if err != nil {
		t.Fatalf("framework.New() error = %v", err)
	}

	// A body json-re-encoding would change (< / & escaped, <b> stripped).
	const raw = `{"tag":"v1","body":"<b>x</b> a < b & c"}`
	var got string
	app.Router().POST("/api/v1/webhooks/github", func(c *gin.Context) {
		b, _ := io.ReadAll(c.Request.Body)
		got = string(b)
		c.Status(http.StatusOK)
	})

	// No CSRF token attached (jar == nil): raw-body paths are CSRF-exempt too.
	rec := doRequest(app, nil, http.MethodPost, "/api/v1/webhooks/github", raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (CSRF-exempt raw-body path); body: %s", rec.Code, rec.Body.String())
	}
	if got != raw {
		t.Fatalf("handler received %q, want the body unmodified %q", got, raw)
	}

	// A neighbor path is not raw-body: CSRF is still enforced. Guards against a
	// regression that disables CSRF globally whenever any raw-body path is set.
	app.Router().POST("/api/v1/other", func(c *gin.Context) { c.Status(http.StatusOK) })
	rec = doRequest(app, nil, http.MethodPost, "/api/v1/other", raw)
	assertCSRFRejected(t, rec)
}

func TestCSRFSafeMethodsAreExempt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieAuthApp(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	app.Router().ServeHTTP(rec, req)
	// No CSRF cookie yet, but GET is a safe method: CSRF middleware must not
	// block it (the 401 below comes from the missing session cookie, not a
	// CSRF rejection).
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /me status = %d, want 401 (missing session, not CSRF)", rec.Code)
	}
	if rec.Result().Header.Get("Set-Cookie") == "" {
		t.Fatal("safe GET request did not bootstrap a csrf cookie")
	}
	if got := rec.Result().Header.Get("WWW-Authenticate"); got != "" {
		t.Fatalf("cookie-mode 401 WWW-Authenticate = %q, want omit (not Bearer)", got)
	}
}

func TestCookieMe401OmitsBearerChallenge(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieAuthApp(t)

	t.Run("missing session cookie", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
		app.Router().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body: %s", rec.Code, rec.Body.String())
		}
		if got := rec.Result().Header.Get("WWW-Authenticate"); got != "" {
			t.Fatalf("WWW-Authenticate = %q, want omit", got)
		}
		if !strings.Contains(rec.Body.String(), "missing session cookie") {
			t.Fatalf("body = %s, want missing session cookie", rec.Body.String())
		}
	})

	t.Run("invalid session cookie", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
		req.AddCookie(&http.Cookie{Name: auth.AccessCookieName, Value: "not-a-jwt"}) //nolint:gosec // G124: request Cookie header only carries name/value.
		app.Router().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body: %s", rec.Code, rec.Body.String())
		}
		if got := rec.Result().Header.Get("WWW-Authenticate"); got != "" {
			t.Fatalf("WWW-Authenticate = %q, want omit", got)
		}
		if !strings.Contains(rec.Body.String(), "invalid session cookie") {
			t.Fatalf("body = %s, want invalid session cookie", rec.Body.String())
		}
	})
}

// TestCookieRefreshRejected runs runCookieRefreshRejected on SQLite.
func TestCookieRefreshRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runCookieRefreshRejected(t, openSQLite(t))
}

// runCookieRefreshRejected checks that a cookie refresh rejected with a 401
// clears both session cookies, whatever made the refresh token unusable:
// otherwise the browser keeps sending the dead gombit_refresh cookie until
// its Max-Age, and every page load repeats the failed refresh. The cleared
// cookies must be the ones logout sends, with the attributes the login set
// them with: to the browser, a cookie with another Path, Domain, Secure or
// SameSite is a different cookie, and the dead one stays. The integration
// tests run it against PostgreSQL and MySQL.
func runCookieRefreshRejected(t *testing.T, db *database.DB) {
	t.Helper()
	app := newCookieAuthAppWithDB(t, db)
	var n int
	register := func(t *testing.T) string {
		t.Helper()
		n++
		email := fmt.Sprintf("cookie-refresh-%d-%d@example.com", time.Now().UnixNano(), n)
		registerCookieUser(t, app, fetchCSRF(t, app), email, testPassword)
		return email
	}
	// signIn logs email in from a browser of its own and returns that
	// browser's cookies and the login response.
	signIn := func(t *testing.T, email string) (*cookieJar, *httptest.ResponseRecorder) {
		t.Helper()
		jar := fetchCSRF(t, app)
		rec := loginCookieUser(t, app, jar, email, testPassword)
		if rec.Code != http.StatusOK {
			t.Fatalf("login status = %d; body: %s", rec.Code, rec.Body.String())
		}
		return jar, rec
	}
	// withRefresh is jar with its refresh cookie set to value: a stale tab,
	// or a browser that kept a refresh cookie the server has since ended.
	withRefresh := func(jar *cookieJar, value string) *cookieJar {
		jar.cookies[auth.RefreshCookieName] = &http.Cookie{Name: auth.RefreshCookieName, Value: value} //nolint:gosec // G124: request Cookie header only carries name/value.
		return jar
	}

	jar, loginRec := signIn(t, register(t))
	set := map[string]*http.Cookie{}
	for _, c := range loginRec.Result().Cookies() {
		set[c.Name] = c
	}
	if set[auth.AccessCookieName] == nil || set[auth.RefreshCookieName] == nil {
		t.Fatalf("login did not set both session cookies; headers: %v", loginRec.Header())
	}
	logoutRec := doRequest(app, jar, http.MethodPost, "/api/v1/auth/logout", "")
	if logoutRec.Code != http.StatusOK {
		t.Fatalf("logout status = %d; body: %s", logoutRec.Code, logoutRec.Body.String())
	}
	logoutClears := logoutRec.Header().Values("Set-Cookie")
	slices.Sort(logoutClears)

	// rejected refreshes with jar's cookies and fails t unless the response
	// is the 401 error envelope that sets exactly the two session cookies,
	// cleared as logout clears them, and the browser is signed out after it.
	rejected := func(t *testing.T, jar *cookieJar) {
		t.Helper()
		rec := doRequest(app, jar, http.MethodPost, "/api/v1/auth/refresh", "")
		assertError(t, rec, http.StatusUnauthorized, "authentication")
		var env contract.ErrorEnvelope
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		if id := rec.Header().Get(framework.RequestIDHeader); env.Body.RequestID == "" || env.Body.RequestID != id {
			t.Errorf("error.request_id = %q, want the response's %s %q", env.Body.RequestID, framework.RequestIDHeader, id)
		}
		cleared := rec.Result().Cookies()
		if len(cleared) != 2 || cleared[0].Name == cleared[1].Name {
			t.Fatalf("Set-Cookie = %q, want exactly %s and %s, cleared", rec.Header().Values("Set-Cookie"), auth.AccessCookieName, auth.RefreshCookieName)
		}
		for _, c := range cleared {
			want, ok := set[c.Name]
			if !ok || (c.Name != auth.AccessCookieName && c.Name != auth.RefreshCookieName) {
				t.Fatalf("the rejected refresh set %s, which is not a session cookie", c.Name)
			}
			if c.Value != "" || c.MaxAge >= 0 {
				t.Errorf("%s = %q with Max-Age %d, want it cleared (empty, Max-Age=0)", c.Name, c.Value, c.MaxAge)
			}
			if c.Path != want.Path || c.Domain != want.Domain || c.Secure != want.Secure ||
				c.HttpOnly != want.HttpOnly || c.SameSite != want.SameSite {
				t.Errorf("%s cleared with Path %q Domain %q Secure %v HttpOnly %v SameSite %v; login set Path %q Domain %q Secure %v HttpOnly %v SameSite %v",
					c.Name, c.Path, c.Domain, c.Secure, c.HttpOnly, c.SameSite,
					want.Path, want.Domain, want.Secure, want.HttpOnly, want.SameSite)
			}
		}
		got := rec.Header().Values("Set-Cookie")
		slices.Sort(got)
		if !slices.Equal(got, logoutClears) {
			t.Errorf("Set-Cookie = %q, want what logout sends: %q", got, logoutClears)
		}
		if status := getMeCookie(app, jar); status != http.StatusUnauthorized {
			t.Errorf("GET /me after the rejected refresh = %d, want 401", status)
		}
	}

	t.Run("missing refresh cookie", func(t *testing.T) {
		jar, _ := signIn(t, register(t))
		delete(jar.cookies, auth.RefreshCookieName)
		rejected(t, jar) // the access cookie still authenticated: it is cleared too
	})

	t.Run("unknown token", func(t *testing.T) {
		jar, _ := signIn(t, register(t))
		rejected(t, withRefresh(jar, strings.Repeat("ab", 32)))
	})

	t.Run("logged-out token", func(t *testing.T) {
		jar, _ := signIn(t, register(t))
		stale := jar.value(auth.RefreshCookieName)
		if rec := doRequest(app, jar, http.MethodPost, "/api/v1/auth/logout", ""); rec.Code != http.StatusOK {
			t.Fatalf("logout status = %d; body: %s", rec.Code, rec.Body.String())
		}
		rejected(t, withRefresh(jar, stale))
	})

	t.Run("token of a revoked session", func(t *testing.T) {
		email := register(t)
		laptop, _ := signIn(t, email)
		phone, _ := signIn(t, email)
		laptopID, _ := splitCurrent(t, decodeSessions(t, doRequest(app, laptop, http.MethodGet, "/api/v1/auth/sessions", "")))
		if rec := doRequest(app, phone, http.MethodDelete, "/api/v1/auth/sessions/"+laptopID, ""); rec.Code != http.StatusOK {
			t.Fatalf("DELETE the laptop's session status = %d; body: %s", rec.Code, rec.Body.String())
		}
		rejected(t, laptop)
		if status := getMeCookie(app, phone); status != http.StatusOK {
			t.Fatalf("phone GET /me = %d, want 200", status)
		}
	})

	t.Run("expired token", func(t *testing.T) {
		jar, _ := signIn(t, register(t))
		row := accessClaimsOf(t, jar.value(auth.AccessCookieName)).Rid
		if err := db.Model(&auth.RefreshToken{}).Where("id = ?", row).
			Update("expires_at", time.Now().Add(-time.Minute)).Error; err != nil {
			t.Fatal(err)
		}
		rejected(t, jar)
	})

	// Expired and revoked with no successor: nothing to keep, so it clears
	// like the unexpired logged-out token.
	t.Run("expired logged-out token", func(t *testing.T) {
		jar, _ := signIn(t, register(t))
		stale := jar.value(auth.RefreshCookieName)
		row := accessClaimsOf(t, jar.value(auth.AccessCookieName)).Rid
		if rec := doRequest(app, jar, http.MethodPost, "/api/v1/auth/logout", ""); rec.Code != http.StatusOK {
			t.Fatalf("logout status = %d; body: %s", rec.Code, rec.Body.String())
		}
		if err := db.Model(&auth.RefreshToken{}).Where("id = ?", row).
			Update("expires_at", time.Now().Add(-time.Minute)).Error; err != nil {
			t.Fatal(err)
		}
		rejected(t, withRefresh(jar, stale))
	})

	// The reuse cascade still runs: the 401 also ends the user's other
	// sessions, and clears this browser's cookies.
	t.Run("already-rotated token", func(t *testing.T) {
		email := register(t)
		laptop, _ := signIn(t, email)
		phone, _ := signIn(t, email)
		stale := laptop.value(auth.RefreshCookieName)
		if rec := doRequest(app, laptop, http.MethodPost, "/api/v1/auth/refresh", ""); rec.Code != http.StatusOK {
			t.Fatalf("refresh status = %d; body: %s", rec.Code, rec.Body.String())
		}
		rejected(t, withRefresh(fetchCSRF(t, app), stale))
		if status := getMeCookie(app, phone); status != http.StatusUnauthorized {
			t.Fatalf("phone GET /me after the reuse = %d, want 401: the cascade ends every session", status)
		}
		if status := getMeCookie(app, laptop); status != http.StatusUnauthorized {
			t.Fatalf("laptop GET /me after the reuse = %d, want 401: the cascade ends every session", status)
		}
	})

	t.Run("deleted user", func(t *testing.T) {
		jar, rec := signIn(t, register(t))
		var body dataBody[auth.PublicUser]
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Data.ID == 0 {
			t.Fatalf("decode login: %v; body: %s", err, rec.Body.String())
		}
		if err := db.Exec("DELETE FROM users WHERE id = ?", body.Data.ID).Error; err != nil {
			t.Fatal(err)
		}
		rejected(t, jar)
	})
}

// TestExpiredRotatedRefresh runs runExpiredRotatedRefresh on SQLite.
func TestExpiredRotatedRefresh(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runExpiredRotatedRefresh(t, openSQLite(t))
}

// runExpiredRotatedRefresh presents a rotated refresh token again after it
// expired: a second tab whose refresh the server reads after the first tab's
// rotation committed, and serves after the token's expires_at. It is a 401
// without the reuse cascade (an expired token is only invalid), and it is
// errRotationRaced: the token has a successor, which can be the live session
// of the same browser, so the cookie route must not clear the cookies the
// first tab's response set. Only the presented row is expired, and the clock
// does not move: the successor, issued in the same instant, would expire
// with it. The integration tests run it against PostgreSQL and MySQL.
func runExpiredRotatedRefresh(t *testing.T, db *database.DB) {
	t.Helper()
	var n int
	email := func() string {
		n++
		return fmt.Sprintf("expired-rotated-%d-%d@example.com", time.Now().UnixNano(), n)
	}
	expire := func(t *testing.T, id uint) {
		t.Helper()
		if err := db.Model(&auth.RefreshToken{}).Where("id = ?", id).
			Update("expires_at", time.Now().Add(-time.Minute)).Error; err != nil {
			t.Fatal(err)
		}
	}

	t.Run("expired rotated token, service", func(t *testing.T) {
		if err := auth.Migrate(db.DB); err != nil {
			t.Fatal(err)
		}
		cfg := config.DefaultFor(config.EnvironmentTest)
		cfg.Auth.JWTSecret = testJWTSecret
		cfg.Auth.BcryptCost = bcrypt.MinCost
		svc, err := auth.NewService(db.DB, cfg)
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		user, err := svc.Register(ctx, email(), testPassword)
		if err != nil {
			t.Fatal(err)
		}
		stale, err := svc.IssueTokens(ctx, user)
		if err != nil {
			t.Fatal(err)
		}
		other, err := svc.IssueTokens(ctx, user)
		if err != nil {
			t.Fatal(err)
		}
		row, err := auth.AuthenticatedRow(ctx, svc, stale.AccessToken)
		if err != nil {
			t.Fatal(err)
		}
		next, err := svc.RotateRefresh(ctx, stale.RefreshToken)
		if err != nil {
			t.Fatal(err)
		}
		expire(t, row)
		if _, err := svc.RotateRefresh(ctx, stale.RefreshToken); !errors.Is(err, auth.ErrRotationRaced) || !errors.Is(err, auth.ErrInvalidRefreshToken) {
			t.Errorf("RotateRefresh(expired rotated token) = %v, want errRotationRaced, an invalid refresh token: it has a successor", err)
		}
		for name, pair := range map[string]auth.TokenPair{"the successor": next, "the other session": other} {
			if _, err := svc.ParseAccess(ctx, pair.AccessToken); err != nil {
				t.Errorf("%s does not authenticate: %v", name, err)
			}
		}
		if _, err := svc.RotateRefresh(ctx, next.RefreshToken); err != nil {
			t.Errorf("the successor cannot refresh: %v", err)
		}
	})

	t.Run("expired rotated token, cookie refresh", func(t *testing.T) {
		app := newCookieAuthAppWithDB(t, db)
		address := email()
		registerCookieUser(t, app, fetchCSRF(t, app), address, testPassword)
		signIn := func(t *testing.T) *cookieJar {
			t.Helper()
			jar := fetchCSRF(t, app)
			if rec := loginCookieUser(t, app, jar, address, testPassword); rec.Code != http.StatusOK {
				t.Fatalf("login status = %d; body: %s", rec.Code, rec.Body.String())
			}
			return jar
		}
		browser, phone := signIn(t), signIn(t)
		// The second tab sends the refresh cookie the browser holds before
		// the first tab's refresh rotates it.
		tab := newCookieJar()
		for name, c := range browser.cookies {
			tab.cookies[name] = &http.Cookie{Name: c.Name, Value: c.Value} //nolint:gosec // G124: request Cookie header only carries name/value.
		}
		row := accessClaimsOf(t, browser.value(auth.AccessCookieName)).Rid
		if rec := doRequest(app, browser, http.MethodPost, "/api/v1/auth/refresh", ""); rec.Code != http.StatusOK {
			t.Fatalf("refresh status = %d; body: %s", rec.Code, rec.Body.String())
		}
		expire(t, row)

		rec := doRequest(app, tab, http.MethodPost, "/api/v1/auth/refresh", "")
		assertError(t, rec, http.StatusUnauthorized, "authentication")
		assertRequestID(t, rec)
		if got := rec.Header().Values("Set-Cookie"); len(got) != 0 {
			t.Errorf("the second tab's 401 set cookies %q, want none: they would clear the ones the first tab's refresh set", got)
		}
		browser.update(rec)
		if status := getMeCookie(app, browser); status != http.StatusOK {
			t.Errorf("GET /me after the second tab's response = %d, want 200: the browser's session is the successor, and it is live", status)
		}
		if status := getMeCookie(app, phone); status != http.StatusOK {
			t.Errorf("phone GET /me = %d, want 200: an expired token runs no cascade", status)
		}
		if rec := doRequest(app, browser, http.MethodPost, "/api/v1/auth/refresh", ""); rec.Code != http.StatusOK {
			t.Errorf("the successor's refresh status = %d, want 200; body: %s", rec.Code, rec.Body.String())
		}
	})
}

// TestRefreshOpenAPI: the cookie refresh's OpenAPI operation declares the
// 401 whose response clears the session cookies (rejectRefreshCookie), with
// its Set-Cookie header and the D10 error envelope, next to the default
// error response and the 200, which stay what Huma generates for the login.
// Huma does not document headers attached to an error at runtime, so the 401
// is declared on the operation, and a declared 401 stops Huma from adding the
// default, so that is declared too. The Bearer refresh sets no cookie and
// declares neither: its responses are the 200 and the default. An operation
// may document its own error responses, but none may document a Set-Cookie
// header on a response that does not send one: outside a 200, only the cookie
// refresh's 401 sets cookies. The check covers every operation auth.Mount
// registers (each tagged Auth), in both modes; the cookie app also serves the
// admin's operations, tagged Admin, which are the admin package's contract,
// not this one.
func TestRefreshOpenAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)
	type authOperation struct {
		id        string
		responses map[string]json.RawMessage
	}
	// operations reads app's /openapi.json and returns its operations tagged
	// Auth, keyed "METHOD path".
	operations := func(t *testing.T, app *framework.App) map[string]authOperation {
		t.Helper()
		rec := httptest.NewRecorder()
		app.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /openapi.json status = %d; body: %s", rec.Code, rec.Body.String())
		}
		var spec struct {
			Paths map[string]map[string]struct {
				OperationID string                     `json:"operationId"`
				Tags        []string                   `json:"tags"`
				Responses   map[string]json.RawMessage `json:"responses"`
			} `json:"paths"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
			t.Fatalf("decode OpenAPI: %v", err)
		}
		ops := map[string]authOperation{}
		for path, methods := range spec.Paths {
			for method, op := range methods {
				if slices.Contains(op.Tags, "Auth") {
					ops[strings.ToUpper(method)+" "+path] = authOperation{id: op.OperationID, responses: op.Responses}
				}
			}
		}
		return ops
	}
	// noErrorSetsCookies fails t unless no response of an operation in ops
	// other than its 200 declares a Set-Cookie header, except the response
	// keyed allowed ("METHOD path status"; "" allows none), and unless ops
	// holds each operation ID in want, so the Auth filter cannot leave the
	// check with nothing to look at.
	noErrorSetsCookies := func(t *testing.T, ops map[string]authOperation, allowed string, want ...string) {
		t.Helper()
		seen := map[string]bool{}
		for key, op := range ops {
			seen[op.id] = true
			for status, raw := range op.responses {
				if status == "200" || key+" "+status == allowed {
					continue
				}
				var resp struct {
					Headers map[string]json.RawMessage `json:"headers"`
				}
				if err := json.Unmarshal(raw, &resp); err != nil {
					t.Fatalf("decode %s %s response: %v", key, status, err)
				}
				for name := range resp.Headers {
					if strings.EqualFold(name, "Set-Cookie") {
						t.Errorf("%s (%s) %s response declares Set-Cookie: outside a 200, only the cookie refresh's 401 sets cookies", key, op.id, status)
					}
				}
			}
		}
		for _, id := range want {
			if !seen[id] {
				t.Errorf("OpenAPI has no operation %q tagged Auth", id)
			}
		}
	}
	sameJSON := func(t *testing.T, a, b []byte) bool {
		t.Helper()
		var x, y any
		if err := json.Unmarshal(a, &x); err != nil {
			t.Fatalf("decode %s: %v", a, err)
		}
		if err := json.Unmarshal(b, &y); err != nil {
			t.Fatalf("decode %s: %v", b, err)
		}
		return reflect.DeepEqual(x, y)
	}
	type errorResponse struct {
		Headers map[string]struct {
			Description string `json:"description"`
			Schema      struct {
				Type string `json:"type"`
			} `json:"schema"`
		} `json:"headers"`
		Content map[string]struct {
			Schema json.RawMessage `json:"schema"`
		} `json:"content"`
	}
	decode := func(t *testing.T, raw json.RawMessage) errorResponse {
		t.Helper()
		var resp errorResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			t.Fatalf("decode response %s: %v", raw, err)
		}
		return resp
	}
	errorEnvelope := []byte(`{"$ref": "#/components/schemas/ErrorEnvelope"}`)
	isErrorEnvelope := func(t *testing.T, resp errorResponse) bool {
		t.Helper()
		media, ok := resp.Content["application/json"]
		return ok && len(resp.Content) == 1 && sameJSON(t, media.Schema, errorEnvelope)
	}

	t.Run("cookie", func(t *testing.T) {
		ops := operations(t, newCookieAuthApp(t))
		noErrorSetsCookies(t, ops, "POST /api/v1/auth/refresh 401", "csrf", "login", "logout", "refresh", "get-me")
		refresh := ops["POST /api/v1/auth/refresh"].responses
		login := ops["POST /api/v1/auth/login"].responses
		if got := slices.Sorted(maps.Keys(refresh)); !slices.Equal(got, []string{"200", "401", "default"}) {
			t.Fatalf("cookie refresh responses = %v, want 200, 401 and default", got)
		}
		unauthorized := decode(t, refresh["401"])
		setCookie, ok := unauthorized.Headers["Set-Cookie"]
		if !ok || len(unauthorized.Headers) != 1 {
			t.Fatalf("cookie refresh 401 headers = %s, want Set-Cookie", refresh["401"])
		}
		if setCookie.Schema.Type != "string" {
			t.Errorf("cookie refresh 401 Set-Cookie schema type = %q, want string", setCookie.Schema.Type)
		}
		for _, want := range []string{auth.AccessCookieName, auth.RefreshCookieName, "Max-Age=0"} {
			if !strings.Contains(setCookie.Description, want) {
				t.Errorf("cookie refresh 401 Set-Cookie description %q does not mention %s", setCookie.Description, want)
			}
		}
		if !isErrorEnvelope(t, unauthorized) {
			t.Errorf("cookie refresh 401 content = %s, want application/json ErrorEnvelope", refresh["401"])
		}
		if !isErrorEnvelope(t, decode(t, refresh["default"])) {
			t.Errorf("cookie refresh default = %s, want application/json ErrorEnvelope", refresh["default"])
		}
		for _, status := range []string{"200", "default"} {
			if !sameJSON(t, refresh[status], login[status]) {
				t.Errorf("cookie refresh %s = %s, want the login's %s", status, refresh[status], login[status])
			}
		}
	})

	t.Run("bearer", func(t *testing.T) {
		ops := operations(t, newAuthApp(t))
		noErrorSetsCookies(t, ops, "", "login", "logout", "refresh", "get-me")
		refresh := ops["POST /api/v1/auth/refresh"].responses
		if got := slices.Sorted(maps.Keys(refresh)); !slices.Equal(got, []string{"200", "default"}) {
			t.Fatalf("Bearer refresh responses = %v, want 200 and default: it declares no 401", got)
		}
		def := decode(t, refresh["default"])
		if len(def.Headers) != 0 {
			t.Errorf("Bearer refresh default headers = %s, want none", refresh["default"])
		}
		if !isErrorEnvelope(t, def) {
			t.Errorf("Bearer refresh default = %s, want application/json ErrorEnvelope", refresh["default"])
		}
	})
}

// assertRequestID fails t unless rec's error envelope carries the request id
// of rec's request-id header.
func assertRequestID(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	var env contract.ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v; body: %s", err, rec.Body.String())
	}
	if id := rec.Header().Get(framework.RequestIDHeader); env.Body.RequestID == "" || env.Body.RequestID != id {
		t.Errorf("error.request_id = %q, want the response's %s %q", env.Body.RequestID, framework.RequestIDHeader, id)
	}
}

// assertClearsSessionCookies fails t unless rec sets exactly the two session
// cookies, cleared (runCookieRefreshRejected also checks their attributes).
func assertClearsSessionCookies(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	cleared := rec.Result().Cookies()
	names := map[string]bool{}
	for _, c := range cleared {
		names[c.Name] = true
		if c.Value != "" || c.MaxAge >= 0 {
			t.Errorf("%s = %q with Max-Age %d, want it cleared (empty, Max-Age=0)", c.Name, c.Value, c.MaxAge)
		}
	}
	if len(cleared) != 2 || !names[auth.AccessCookieName] || !names[auth.RefreshCookieName] {
		t.Fatalf("Set-Cookie = %q, want exactly %s and %s, cleared", rec.Header().Values("Set-Cookie"), auth.AccessCookieName, auth.RefreshCookieName)
	}
}

// assertSignedOut fails t unless GET /me with jar answers 401 "missing
// session cookie": the browser no longer sends the session's access cookie
// (the jar keeps a cleared cookie with an empty value, which the server
// reads as missing). A jar that still held the access cookie of a session
// that has ended gets "invalid session cookie" instead, so this tells
// cleared cookies from a dead session whose cookies the browser kept.
func assertSignedOut(t *testing.T, app *framework.App, jar *cookieJar) {
	t.Helper()
	rec := doRequest(app, jar, http.MethodGet, "/api/v1/me", "")
	var env contract.ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Errorf("GET /me: decode error envelope: %v; body: %s", err, rec.Body.String())
		return
	}
	if rec.Code != http.StatusUnauthorized || env.Body.Message != "missing session cookie" {
		t.Errorf("GET /me = %d %q, want 401 %q: the browser should hold no session cookie", rec.Code, env.Body.Message, "missing session cookie")
	}
}

func assertCSRFRejected(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body: %s", rec.Code, rec.Body.String())
	}
	var env contract.ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v; body: %s", err, rec.Body.String())
	}
	if env.Body.Code != "authorization" {
		t.Fatalf("error.code = %q, want %q; body: %s", env.Body.Code, "authorization", rec.Body.String())
	}
}
