package framework

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

// A failing datastore probe makes the app not ready: 503 with a D10 not_ready
// envelope. Critically, the raw probe error — which may embed a DSN — must NOT
// reach the public probe; only the fixed reason does. The cause is logged.
func TestReadyzUnreachableDatastoreHides503Cause(t *testing.T) {
	app := newTestApp(t)
	const secret = "dial tcp 10.0.0.5:5432: host=10.0.0.5 user=admin password=hunter2 dbname=prod"
	app.readyProbe = func(context.Context) error { return errors.New(secret) }

	rec, body := serveReadyz(t, app)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz status = %d, want %d; body=%s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
	if body.Error.Code != "not_ready" {
		t.Fatalf("GET /readyz error.code = %q, want %q", body.Error.Code, "not_ready")
	}
	if body.Error.Message != reasonDatastore {
		t.Fatalf("GET /readyz error.message = %q, want fixed reason %q", body.Error.Message, reasonDatastore)
	}
	// The DSN / secret must not leak anywhere in the response body.
	for _, leak := range []string{"hunter2", "user=admin", "host=10.0.0.5", "password", "10.0.0.5:5432"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Fatalf("GET /readyz body leaked %q: %s", leak, rec.Body.String())
		}
	}
}

// The datastore probe is bounded by readinessTimeout: a hung datastore yields a
// prompt 503, not a hung probe.
func TestReadyzProbeTimesOut(t *testing.T) {
	restore := readinessTimeout
	readinessTimeout = 20 * time.Millisecond
	t.Cleanup(func() { readinessTimeout = restore })

	app := newTestApp(t)
	probeReturned := make(chan struct{})
	app.readyProbe = func(ctx context.Context) error {
		<-ctx.Done() // block until the readiness timeout fires
		close(probeReturned)
		return ctx.Err()
	}

	start := time.Now()
	rec, body := serveReadyz(t, app)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("GET /readyz took %v, want it bounded near readinessTimeout", elapsed)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if body.Error.Code != "not_ready" {
		t.Fatalf("GET /readyz error.code = %q, want %q", body.Error.Code, "not_ready")
	}
	select {
	case <-probeReturned:
	case <-time.After(time.Second):
		t.Fatal("probe context was not canceled by readinessTimeout")
	}
}

// While draining, readiness reports 503 with the draining reason even when the
// datastore is healthy, so a host deregisters the instance.
func TestReadyzDrainingReturns503(t *testing.T) {
	app := newDBApp(t) // healthy datastore
	app.draining.Store(true)

	rec, body := serveReadyz(t, app)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if body.Error.Code != "not_ready" {
		t.Fatalf("GET /readyz error.code = %q, want %q", body.Error.Code, "not_ready")
	}
	if body.Error.Message != reasonDraining {
		t.Fatalf("GET /readyz error.message = %q, want %q", body.Error.Message, reasonDraining)
	}
}

// With a drain delay, a host polling /readyz over HTTP observes the 503
// ("shutting down") while the server is still accepting connections — the flag
// is host-visible, not just process-local. This is the window that lets a load
// balancer deregister the instance before any request is connection-refused.
func TestShutdownDrainWindowServes503OverHTTP(t *testing.T) {
	app := newTestApp(t, WithShutdownDrainDelay(500*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunContext(ctx, app) }()

	waitForHTTP(t, app, "/livez")
	cancel() // begin shutdown → draining flips, server keeps serving for 500ms

	client := &http.Client{Timeout: 100 * time.Millisecond}
	deadline := time.Now().Add(time.Second)
	var saw503 bool
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + app.Addr() + "/readyz")
		if err != nil {
			continue // listener closed after the drain window; stop looking below
		}
		var body readyzBody
		_ = json.NewDecoder(resp.Body).Decode(&body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusServiceUnavailable {
			if body.Error.Code != "not_ready" || body.Error.Message != reasonDraining {
				t.Fatalf("drain 503 body = {%q, %q}, want {not_ready, %q}", body.Error.Code, body.Error.Message, reasonDraining)
			}
			saw503 = true
			break
		}
	}
	if !saw503 {
		t.Fatal("host never observed a 503 during the drain window, want a readiness gate")
	}

	if err := waitRun(done); err != nil {
		t.Fatalf("RunContext() error = %v, want nil", err)
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
