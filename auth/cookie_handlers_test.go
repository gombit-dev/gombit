package auth_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/contract"
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
