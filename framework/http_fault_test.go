package framework

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/internal/faulttest"
)

// TestFault_HTTP_HandlerDeadline: a handler behind HTTP.RequestTimeout that
// calls a hung dependency with its request context ends at the deadline
// (INV-2), and the cancellation reaches the dependency (INV-3): the outbound
// call is abandoned there, not left running, and its body is not leaked.
func TestFault_HTTP_HandlerDeadline(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dep := faulttest.NewHTTPDependency(t, faulttest.Hang(make(chan struct{})))
	tracker := faulttest.TrackBodies(dep.Client().Transport)
	client := &http.Client{Transport: tracker}

	cfg := config.Default()
	cfg.Environment = config.EnvironmentTest
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.HTTP.RequestTimeout = 100 * time.Millisecond
	app, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	outbound := make(chan error, 1)
	app.Router().GET("/proxy", func(c *gin.Context) {
		req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, dep.URL(), nil)
		if err != nil {
			outbound <- err
			c.Status(http.StatusInternalServerError)
			return
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		outbound <- err
		if err != nil {
			c.JSON(http.StatusGatewayTimeout, gin.H{"error": gin.H{"code": "dependency_timeout"}})
			return
		}
		c.Status(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	served := make(chan struct{})
	go func() {
		defer close(served)
		app.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/proxy", nil))
	}()
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		// Closing the dependency (cleanup) releases the handler.
		t.Fatal("the request did not end at the 100ms deadline: it waited on the hung dependency")
	}
	if err := <-outbound; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("outbound call = %v, want context.DeadlineExceeded from the request deadline", err)
	}
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want the handler's 504", rec.Code)
	}
	dep.WaitIdle(t)
	if dep.Abandoned() != 1 {
		t.Fatalf("dependency abandoned calls = %d, want 1: the cancellation did not reach it", dep.Abandoned())
	}
	if tracker.Open() != 0 {
		t.Fatalf("%d response bodies left open", tracker.Open())
	}
}

// TestFault_HTTP_HandlerDeadlineResponseDelivered: the response a handler writes
// when its request deadline fires reaches the client over a real connection
// (issue #430). TestFault_HTTP_HandlerDeadline asserts the 504 through a
// recorder, which has no connection write deadline; this one goes through
// RunContext's http.Server, whose WriteTimeout must outlast the handler
// deadline or the write is dropped and the client sees the connection close.
func TestFault_HTTP_HandlerDeadlineResponseDelivered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := config.Default()
	cfg.Environment = config.EnvironmentTest
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.HTTP.RequestTimeout = 300 * time.Millisecond
	app, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	app.Router().GET("/slow", func(c *gin.Context) {
		// A dependency call that honors the request context returns here.
		<-c.Request.Context().Done()
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": gin.H{
			"code": "timeout", "message": "request timed out", "request_id": GetRequestID(c),
		}})
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunContext(ctx, app) }()
	t.Cleanup(func() {
		cancel()
		if err := waitRun(done); err != nil {
			t.Errorf("RunContext() error = %v, want nil", err)
		}
	})
	waitForHTTP(t, app, "/livez")

	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{}}
	resp, err := client.Get("http://" + app.Addr() + "/slow")
	if err != nil {
		t.Fatalf("the client never received the handler's timeout response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want the handler's 504", resp.StatusCode)
	}
	var body struct {
		Error struct {
			Code      string `json:"code"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode timeout response: %v", err)
	}
	if body.Error.Code != "timeout" || body.Error.RequestID == "" || body.Error.RequestID != resp.Header.Get(RequestIDHeader) {
		t.Fatalf("body = %+v, want the D10 timeout error carrying the X-Request-Id %q", body, resp.Header.Get(RequestIDHeader))
	}
}
