package framework

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// The per-handler timeout moved into requestContextMiddleware (issue #268): the
// deadline now shares that middleware's single Request.WithContext instead of a
// standalone request_timeout layer's second one. The #242 guard logic lives in
// applyTimeout, so these tests exercise it there — the isolated unit that owns
// the decision — plus the merged middleware end to end.

// TestApplyTimeoutSkipsWrapWhenTighterDeadlineExists locks the issue #242 guard:
// when the context already carries a deadline at or before the timeout we would
// impose, applyTimeout must not wrap it again (a second timer/timerCtx that can
// never fire first). Context identity is the discriminator — a re-wrap would
// return a distinct child even though the effective deadline is unchanged.
func TestApplyTimeoutSkipsWrapWhenTighterDeadlineExists(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	got, gotCancel := applyTimeout(parent, time.Hour)
	defer gotCancel()

	if got != parent {
		t.Fatal("applyTimeout re-wrapped a context that already had a tighter deadline; guard did not fire")
	}
}

// TestApplyTimeoutImposesDeadlineWhenNonePresent is the other half: a context
// with no deadline (the common case) must come back bounded, and it must be a
// fresh context, not the original.
func TestApplyTimeoutImposesDeadlineWhenNonePresent(t *testing.T) {
	parent := context.Background()

	got, cancel := applyTimeout(parent, time.Hour)
	defer cancel()

	if got == parent {
		t.Fatal("applyTimeout left the original context in place")
	}
	if _, ok := got.Deadline(); !ok {
		t.Fatal("applyTimeout did not impose a deadline on a context that had none")
	}
}

// TestApplyTimeoutDisabledIsNoop confirms a non-positive timeout leaves the
// context untouched and imposes no deadline.
func TestApplyTimeoutDisabledIsNoop(t *testing.T) {
	parent := context.Background()

	got, cancel := applyTimeout(parent, 0)
	defer cancel()

	if got != parent {
		t.Fatal("disabled timeout wrapped the context")
	}
	if _, ok := got.Deadline(); ok {
		t.Fatal("disabled timeout imposed a deadline")
	}
}

// TestRequestContextMiddlewareImposesConfiguredTimeout proves the merged layer
// still bounds the handler's context when a timeout is configured.
func TestRequestContextMiddlewareImposesConfiguredTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)

	eng := gin.New()
	eng.Use(requestContextMiddleware(time.Hour))
	var hasDeadline bool
	eng.GET("/", func(c *gin.Context) {
		_, hasDeadline = c.Request.Context().Deadline()
		c.Status(http.StatusOK)
	})

	eng.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if !hasDeadline {
		t.Fatal("merged request_context did not impose the configured per-handler deadline")
	}
}

// TestRequestContextMiddlewareDisabledTimeoutImposesNoDeadline is the disabled
// counterpart: with timeout 0 the handler's context carries no deadline, but
// the correlation IDs (the middleware's other job) still land.
func TestRequestContextMiddlewareDisabledTimeoutImposesNoDeadline(t *testing.T) {
	gin.SetMode(gin.TestMode)

	eng := gin.New()
	eng.Use(requestContextMiddleware(0))
	var hasDeadline bool
	var requestID string
	eng.GET("/", func(c *gin.Context) {
		_, hasDeadline = c.Request.Context().Deadline()
		requestID = GetRequestID(c)
		c.Status(http.StatusOK)
	})

	eng.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if hasDeadline {
		t.Fatal("request_context imposed a deadline with the timeout disabled")
	}
	if requestID == "" {
		t.Fatal("request_context did not assign a request ID with the timeout disabled")
	}
}

// BenchmarkRequestContextMiddleware quantifies the enabled path and guards
// against a regression. The recorder allocation is constant across runs; the
// interesting delta is the correlation IDs plus the context.WithTimeout timer
// and single Request copy the merged layer performs.
func BenchmarkRequestContextMiddleware(b *testing.B) {
	gin.SetMode(gin.TestMode)
	eng := gin.New()
	eng.Use(requestContextMiddleware(30 * time.Second))
	eng.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		eng.ServeHTTP(httptest.NewRecorder(), req)
	}
}
