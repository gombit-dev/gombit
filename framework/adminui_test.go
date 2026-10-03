package framework

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gin-gonic/gin"
)

func TestMountAdminSPASkipsWithoutIndex(t *testing.T) {
	previous := gin.Mode()
	t.Cleanup(func() { gin.SetMode(previous) })
	gin.SetMode(gin.TestMode)

	router := gin.New()
	mountAdminSPA(router, fstest.MapFS{".keep": {Data: []byte("")}}, "/api/v1")

	req := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /admin/ status = %d, want %d without index.html", rec.Code, http.StatusNotFound)
	}
}

func TestEmbeddedFrontendDoesNotTakeAdminPath(t *testing.T) {
	app := newTestApp(t, WithEmbeddedFrontend(spaFixtureFS()))

	rec := serveEmbed(t, app, http.MethodGet, "/", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != embedIndexBody {
		t.Fatalf("GET / = %d %q, want app SPA index", rec.Code, rec.Body.String())
	}

	rec = serveEmbed(t, app, http.MethodGet, "/admin/", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /admin/ status = %d, want %d (reserved; JWT/no-cookie app)", rec.Code, http.StatusNotFound)
	}
	if strings.Contains(rec.Body.String(), embedIndexBody) {
		t.Fatal("GET /admin/ served the application SPA index")
	}

	rec = serveEmbed(t, app, http.MethodGet, "/admin/login", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /admin/login status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestAdminSPAHandlerRejectsDotDot(t *testing.T) {
	previous := gin.Mode()
	t.Cleanup(func() { gin.SetMode(previous) })
	gin.SetMode(gin.TestMode)

	fsys := fstest.MapFS{
		"index.html":    {Data: []byte("<!doctype html><div id=\"root\">admin</div>")},
		"assets/app.js": {Data: []byte("console.log('admin')\n")},
	}
	router := gin.New()
	mountAdminSPA(router, fsys, "/api/v1")

	req := httptest.NewRequest(http.MethodGet, "/admin/assets/../assets/app.js", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cleaned asset path status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "console.log('admin')\n" {
		t.Fatalf("cleaned asset path served %q", rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/foo/../../etc/passwd", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("escaped path status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdminSPAHandlerServesIndexAndAsset(t *testing.T) {
	previous := gin.Mode()
	t.Cleanup(func() { gin.SetMode(previous) })
	gin.SetMode(gin.TestMode)

	fsys := fstest.MapFS{
		"index.html":    {Data: []byte("<!doctype html><div id=\"root\">admin</div>")},
		"assets/app.js": {Data: []byte("console.log('admin')\n")},
	}
	router := gin.New()
	mountAdminSPA(router, fsys, "/api/v1")

	for _, path := range []string{"/admin", "/admin/", "/admin/login", "/admin/widgets"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want %d; body=%s", path, rec.Code, http.StatusOK, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Fatalf("GET %s Content-Type = %q, want text/html", path, ct)
		}
		if !strings.Contains(rec.Body.String(), "id=\"root\"") {
			t.Fatalf("GET %s body = %q, want index.html", path, rec.Body.String())
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/assets/app.js", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET asset status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "console.log('admin')\n" {
		t.Fatalf("GET asset body = %q", rec.Body.String())
	}

	for _, path := range []string{"/admin", "/admin/", "/admin/login", "/admin/widgets"} {
		req = httptest.NewRequest(http.MethodHead, path, nil)
		rec = httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("HEAD %s status = %d, want %d", path, rec.Code, http.StatusOK)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Fatalf("HEAD %s Content-Type = %q, want text/html", path, ct)
		}
	}
}

func TestAdminSPAInjectsRuntimeAPIPrefix(t *testing.T) {
	previous := gin.Mode()
	t.Cleanup(func() { gin.SetMode(previous) })
	gin.SetMode(gin.TestMode)

	fsys := fstest.MapFS{
		"index.html": {Data: []byte("<!doctype html><meta name=\"gombit-api-prefix\" content=\"__GOMBIT_API_PREFIX__\"><div id=\"root\">admin</div>")},
	}
	router := gin.New()
	mountAdminSPA(router, fsys, "/svc/v2")

	req := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/ status = %d; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "/svc/v2") {
		t.Fatalf("GET /admin/ missing runtime prefix /svc/v2: %s", body)
	}
	if strings.Contains(body, apiPrefixPlaceholder) {
		t.Fatal("GET /admin/ still contains __GOMBIT_API_PREFIX__ placeholder")
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/config.json", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/config.json status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Fatalf("GET /admin/config.json Content-Type = %q, want json", ct)
	}
	if !strings.Contains(rec.Body.String(), "/svc/v2") {
		t.Fatalf("GET /admin/config.json = %s, want /svc/v2", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "/api/v1") {
		t.Fatalf("GET /admin/config.json still mentions /api/v1: %s", rec.Body.String())
	}
}

func TestAdminSPAAllowsTheStoreOrigin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fsys := fstest.MapFS{"index.html": {Data: []byte("<!doctype html>admin")}}
	router := gin.New()
	mountAdminSPA(router, fsys, "/api/v1", "https://bucket.s3.example.com")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/", nil))
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "img-src 'self' data: https://bucket.s3.example.com") || !strings.Contains(csp, "connect-src 'self' https://bucket.s3.example.com") {
		t.Fatalf("admin CSP = %q", csp)
	}
}
