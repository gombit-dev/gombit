package auth_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	"gorm.io/gorm"
)

const testJWTSecret = "test-jwt-secret-32-bytes-minimum!"
const testPassword = "correct-horse"

type tokenPair struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
}

type dataBody[T any] struct {
	Data T `json:"data"`
}

func TestHandlerTable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newAuthApp(t)

	t.Run("login unknown user", func(t *testing.T) {
		rec := postJSON(t, app, "/api/v1/auth/login", `{"email":"missing@example.com","password":"correct-horse"}`)
		assertError(t, rec, http.StatusUnauthorized, "authentication")
	})

	t.Run("login wrong password", func(t *testing.T) {
		registerUser(t, app, "ada@example.com", testPassword)
		rec := postJSON(t, app, "/api/v1/auth/login", `{"email":"ada@example.com","password":"wrong-horse-1"}`)
		assertError(t, rec, http.StatusUnauthorized, "authentication")
		if !strings.Contains(rec.Body.String(), "invalid email or password") {
			t.Fatalf("body = %s, want invalid email or password", rec.Body.String())
		}
	})

	t.Run("missing bearer", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
		app.Router().ServeHTTP(rec, req)
		assertError(t, rec, http.StatusUnauthorized, "authentication")
		if got := rec.Result().Header.Get("WWW-Authenticate"); got != `Bearer realm="api"` {
			t.Fatalf("WWW-Authenticate = %q, want Bearer realm=%q", got, "api")
		}
	})

	t.Run("invalid bearer", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
		req.Header.Set("Authorization", "Bearer not-a-jwt")
		app.Router().ServeHTTP(rec, req)
		assertError(t, rec, http.StatusUnauthorized, "authentication")
	})

	t.Run("duplicate register", func(t *testing.T) {
		registerUser(t, app, "dup@example.com", testPassword)
		rec := postJSON(t, app, "/api/v1/auth/register", `{"email":"dup@example.com","password":"correct-horse"}`)
		assertError(t, rec, http.StatusConflict, "conflict")
	})

	t.Run("refresh rotation", func(t *testing.T) {
		registerUser(t, app, "rot@example.com", testPassword)
		first := loginUser(t, app, "rot@example.com", testPassword)
		second := refreshUser(t, app, first.RefreshToken)
		if second.AccessToken == "" || second.RefreshToken == "" {
			t.Fatalf("refresh pair missing tokens: %+v", second)
		}
		if second.RefreshToken == first.RefreshToken {
			t.Fatal("refresh token was not rotated")
		}
		if me := getMe(t, app, second.AccessToken); me != http.StatusOK {
			t.Fatalf("GET /me with rotated access status = %d, want 200", me)
		}
		ageRotations(t, app.DB())
		rec := postJSON(t, app, "/api/v1/auth/refresh", `{"refresh_token":"`+first.RefreshToken+`"}`)
		assertError(t, rec, http.StatusUnauthorized, "authentication")
		if me := getMe(t, app, second.AccessToken); me != http.StatusUnauthorized {
			t.Fatalf("GET /me after refresh reuse status = %d, want 401", me)
		}
	})

	// Two tabs share one refresh token; tab 2's refresh with it arrives a
	// round trip after tab 1's rotation committed. Within the grace it is a
	// lost race, not reuse: a 401 that leaves tab 1's new session alive
	// (#459).
	t.Run("two tabs refresh the same token", func(t *testing.T) {
		registerUser(t, app, "tabs@example.com", testPassword)
		shared := loginUser(t, app, "tabs@example.com", testPassword)
		tab1 := refreshUser(t, app, shared.RefreshToken)
		rec := postJSON(t, app, "/api/v1/auth/refresh", `{"refresh_token":"`+shared.RefreshToken+`"}`)
		assertError(t, rec, http.StatusUnauthorized, "authentication")
		if me := getMe(t, app, tab1.AccessToken); me != http.StatusOK {
			t.Fatalf("GET /me with tab 1's new session = %d, want 200: the lost race revoked it", me)
		}
		if next := refreshUser(t, app, tab1.RefreshToken); next.RefreshToken == "" {
			t.Fatal("tab 1's successor token did not rotate")
		}
	})
}

func TestE2ELoginRefreshLogout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newAuthApp(t)
	runBearerE2E(t, app)
}

func TestNewRequiresDatabaseWhenJWTSecretSet(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := config.DefaultFor(config.EnvironmentTest)
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.Auth.JWTSecret = testJWTSecret
	cfg.Auth.BcryptCost = bcrypt.MinCost
	_, err := framework.New(framework.WithConfig(cfg), framework.WithLogger(zap.NewNop()))
	if err == nil {
		t.Fatal("New() error = nil, want missing database error")
	}
	if !strings.Contains(err.Error(), "database") {
		t.Fatalf("New() error = %q, want database message", err)
	}
}

func TestOpenAPIIncludesAuthRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newAuthApp(t)
	rec := httptest.NewRecorder()
	app.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json status = %d; body: %s", rec.Code, rec.Body.String())
	}
	spec := rec.Body.String()
	for _, path := range []string{
		"/api/v1/auth/login",
		"/api/v1/auth/refresh",
		"/api/v1/auth/logout",
		"/api/v1/auth/register",
		"/api/v1/auth/sessions",
		"/api/v1/auth/sessions/{id}",
		"/api/v1/me",
		"bearerAuth",
	} {
		if !strings.Contains(spec, path) {
			t.Fatalf("OpenAPI missing %s; body: %s", path, spec)
		}
	}
}

func runBearerE2E(t *testing.T, app *framework.App) {
	t.Helper()
	registerUser(t, app, "e2e@example.com", testPassword)
	dup := postJSON(t, app, "/api/v1/auth/register", `{"email":"e2e@example.com","password":"correct-horse"}`)
	assertError(t, dup, http.StatusConflict, "conflict")
	pair := loginUser(t, app, "e2e@example.com", testPassword)
	if pair.AccessToken == "" || pair.RefreshToken == "" || pair.TokenType != "Bearer" {
		t.Fatalf("login pair = %+v", pair)
	}

	if status := getMe(t, app, pair.AccessToken); status != http.StatusOK {
		t.Fatalf("GET /me after login status = %d, want 200", status)
	}

	rotated := refreshUser(t, app, pair.RefreshToken)
	if rotated.AccessToken == pair.AccessToken {
		t.Fatal("access token was not refreshed")
	}
	if status := getMe(t, app, rotated.AccessToken); status != http.StatusOK {
		t.Fatalf("GET /me after refresh status = %d, want 200", status)
	}

	logout := postJSON(t, app, "/api/v1/auth/logout", `{"refresh_token":"`+rotated.RefreshToken+`"}`)
	if logout.Code != http.StatusOK {
		t.Fatalf("logout status = %d; body: %s", logout.Code, logout.Body.String())
	}
	if status := getMe(t, app, rotated.AccessToken); status != http.StatusUnauthorized {
		t.Fatalf("GET /me after logout status = %d, want 401", status)
	}
	oldRefresh := postJSON(t, app, "/api/v1/auth/refresh", `{"refresh_token":"`+pair.RefreshToken+`"}`)
	assertError(t, oldRefresh, http.StatusUnauthorized, "authentication")

	// Bearer mode has no session cookies, so a rejected refresh clears none.
	for name, refresh := range map[string]string{
		"rotated":    pair.RefreshToken,
		"logged-out": rotated.RefreshToken,
		"unknown":    strings.Repeat("ab", 32),
	} {
		rec := postJSON(t, app, "/api/v1/auth/refresh", `{"refresh_token":"`+refresh+`"}`)
		assertError(t, rec, http.StatusUnauthorized, "authentication")
		if got := rec.Header().Values("Set-Cookie"); len(got) != 0 {
			t.Fatalf("a Bearer refresh with the %s token set cookies %q, want none", name, got)
		}
	}
}

func newAuthApp(t *testing.T) *framework.App {
	t.Helper()
	return newAuthAppWithDB(t, openSQLite(t))
}

func newAuthAppWithDB(t *testing.T, db *database.DB) *framework.App {
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

// newAuthAppWithTTLs is newAuthAppWithDB in the given auth mode, with the
// given token TTLs.
func newAuthAppWithTTLs(t *testing.T, db *database.DB, mode config.AuthMode, access, refresh time.Duration) *framework.App {
	t.Helper()
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatalf("auth.Migrate() error = %v", err)
	}
	cfg := config.DefaultFor(config.EnvironmentTest)
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.Auth.JWTSecret = testJWTSecret
	cfg.Auth.BcryptCost = bcrypt.MinCost
	cfg.Auth.AccessTokenTTL = access
	cfg.Auth.RefreshTokenTTL = refresh
	cfg.Auth.Mode = mode
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

func openSQLite(t *testing.T) *database.DB {
	t.Helper()
	db, err := database.Open(config.DatabaseConfig{
		Driver: config.DatabaseDriverSQLite,
		DSN:    "file:" + filepath.Join(t.TempDir(), "auth.db") + "?cache=shared&_fk=1",
	})
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func registerUser(t *testing.T, app *framework.App, email, password string) {
	t.Helper()
	body, err := json.Marshal(map[string]string{"email": email, "password": password})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := postJSON(t, app, "/api/v1/auth/register", string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("register status = %d; body: %s", rec.Code, rec.Body.String())
	}
}

func loginUser(t *testing.T, app *framework.App, email, password string) tokenPair {
	t.Helper()
	body, err := json.Marshal(map[string]string{"email": email, "password": password})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := postJSON(t, app, "/api/v1/auth/login", string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var envelope dataBody[tokenPair]
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode login: %v; body: %s", err, rec.Body.String())
	}
	return envelope.Data
}

func refreshUser(t *testing.T, app *framework.App, refresh string) tokenPair {
	t.Helper()
	body, err := json.Marshal(map[string]string{"refresh_token": refresh})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := postJSON(t, app, "/api/v1/auth/refresh", string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var envelope dataBody[tokenPair]
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode refresh: %v; body: %s", err, rec.Body.String())
	}
	return envelope.Data
}

// TestLogoutWithRotatedToken: a logout that carries the refresh token a
// silent refresh has just rotated ends the session it rotated into (#466).
func TestLogoutWithRotatedToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newAuthApp(t)
	registerUser(t, app, "rotated-logout@example.com", testPassword)
	a := loginUser(t, app, "rotated-logout@example.com", testPassword)
	b := refreshUser(t, app, a.RefreshToken)
	rec := postJSON(t, app, "/api/v1/auth/logout", `{"refresh_token":"`+a.RefreshToken+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout with the rotated token = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if me := getMe(t, app, b.AccessToken); me != http.StatusUnauthorized {
		t.Fatalf("GET /me with the session the token rotated into = %d, want 401", me)
	}
	rec = postJSON(t, app, "/api/v1/auth/refresh", `{"refresh_token":"`+b.RefreshToken+`"}`)
	assertError(t, rec, http.StatusUnauthorized, "authentication")

	// A logout whose walk cannot reach the session's head fails with a 500
	// rather than answer 200 with the session alive.
	defer auth.SetMaxChainHops(1)()
	c := loginUser(t, app, "rotated-logout@example.com", testPassword)
	d := refreshUser(t, app, c.RefreshToken)
	rec = postJSON(t, app, "/api/v1/auth/logout", `{"refresh_token":"`+c.RefreshToken+`"}`)
	assertError(t, rec, http.StatusInternalServerError, "internal")
	if me := getMe(t, app, d.AccessToken); me != http.StatusOK {
		t.Fatalf("GET /me after the failed logout = %d, want 200: it ended nothing", me)
	}
}

// TestBearerSchemeIsCaseInsensitive: a valid access token authenticates
// whatever the case of its Authorization scheme (RFC 9110 §11.1), as clients,
// proxies and gateways send `bearer` too (#464).
func TestBearerSchemeIsCaseInsensitive(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newAuthApp(t)
	registerUser(t, app, "scheme@example.com", testPassword)
	pair := loginUser(t, app, "scheme@example.com", testPassword)
	for _, scheme := range []string{"Bearer", "bearer", "BEARER"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
		req.Header.Set("Authorization", scheme+" "+pair.AccessToken)
		app.Router().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("GET /me with scheme %q = %d, want 200; body: %s", scheme, rec.Code, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("Authorization", "Basic "+pair.AccessToken)
	app.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /me with scheme Basic = %d, want 401", rec.Code)
	}
}

func getMe(t *testing.T, app *framework.App, access string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	app.Router().ServeHTTP(rec, req)
	return rec.Code
}

func postJSON(t *testing.T, app *framework.App, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	app.Router().ServeHTTP(rec, req)
	return rec
}

func assertError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, status, rec.Body.String())
	}
	var env contract.ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v; body: %s", err, rec.Body.String())
	}
	if env.Body.Code != code {
		t.Fatalf("error.code = %q, want %q; body: %s", env.Body.Code, code, rec.Body.String())
	}
}

// TestAccountEmailRuleAgreesWithTheRoutes: the service's email rule
// (auth.CheckNewAccount, which Register and CreateSuperuser apply) and the
// login body's format:"email" / maxLength give the same verdict on one
// corpus, both ways. The login route is the one #461 is about, and on an
// empty database it answers from Huma alone: a 422 is the body refused, a
// 401 the body accepted and no such account. The service rule is a second
// copy, so a Huma change to the check fails here, whichever side it moves.
func TestAccountEmailRuleAgreesWithTheRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newAuthApp(t)
	for _, email := range []string{
		"admin", "Name <name@example.com>", `"ab"@example.com`, "trailing@example.com.", "a..b@example.com", "spaced out@example.com",
		"tagged+x@example.com", "UPPER@EXAMPLE.COM", "user@bücher.de",
		strings.Repeat("a", 243) + "@example.com", strings.Repeat("a", 244) + "@example.com", // 255 and 256 characters
	} {
		body, err := json.Marshal(map[string]string{"email": email, "password": testPassword})
		if err != nil {
			t.Fatal(err)
		}
		rec := postJSON(t, app, "/api/v1/auth/login", string(body))
		if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("login %q = %d, want 401 (accepted, no account) or 422 (refused); body: %s", email, rec.Code, rec.Body.String())
		}
		route := rec.Code == http.StatusUnauthorized
		service := auth.CheckNewAccount(email, testPassword) == nil
		if route != service {
			t.Errorf("%q: the login body accepts = %v, the service accepts = %v; they must agree", email, route, service)
		}
	}
}

// ageRotations moves every rotation in db past config.DefaultRefreshReuseGrace, so a
// rotated token presented again is reuse rather than a lost race (#459).
func ageRotations(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := db.Model(&auth.RefreshToken{}).Where("replaced_by IS NOT NULL").
		Update("revoked_at", time.Now().Add(-config.DefaultRefreshReuseGrace-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
}
