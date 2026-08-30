package framework

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// TestRequestTimeoutMiddlewareSkipsWrapWhenTighterDeadlineExists locks the
// issue #242 guard: when the incoming request already carries a deadline at or
// before the middleware's timeout, the middleware must not allocate a second
// timeout context — the handler must observe the exact same context instance it
// came in with. Context identity is the discriminator: a re-wrap would hand the
// handler a different (child) context even though the effective deadline is
// unchanged.
func TestRequestTimeoutMiddlewareSkipsWrapWhenTighterDeadlineExists(t *testing.T) {
	gin.SetMode(gin.TestMode)

	parent, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	eng := gin.New()
	eng.Use(requestTimeoutMiddleware(time.Hour))
	var sameContext bool
	eng.GET("/", func(c *gin.Context) {
		sameContext = c.Request.Context() == parent
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(parent)
	eng.ServeHTTP(httptest.NewRecorder(), req)

	if !sameContext {
		t.Fatal("middleware re-wrapped a request that already had a tighter deadline; guard did not fire")
	}
}

// TestRequestTimeoutMiddlewareImposesDeadlineWhenNonePresent is the other half:
// a request with no deadline (the common case) must still leave the handler
// with a bounded context, and it must be a fresh context, not the original.
func TestRequestTimeoutMiddlewareImposesDeadlineWhenNonePresent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	eng := gin.New()
	eng.Use(requestTimeoutMiddleware(time.Hour))
	var hasDeadline, replaced bool
	eng.GET("/", func(c *gin.Context) {
		_, hasDeadline = c.Request.Context().Deadline()
		replaced = c.Request.Context() != context.Background()
		c.Status(http.StatusOK)
	})

	// httptest.NewRequest attaches context.Background() by default (no deadline).
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	eng.ServeHTTP(httptest.NewRecorder(), req)

	if !hasDeadline {
		t.Fatal("middleware did not impose a deadline on a request that had none")
	}
	if !replaced {
		t.Fatal("middleware left the original context in place")
	}
}

// TestRequestTimeoutMiddlewareDisabledIsNoop confirms a non-positive timeout
// leaves the request context untouched.
func TestRequestTimeoutMiddlewareDisabledIsNoop(t *testing.T) {
	gin.SetMode(gin.TestMode)

	eng := gin.New()
	eng.Use(requestTimeoutMiddleware(0))
	var hasDeadline bool
	eng.GET("/", func(c *gin.Context) {
		_, hasDeadline = c.Request.Context().Deadline()
		c.Status(http.StatusOK)
	})

	eng.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if hasDeadline {
		t.Fatal("disabled timeout middleware imposed a deadline")
	}
}

// BenchmarkRequestTimeoutMiddleware quantifies the per-request cost the guard
// avoids and guards against a regression in the enabled path. The recorder
// allocation is constant across runs; the interesting delta is the
// context.WithTimeout timer + Request copy.
func BenchmarkRequestTimeoutMiddleware(b *testing.B) {
	gin.SetMode(gin.TestMode)
	eng := gin.New()
	eng.Use(requestTimeoutMiddleware(30 * time.Second))
	eng.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		eng.ServeHTTP(httptest.NewRecorder(), req)
	}
}
