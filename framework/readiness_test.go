package framework

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
)

// readyzBody is the union of the ready (data) and not-ready (error) shapes so a
// single decode covers both /readyz outcomes (HOST-2 / ADR-015).
type readyzBody struct {
	Data struct {
		Status string `json:"status"`
	} `json:"data"`
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

func serveReadyz(t *testing.T, app *App) (*httptest.ResponseRecorder, readyzBody) {
	t.Helper()
	rec := httptest.NewRecorder()
	app.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	var body readyzBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal /readyz body %q: %v", rec.Body.String(), err)
	}
	return rec, body
}

// A reachable datastore makes the app ready.
func TestReadyzReadyWithReachableDatabase(t *testing.T) {
	app := newDBApp(t)

	rec, body := serveReadyz(t, app)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /readyz status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if body.Data.Status != "ready" {
		t.Fatalf("GET /readyz data.status = %q, want %q", body.Data.Status, "ready")
	}
}

// An attached but unreachable datastore makes the app not ready: 503 with a D10
// not_ready envelope. This is the signal a host gates traffic on (§23/§24).
func TestReadyzUnreachableDatabaseReturns503(t *testing.T) {
	db, err := database.Open(config.DatabaseConfig{
		Driver: config.DatabaseDriverSQLite,
		DSN:    "file::memory:?cache=shared&_fk=1",
	})
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	app := newTestApp(t, WithDatabase(db))

	// Close the pool so PingContext fails, simulating an unreachable datastore.
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close: %v", err)
	}

	rec, body := serveReadyz(t, app)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz status = %d, want %d; body=%s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
	if body.Error.Code != "not_ready" {
		t.Fatalf("GET /readyz error.code = %q, want %q", body.Error.Code, "not_ready")
	}
	if body.Error.Message == "" {
		t.Fatal("GET /readyz error.message is empty, want a reason")
	}
}

// While draining, readiness reports 503 so a host deregisters the instance
// before in-flight requests finish, even with a healthy datastore.
func TestReadyzDrainingReturns503(t *testing.T) {
	app := newTestApp(t)
	app.draining.Store(true)

	rec, body := serveReadyz(t, app)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if body.Error.Code != "not_ready" {
		t.Fatalf("GET /readyz error.code = %q, want %q", body.Error.Code, "not_ready")
	}
}

// Graceful shutdown flips readiness to draining.
func TestShutdownMarksDraining(t *testing.T) {
	app := newTestApp(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunContext(ctx, app) }()

	waitForHTTP(t, app, "/readyz")
	if app.draining.Load() {
		t.Fatal("app draining before shutdown, want not draining")
	}

	cancel()
	if err := waitRun(done); err != nil {
		t.Fatalf("RunContext() error = %v, want nil", err)
	}
	if !app.draining.Load() {
		t.Fatal("app not draining after shutdown, want draining")
	}
}
