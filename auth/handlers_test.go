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
		rec := postJSON(t, app, "/api/v1/auth/refresh", `{"refresh_token":"`+first.RefreshToken+`"}`)
		assertError(t, rec, http.StatusUnauthorized, "authentication")
		if me := getMe(t, app, second.AccessToken); me != http.StatusUnauthorized {
			t.Fatalf("GET /me after refresh reuse status = %d, want 401", me)
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
