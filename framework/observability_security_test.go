package framework

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gombit-dev/gombit/config"
)

func TestDefaultRuntimeMiddlewareOrder(t *testing.T) {
	stack := runtimeMiddlewareStack(config.Default(), newHTTPMetrics(), nil, nil)
	got := make([]string, 0, len(stack))
	for _, middleware := range stack {
		got = append(got, middleware.name)
	}

	want := []string{
		"recovery",
		"request_context",
		"metrics",
		"security_headers",
		"xss",
		"request_timeout",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("runtime middleware order = %v, want %v", got, want)
	}
}

func TestDefaultRouterAddsRequestID(t *testing.T) {
	app := newTestApp(t)
	app.Router().GET("/request-id", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"gin":     GetRequestID(c),
			"context": GetRequestIDFromContext(c.Request.Context()),
		})
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/request-id", nil)
	app.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /request-id status = %d, want %d", rec.Code, http.StatusOK)
	}
	requestID := rec.Header().Get(RequestIDHeader)
	if requestID == "" {
		t.Fatal("request ID response header is empty")
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response body: %v", err)
	}
	if body["gin"] != requestID || body["context"] != requestID {
		t.Fatalf("request IDs = %#v, want both values to match response header %q", body, requestID)
	}
}

func TestDefaultRouterPreservesRequestID(t *testing.T) {
	app := newTestApp(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/livez", nil)
	req.Header.Set(RequestIDHeader, "req-123")
	app.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /livez status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get(RequestIDHeader); got != "req-123" {
		t.Fatalf("%s = %q, want req-123", RequestIDHeader, got)
	}
}

func TestDefaultRouterCarriesTraceContext(t *testing.T) {
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	app := newTestApp(t)
	app.Router().GET("/trace", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"gin":     GetTraceID(c),
			"context": GetTraceIDFromContext(c.Request.Context()),
		})
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/trace", nil)
	req.Header.Set(TraceparentHeader, "00-"+traceID+"-00f067aa0ba902b7-01")
	app.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /trace status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get(TraceIDHeader); got != traceID {
		t.Fatalf("%s = %q, want %q", TraceIDHeader, got, traceID)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response body: %v", err)
	}
	if body["gin"] != traceID || body["context"] != traceID {
		t.Fatalf("trace IDs = %#v, want both values to match traceparent trace ID %q", body, traceID)
	}
}

func TestDefaultRouterAddsSecurityHeaders(t *testing.T) {
	app := newTestApp(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/livez", nil)
	app.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /livez status = %d, want %d", rec.Code, http.StatusOK)
	}

	want := map[string]string{
		"Content-Security-Policy": "default-src 'self'",
		"Referrer-Policy":         "strict-origin-when-cross-origin",
		"X-Content-Type-Options":  "nosniff",
		"X-Download-Options":      "noopen",
		"X-Frame-Options":         "DENY",
	}
	for header, value := range want {
		if got := rec.Header().Get(header); got != value {
			t.Fatalf("%s = %q, want %q", header, got, value)
		}
	}
	if got := rec.Header().Get("Strict-Transport-Security"); got != "" {
		t.Fatalf("Strict-Transport-Security = %q, want empty outside production", got)
	}
	if got := rec.Header().Get("X-XSS-Protection"); got != "" {
		t.Fatalf("X-XSS-Protection = %q, want empty deprecated header", got)
	}
}

// TestSecurityHeaderSharedValueContract locks the invariant behind the
// allocation optimization in securityHeadersMiddleware: the header values are
// shared, read-only package-level slices assigned straight into each
// response's header map. Sharing is safe only under http.Header's mutation
// APIs (Set/Del replace the entry; Add reallocates a len==cap==1 slice); an
// in-place slice write leaks into the process-global value. All three paths
// are exercised so the contract is proven, not merely self-consistent.
func TestSecurityHeaderSharedValueContract(t *testing.T) {
	const key = "X-Frame-Options"

	// Set is the supported override API — applySPAContentSecurityPolicy uses
	// it. It replaces the map entry, so the shared value stays request-local.
	t.Run("set override stays request-local", func(t *testing.T) {
		app := newTestApp(t)
		app.Router().GET("/set-header", func(c *gin.Context) {
			c.Header(key, "SAMEORIGIN")
			c.Status(http.StatusOK)
		})

		overridden := httptest.NewRecorder()
		app.Router().ServeHTTP(overridden, httptest.NewRequest(http.MethodGet, "/set-header", nil))
		if got := overridden.Header().Get(key); got != "SAMEORIGIN" {
			t.Fatalf("overridden %s = %q, want SAMEORIGIN", key, got)
		}

		next := httptest.NewRecorder()
		app.Router().ServeHTTP(next, httptest.NewRequest(http.MethodGet, "/livez", nil))
		if got := next.Header().Get(key); got != "DENY" {
			t.Fatalf("later %s = %q, want DENY — Set must not touch the shared value", key, got)
		}
		if frameOptionsValue[0] != "DENY" {
			t.Fatalf("frameOptionsValue = %v, want [DENY]", frameOptionsValue)
		}
	})

	// Add appends; on a len==cap==1 slice it must reallocate, so it too stays
	// request-local. That reallocation is the load-bearing reason Add is safe.
	t.Run("add reallocates and stays request-local", func(t *testing.T) {
		app := newTestApp(t)
		app.Router().GET("/add-header", func(c *gin.Context) {
			c.Writer.Header().Add(key, "SAMEORIGIN")
			c.Status(http.StatusOK)
		})

		added := httptest.NewRecorder()
		app.Router().ServeHTTP(added, httptest.NewRequest(http.MethodGet, "/add-header", nil))
		if got := added.Header().Values(key); len(got) != 2 || got[0] != "DENY" || got[1] != "SAMEORIGIN" {
			t.Fatalf("added %s = %v, want [DENY SAMEORIGIN]", key, got)
		}

		next := httptest.NewRecorder()
		app.Router().ServeHTTP(next, httptest.NewRequest(http.MethodGet, "/livez", nil))
		if got := next.Header().Values(key); len(got) != 1 || got[0] != "DENY" {
			t.Fatalf("later %s = %v, want [DENY]", key, got)
		}
		if frameOptionsValue[0] != "DENY" {
			t.Fatalf("frameOptionsValue = %v, want [DENY]", frameOptionsValue)
		}
	})

	// The hazard the sharing depends on callers avoiding: an in-place write
	// through the live slice Header.Values returns corrupts the process-global
	// value for every subsequent request. Demonstrated here (and restored via
	// Cleanup) so the read-only constraint in securityHeadersMiddleware is
	// executable, not just prose. Real handlers MUST NOT do this — use Set.
	t.Run("in-place mutation leaks into the shared value (unsupported)", func(t *testing.T) {
		t.Cleanup(func() { frameOptionsValue = []string{"DENY"} })

		app := newTestApp(t)
		app.Router().GET("/mutate-in-place", func(c *gin.Context) {
			c.Writer.Header().Values(key)[0] = "ALLOWALL"
			c.Status(http.StatusOK)
		})
		app.Router().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/mutate-in-place", nil))

		if frameOptionsValue[0] != "ALLOWALL" {
			t.Fatalf("frameOptionsValue = %v; an in-place Values() write was expected to leak into the "+
				"shared value. If it no longer does, in-place mutation became safe and the middleware "+
				"comment must be updated to match", frameOptionsValue)
		}
	})
}

func TestProductionRouterAddsHSTS(t *testing.T) {
	previousMode := gin.Mode()
	t.Cleanup(func() {
		gin.SetMode(previousMode)
	})

	cfg := config.DefaultFor(config.EnvironmentProduction)
	cfg.HTTP.Addr = "127.0.0.1:0"
	app := newTestApp(t, WithConfig(cfg))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/livez", nil)
	app.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /livez status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Strict-Transport-Security"); !strings.Contains(got, "max-age=315360000") {
		t.Fatalf("Strict-Transport-Security = %q, want max-age=315360000 in production", got)
	}
}

func TestDefaultRouterRequestTimeoutSetsDeadline(t *testing.T) {
	cfg := config.Default()
	cfg.Environment = config.EnvironmentTest
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.HTTP.RequestTimeout = 40 * time.Millisecond

	app := newTestApp(t, WithConfig(cfg))
	app.Router().GET("/slow", func(c *gin.Context) {
		ctx := c.Request.Context()
		select {
		case <-time.After(500 * time.Millisecond):
			c.Status(http.StatusOK)
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				c.AbortWithStatus(http.StatusGatewayTimeout)
				return
			}
			c.AbortWithStatus(http.StatusInternalServerError)
		}
	})

	rec := httptest.NewRecorder()
	start := time.Now()
	app.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/slow", nil))

	if elapsed := time.Since(start); elapsed >= 200*time.Millisecond {
		t.Fatalf("GET /slow took %v, want handler to stop after request context deadline", elapsed)
	}
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("GET /slow status = %d, want %d", rec.Code, http.StatusGatewayTimeout)
	}
}

func TestRunContextConfiguresServerTimeouts(t *testing.T) {
	cfg := config.Default()
	cfg.Environment = config.EnvironmentTest
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.HTTP.RequestTimeout = 2 * time.Second
	app := newTestApp(t, WithConfig(cfg))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunContext(ctx, app)
	}()
	t.Cleanup(func() {
		cancel()
		if err := waitRun(done); err != nil {
			t.Fatalf("RunContext() error = %v, want nil", err)
		}
	})

	waitForHTTP(t, app, "/livez")

	app.mu.RLock()
	server := app.server
	app.mu.RUnlock()
	if server == nil {
		t.Fatal("server = nil, want configured HTTP server")
	}
	if server.ReadTimeout != cfg.HTTP.RequestTimeout {
		t.Fatalf("server.ReadTimeout = %v, want %v", server.ReadTimeout, cfg.HTTP.RequestTimeout)
	}
	if server.WriteTimeout != cfg.HTTP.RequestTimeout {
		t.Fatalf("server.WriteTimeout = %v, want %v", server.WriteTimeout, cfg.HTTP.RequestTimeout)
	}
	if server.IdleTimeout != cfg.HTTP.RequestTimeout {
		t.Fatalf("server.IdleTimeout = %v, want %v", server.IdleTimeout, cfg.HTTP.RequestTimeout)
	}
}

func TestDefaultRouterMetricsEndpointRecordsRequests(t *testing.T) {
	app := newTestApp(t)

	livez := httptest.NewRecorder()
	app.Router().ServeHTTP(livez, httptest.NewRequest(http.MethodGet, "/livez", nil))
	if livez.Code != http.StatusOK {
		t.Fatalf("GET /livez status = %d, want %d", livez.Code, http.StatusOK)
	}

	metrics := httptest.NewRecorder()
	app.Router().ServeHTTP(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if metrics.Code != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want %d", metrics.Code, http.StatusOK)
	}

	body := metrics.Body.String()
	for _, want := range []string{
		"gombit_http_active_requests",
		`gombit_http_requests_total{method="GET",route="/livez",status="200"} 1`,
		`gombit_http_request_duration_seconds_sum{method="GET",route="/livez",status="200"}`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("GET /metrics body = %q, want it to contain %q", body, want)
		}
	}
}

func TestDefaultRouterMetricsUsesBoundedLabelForUnmatchedRoutes(t *testing.T) {
	app := newTestApp(t)

	for _, path := range []string{"/missing/one", "/missing/two"} {
		rec := httptest.NewRecorder()
		app.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s status = %d, want %d", path, rec.Code, http.StatusNotFound)
		}
	}

	metrics := httptest.NewRecorder()
	app.Router().ServeHTTP(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if metrics.Code != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want %d", metrics.Code, http.StatusOK)
	}

	body := metrics.Body.String()
	if !strings.Contains(body, `gombit_http_requests_total{method="GET",route="unmatched",status="404"} 2`) {
		t.Fatalf("GET /metrics body = %q, want bounded unmatched route label", body)
	}
	for _, rawPath := range []string{"/missing/one", "/missing/two"} {
		if strings.Contains(body, rawPath) {
			t.Fatalf("GET /metrics body = %q, want no raw unmatched path %q", body, rawPath)
		}
	}
}

// TestMetricsMiddlewareBoundsMethodLabelCardinality is the regression test for
// issue #197: the metrics `method` label is derived from the client-supplied
// HTTP method, which is an unbounded RFC 7230 token. Recording it verbatim
// into the never-evicted metrics maps let an unauthenticated remote caller
// mint unlimited distinct series (a slow OOM). Arbitrary methods must collapse
// to a single "other" bucket, keeping cardinality bounded.
func TestMetricsMiddlewareBoundsMethodLabelCardinality(t *testing.T) {
	metrics := newHTTPMetrics()
	engine := gin.New()
	engine.Use(metricsMiddleware(metrics))
	engine.GET("/livez", func(c *gin.Context) { c.Status(http.StatusOK) })

	// Global middleware runs on 404s too, so unregistered methods still reach
	// observe — exactly the attack path in the issue.
	for i := 0; i < 1000; i++ {
		req := httptest.NewRequest(fmt.Sprintf("EVILMETHOD%d", i), "/livez", nil)
		engine.ServeHTTP(httptest.NewRecorder(), req)
	}

	if got := metrics.series(); got != 1 {
		t.Fatalf("distinct metric series after 1000 arbitrary methods = %d, want 1 (bounded)", got)
	}

	body := metrics.render()
	if !strings.Contains(body, `method="other"`) {
		t.Fatalf("metrics body = %q, want non-standard methods bucketed as method=\"other\"", body)
	}
	if strings.Contains(body, "EVILMETHOD") {
		t.Fatalf("metrics body = %q, want no raw client method token recorded", body)
	}
}

// TestMetricsMiddlewareKeepsStandardMethodLabels confirms the bounding does not
// flatten legitimate, standard methods into the "other" bucket.
func TestMetricsMiddlewareKeepsStandardMethodLabels(t *testing.T) {
	metrics := newHTTPMetrics()
	engine := gin.New()
	engine.Use(metricsMiddleware(metrics))
	engine.Any("/thing", func(c *gin.Context) { c.Status(http.StatusOK) })

	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, "/thing", nil))
	}

	body := metrics.render()
	for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
		if !strings.Contains(body, fmt.Sprintf(`method=%q`, method)) {
			t.Fatalf("metrics body = %q, want standard method label %q preserved", body, method)
		}
	}
	if strings.Contains(body, `method="other"`) {
		t.Fatalf("metrics body = %q, want no \"other\" bucket for standard methods", body)
	}
}

// TestMetricsMiddlewareConcurrentCountsAreExact drives the metrics middleware
// from many goroutines at once and asserts every request is counted exactly
// once. It guards the lock-free hot path (issue #239): atomic accumulation must
// lose no increments under contention, and the active gauge must return to zero
// once all requests drain. Run with -race to also catch any unsynchronized
// access to the counters.
func TestMetricsMiddlewareConcurrentCountsAreExact(t *testing.T) {
	metrics := newHTTPMetrics()
	engine := gin.New()
	engine.Use(metricsMiddleware(metrics))
	engine.GET("/thing", func(c *gin.Context) { c.Status(http.StatusOK) })

	const goroutines = 32
	const perGoroutine = 500
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range perGoroutine {
				engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/thing", nil))
			}
		}()
	}
	wg.Wait()

	wantTotal := int64(goroutines * perGoroutine)
	counter := metrics.counterFor(metricsKey{method: http.MethodGet, route: "/thing", status: http.StatusOK})
	if got := counter.requests.Load(); got != wantTotal {
		t.Fatalf("recorded request count = %d, want %d (lost increments under contention)", got, wantTotal)
	}
	if got := metrics.active.Load(); got != 0 {
		t.Fatalf("active gauge after drain = %d, want 0", got)
	}
	if got := metrics.series(); got != 1 {
		t.Fatalf("distinct series = %d, want 1", got)
	}
}

func TestReadyzAndLivezUseEnvelope(t *testing.T) {
	app := newTestApp(t)

	for _, path := range []string{"/livez", "/readyz"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			app.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s status = %d, want %d", path, rec.Code, http.StatusOK)
			}
			var body struct {
				Data struct {
					Status string `json:"status"`
				} `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("unmarshal %s response body: %v", path, err)
			}
			if body.Data.Status != "ok" {
				t.Fatalf("GET %s data.status = %q, want ok", path, body.Data.Status)
			}
		})
	}
}

func TestTrustedProxiesNilIgnoresForwardedFor(t *testing.T) {
	app := newTestApp(t)
	app.Router().GET("/client-ip", func(c *gin.Context) {
		c.String(http.StatusOK, c.ClientIP())
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/client-ip", nil)
	req.RemoteAddr = "192.0.2.10:5555"
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	app.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /client-ip status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "192.0.2.10" {
		t.Fatalf("client IP = %q, want direct peer IP", got)
	}
}

func TestTrustedProxiesInvalidConfigReturnsError(t *testing.T) {
	cfg := config.Default()
	cfg.Environment = config.EnvironmentTest
	cfg.HTTP.TrustedProxies = []string{"not-a-valid-cidr!!!"}

	_, err := New(WithConfig(cfg))
	if err == nil {
		t.Fatal("New() error = nil, want trusted proxy validation error")
	}
	if !strings.Contains(err.Error(), "trusted proxies") {
		t.Fatalf("New() error = %q, want trusted proxies message", err)
	}
}

func TestTrustedProxiesAllowsForwardedFromTrustedPeer(t *testing.T) {
	cfg := config.Default()
	cfg.Environment = config.EnvironmentTest
	cfg.HTTP.TrustedProxies = []string{"192.0.2.10/32"}
	app := newTestApp(t, WithConfig(cfg))
	app.Router().GET("/client-ip", func(c *gin.Context) {
		c.String(http.StatusOK, c.ClientIP())
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/client-ip", nil)
	req.RemoteAddr = "192.0.2.10:5555"
	req.Header.Set("X-Forwarded-For", "198.51.100.22")
	app.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /client-ip status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "198.51.100.22" {
		t.Fatalf("client IP = %q, want forwarded IP", got)
	}
}

func TestRunContextServesMetricsWithRuntimeMiddleware(t *testing.T) {
	app := newTestApp(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunContext(ctx, app)
	}()
	t.Cleanup(func() {
		cancel()
		if err := waitRun(done); err != nil {
			t.Fatalf("RunContext() error = %v, want nil", err)
		}
	})

	waitForHTTP(t, app, "/livez")
	resp := getHTTP(t, app, "/metrics")
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read /metrics body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if !strings.Contains(string(body), "gombit_http_requests_total") {
		t.Fatalf("GET /metrics body = %q, want request metrics", string(body))
	}
}

func TestDefaultRouterSanitizesXSSInJSONBody(t *testing.T) {
	app := newTestApp(t)
	app.Router().POST("/comment", func(c *gin.Context) {
		var body struct {
			Comment string `json:"comment"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			t.Fatalf("bind JSON: %v", err)
		}
		c.JSON(http.StatusOK, gin.H{"comment": body.Comment})
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(
		http.MethodPost,
		"/comment",
		strings.NewReader(`{"comment":"<script>alert(1)</script>hi"}`),
	)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	app.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /comment status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v; body: %s", err, rec.Body.String())
	}
	if body["comment"] != "hi" {
		t.Fatalf("comment = %q, want %q", body["comment"], "hi")
	}
}

func TestDefaultRouterLeavesPasswordFieldUnsanitized(t *testing.T) {
	app := newTestApp(t)
	app.Router().POST("/login", func(c *gin.Context) {
		var body struct {
			Password string `json:"password"`
			Note     string `json:"note"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			t.Fatalf("bind JSON: %v", err)
		}
		c.JSON(http.StatusOK, gin.H{
			"password": body.Password,
			"note":     body.Note,
		})
	})

	const rawMarkup = `<b>secret</b>`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(
		http.MethodPost,
		"/login",
		strings.NewReader(`{"password":"`+rawMarkup+`","note":"<i>hi</i>"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	app.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /login status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v; body: %s", err, rec.Body.String())
	}
	if body["password"] != rawMarkup {
		t.Fatalf("password = %q, want unsanitized %q", body["password"], rawMarkup)
	}
	if body["note"] != "hi" {
		t.Fatalf("note = %q, want %q", body["note"], "hi")
	}
}

func TestDefaultRouterSanitizesXSSInQuery(t *testing.T) {
	app := newTestApp(t)
	app.Router().GET("/search", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"q": c.Query("q")})
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/search?q=<script>x</script>hi", nil)
	app.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /search status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v; body: %s", err, rec.Body.String())
	}
	if body["q"] != "hi" {
		t.Fatalf("q = %q, want %q", body["q"], "hi")
	}
}
