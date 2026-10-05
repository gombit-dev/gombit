package framework

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gin-gonic/gin"
)

type d10Error struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

// assertD10 checks rec is a JSON D10 error with the given status and code whose
// request_id is the one the response's X-Request-Id header carries.
func assertD10(t *testing.T, label string, rec *httptest.ResponseRecorder, status int, code string) d10Error {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("%s: status = %d, want %d; body=%q", label, rec.Code, status, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("%s: Content-Type = %q, want application/json; body=%q", label, ct, rec.Body.String())
	}
	var body d10Error
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: body %q is not JSON: %v", label, rec.Body.String(), err)
	}
	if body.Error.Code != code {
		t.Fatalf("%s: error.code = %q, want %q", label, body.Error.Code, code)
	}
	if rid := rec.Header().Get(RequestIDHeader); body.Error.RequestID == "" || body.Error.RequestID != rid {
		t.Fatalf("%s: error.request_id = %q, want the X-Request-Id %q", label, body.Error.RequestID, rid)
	}
	return body
}

// TestUnmatchedPathReturnsD10NotFound locks issue #438: an unknown path used to
// get Gin's text/plain "404 page not found", and with an embedded frontend an
// API or reserved path got an empty 404. Both now carry the D10 not_found
// envelope with request_id, like the 405 sibling.
func TestUnmatchedPathReturnsD10NotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)
	spa := fstest.MapFS{"index.html": {Data: []byte(embedIndexBody)}}
	for name, app := range map[string]*App{
		"no frontend":       newTestApp(t),
		"embedded frontend": newTestApp(t, WithEmbeddedFrontend(spa)),
	} {
		for _, req := range []struct{ method, path string }{
			{http.MethodGet, "/api/v1/nope"},
			{http.MethodPost, "/api/v1/nope"},
			{http.MethodGet, "/api"},
			{http.MethodDelete, "/somewhere"},
		} {
			rec := httptest.NewRecorder()
			app.Router().ServeHTTP(rec, httptest.NewRequest(req.method, req.path, nil))
			assertD10(t, name+" "+req.method+" "+req.path, rec, http.StatusNotFound, "not_found")
		}
	}

	// Without a frontend, any unknown path is an API-style 404; with one, an
	// ordinary GET still reaches the SPA fallback.
	rec := httptest.NewRecorder()
	newTestApp(t, WithEmbeddedFrontend(spa)).Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/some/page", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "index") {
		t.Fatalf("SPA fallback: status = %d body=%q, want the index", rec.Code, rec.Body.String())
	}
}

// TestRecoveredPanicReturnsD10Internal locks the other half of issue #438: a
// recovered panic used to be a 500 with an empty body and no Content-Type. It
// is now the D10 internal envelope, without the panic value in it. A handler
// that already started its response keeps it untouched.
func TestRecoveredPanicReturnsD10Internal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newTestApp(t)
	app.Router().GET("/boom", func(*gin.Context) { panic("secret detail") })
	app.Router().GET("/partial", func(c *gin.Context) {
		c.String(http.StatusOK, "partial")
		panic("after write")
	})

	rec := httptest.NewRecorder()
	app.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))
	body := assertD10(t, "GET /boom", rec, http.StatusInternalServerError, "internal")
	if strings.Contains(rec.Body.String(), "secret detail") || body.Error.Message == "" {
		t.Fatalf("GET /boom: body = %q, want a generic message without the panic value", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	app.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/partial", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "partial" {
		t.Fatalf("GET /partial: status = %d body = %q, want the handler's own partial 200 untouched", rec.Code, rec.Body.String())
	}
}
