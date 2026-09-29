package auth_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/config"
	"github.com/gin-gonic/gin"
)

// csrfTestEngine builds a gin engine guarded by CSRFMiddleware with the given
// exempt paths and two POST routes plus a GET route to exercise bootstrap.
func csrfTestEngine(exempt ...string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	cfg := config.DefaultFor(config.EnvironmentTest)
	cfg.Auth.JWTSecret = testJWTSecret

	engine := gin.New()
	engine.Use(auth.CSRFMiddleware(cfg, exempt...))
	ok := func(c *gin.Context) { c.Status(http.StatusOK) }
	engine.POST("/api/v1/webhooks/github", ok)
	engine.POST("/api/v1/products", ok)
	engine.GET("/api/v1/products", ok)
	return engine
}

func TestCSRFMiddlewareExemptPathSkipsEnforcement(t *testing.T) {
	engine := csrfTestEngine("/api/v1/webhooks/github")

	// An exempt POST with no CSRF cookie/header is allowed through so the route
	// handler (which does its own auth) can run.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", strings.NewReader("{}"))
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("exempt POST status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
}

func TestCSRFMiddlewareNonExemptPathStillEnforced(t *testing.T) {
	engine := csrfTestEngine("/api/v1/webhooks/github")

	// A non-exempt POST with no CSRF token is still rejected.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/products", strings.NewReader("{}"))
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-exempt POST status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestCSRFMiddlewareNoExemptionsRejectsAll(t *testing.T) {
	engine := csrfTestEngine() // no exemptions

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", strings.NewReader("{}"))
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unexempted webhook POST status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestCSRFMiddlewareSafeMethodBootstrapsCookie(t *testing.T) {
	engine := csrfTestEngine("/api/v1/webhooks/github")

	// GET is a safe method: it passes and mints the double-submit cookie.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want %d", rec.Code, http.StatusOK)
	}
	var found bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.CSRFCookieName && c.Value != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("GET did not set %s cookie", auth.CSRFCookieName)
	}
}

// TestCSRFMiddlewareExemptPrefix: a prefix exempts the paths under it (the
// storage route's signed uploads), and nothing else: not the prefix
// without its slash, not a sibling path, and exact exemptions stay exact.
func TestCSRFMiddlewareExemptPrefix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := config.DefaultFor(config.EnvironmentTest)
	cfg.Auth.JWTSecret = testJWTSecret
	engine := gin.New()
	engine.Use(auth.CSRFMiddlewareExempting(cfg, []string{"/hooks/exact"}, []string{"/_storage/"}))
	ok := func(c *gin.Context) { c.Status(http.StatusOK) }
	engine.PUT("/_storage/*key", ok)
	engine.PUT("/_storagex/*key", ok)
	engine.PUT("/hooks/*rest", ok)
	for path, want := range map[string]int{
		"/_storage/uploads/a":  http.StatusOK,
		"/_storagex/uploads/a": http.StatusForbidden,
		"/hooks/exact":         http.StatusOK,
		"/hooks/exact/more":    http.StatusForbidden,
	} {
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, path, strings.NewReader("x")))
		if rec.Code != want {
			t.Errorf("PUT %s = %d, want %d", path, rec.Code, want)
		}
	}
}
