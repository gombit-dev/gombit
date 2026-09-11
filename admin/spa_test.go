package admin_test

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/framework"
	"github.com/gombit-dev/gombit/internal/adminui"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"
)

func TestAdminSPAServesIndexFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)

	for _, path := range []string{"/admin", "/admin/", "/admin/login", "/admin/widgets"} {
		rec := doRequest(app, nil, http.MethodGet, path, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want %d; body=%s", path, rec.Code, http.StatusOK, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Fatalf("GET %s Content-Type = %q, want text/html", path, ct)
		}
		body := rec.Body.String()
		if !strings.Contains(strings.ToLower(body), "<div id=\"root\">") && !strings.Contains(body, "id=\"root\"") {
			t.Fatalf("GET %s body missing SPA index: %s", path, truncateBody(body))
		}
		csp := rec.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "fonts.googleapis.com") {
			t.Fatalf("GET %s CSP = %q, want SPA font hosts", path, csp)
		}
		if !strings.Contains(csp, "'unsafe-inline'") {
			t.Fatalf("GET %s CSP = %q, want style-src unsafe-inline", path, csp)
		}

		head := doRequest(app, nil, http.MethodHead, path, "")
		if head.Code != http.StatusOK {
			t.Fatalf("HEAD %s status = %d, want %d", path, head.Code, http.StatusOK)
		}
		if ct := head.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Fatalf("HEAD %s Content-Type = %q, want text/html", path, ct)
		}
	}
}

func TestAdminSPAIndexHonorsAPIPrefix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := openSQLite(t)
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatalf("auth.Migrate: %v", err)
	}
	cfg := config.DefaultFor(config.EnvironmentTest)
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.Auth.JWTSecret = testJWTSecret
	cfg.Auth.BcryptCost = bcrypt.MinCost
	cfg.Auth.AccessTokenTTL = time.Minute
	cfg.Auth.RefreshTokenTTL = time.Hour
	cfg.Auth.Mode = config.AuthModeCookie
	cfg.API.Prefix = "/svc/v2"
	app, err := framework.New(
		framework.WithConfig(cfg),
		framework.WithDatabase(db),
		framework.WithLogger(zap.NewNop()),
	)
	if err != nil {
		t.Fatalf("framework.New: %v", err)
	}

	rec := doRequest(app, nil, http.MethodGet, "/admin/", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/ status = %d; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "/svc/v2") {
		t.Fatalf("GET /admin/ missing runtime prefix /svc/v2: %s", truncateBody(body))
	}
	if strings.Contains(body, "__GOMBIT_API_PREFIX__") {
		t.Fatal("GET /admin/ still contains __GOMBIT_API_PREFIX__ placeholder")
	}

	rec = doRequest(app, nil, http.MethodGet, "/admin/config.json", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/config.json status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "/svc/v2") {
		t.Fatalf("GET /admin/config.json = %s, want /svc/v2", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "/api/v1") {
		t.Fatalf("GET /admin/config.json should not mention /api/v1: %s", rec.Body.String())
	}
}

func TestAdminSPAServesHashedAsset(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	asset := firstAdminAsset(t)
	if asset == "" {
		t.Skip("committed dist/ has no assets/ file yet")
	}

	rec := doRequest(app, nil, http.MethodGet, "/admin/"+asset, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/%s status = %d; body=%s", asset, rec.Code, rec.Body.String())
	}
	if rec.Body.Len() == 0 {
		t.Fatalf("GET /admin/%s empty body", asset)
	}
	ct := rec.Header().Get("Content-Type")
	if strings.Contains(ct, "text/html") {
		t.Fatalf("GET /admin/%s Content-Type = %q, want asset not HTML", asset, ct)
	}
}

func TestAdminMetaIsJSONNotHTML(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	registerWidgets(t, app)

	rec := doRequest(app, nil, http.MethodGet, "/api/v1/admin/meta", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET /api/v1/admin/meta status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
	ct := rec.Header().Get("Content-Type")
	if strings.Contains(ct, "text/html") {
		t.Fatalf("GET /api/v1/admin/meta Content-Type = %q, want JSON", ct)
	}
	if strings.Contains(rec.Body.String(), "<div id=\"root\">") {
		t.Fatal("GET /api/v1/admin/meta served admin index.html")
	}
	assertError(t, rec, http.StatusUnauthorized, "authentication")

	jar := loginSuperuser(t, app)
	rec = doRequest(app, jar, http.MethodGet, "/api/v1/admin/meta", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("superuser GET /api/v1/admin/meta status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "json") {
		t.Fatalf("Content-Type = %q, want json", rec.Header().Get("Content-Type"))
	}
	apiCSP := rec.Header().Get("Content-Security-Policy")
	if apiCSP != "default-src 'none'; frame-ancestors 'none'" {
		t.Fatalf("API CSP = %q, want the API policy default-src 'none'; frame-ancestors 'none' (not SPA CSP)", apiCSP)
	}
}

func TestAdminSPALeavesProbesAndDocs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)

	rec := doRequest(app, nil, http.MethodGet, "/readyz", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /readyz status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "<div id=\"root\">") {
		t.Fatal("GET /readyz served admin index.html")
	}
	if rec.Header().Get("Content-Security-Policy") != "default-src 'none'; frame-ancestors 'none'" {
		t.Fatalf("GET /readyz CSP = %q, want the API policy default-src 'none'; frame-ancestors 'none'", rec.Header().Get("Content-Security-Policy"))
	}

	rec = doRequest(app, nil, http.MethodGet, "/docs", "")
	if strings.Contains(rec.Body.String(), "<div id=\"root\">") {
		t.Fatal("GET /docs served admin index.html")
	}
}

func TestJWTAppAdminSPAIs404(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newJWTApp(t)

	rec := doRequest(app, nil, http.MethodGet, "/admin/", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("JWT GET /admin/ status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdminSPAAbsentFromOpenAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newCookieApp(t)
	rec := doRequest(app, nil, http.MethodGet, "/openapi.json", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("openapi status = %d", rec.Code)
	}
	var spec struct {
		Paths map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
		t.Fatalf("decode openapi: %v", err)
	}
	for p := range spec.Paths {
		if p == "/admin" || p == "/admin/" || strings.HasPrefix(p, "/admin/") {
			t.Fatalf("openapi.json includes SPA path %s", p)
		}
	}
	if _, ok := spec.Paths["/api/v1/admin/meta"]; !ok {
		t.Fatal("openapi.json missing /api/v1/admin/meta (Huma admin API should remain)")
	}
}

func TestAdminSPAAndAppEmbedCoexist(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := openSQLite(t)
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatalf("auth.Migrate: %v", err)
	}
	appFS := fstest.MapFS{
		"index.html": {Data: []byte("<!doctype html><title>app</title>app-index")},
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
		framework.WithEmbeddedFrontend(appFS),
		framework.WithLogger(zap.NewNop()),
	)
	if err != nil {
		t.Fatalf("framework.New: %v", err)
	}

	rec := doRequest(app, nil, http.MethodGet, "/", "")
	if rec.Code != http.StatusOK || rec.Body.String() != "<!doctype html><title>app</title>app-index" {
		t.Fatalf("GET / = %d %q, want application embed index", rec.Code, rec.Body.String())
	}

	rec = doRequest(app, nil, http.MethodGet, "/admin/login", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/login status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() == "<!doctype html><title>app</title>app-index" {
		t.Fatal("GET /admin/login served the application embed, want admin SPA")
	}
	if !strings.Contains(rec.Body.String(), "root") {
		t.Fatalf("GET /admin/login body = %q, want admin index", rec.Body.String())
	}
}

func firstAdminAsset(t *testing.T) string {
	t.Helper()
	var found string
	err := fs.WalkDir(adminui.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if strings.HasPrefix(path, "assets/") {
			found = path
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk adminui.FS: %v", err)
	}
	return found
}

func truncateBody(s string) string {
	if len(s) <= 240 {
		return s
	}
	return s[:240]
}
