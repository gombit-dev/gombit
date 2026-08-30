package framework

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// TraceparentHeader is the W3C trace context header.
const TraceparentHeader = "Traceparent"

// TraceIDHeader exposes the active trace ID for logs, diagnostics, and tests.
const TraceIDHeader = "X-Trace-Id"

const traceIDGinKey = "trace_id"

var traceparentPattern = regexp.MustCompile(`^[0-9a-f]{2}-([0-9a-f]{32})-[0-9a-f]{16}-[0-9a-f]{2}$`)

func requestTimeoutMiddleware(timeout time.Duration) gin.HandlerFunc {
	if timeout <= 0 {
		return func(c *gin.Context) {
			c.Next()
		}
	}

	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// GetTraceID returns the trace ID stored in the Gin context.
func GetTraceID(c *gin.Context) string {
	if c == nil {
		return ""
	}
	value, ok := c.Get(traceIDGinKey)
	if !ok {
		return ""
	}
	traceID, _ := value.(string)
	return traceID
}

// GetTraceIDFromContext reads the trace ID from a request context.
func GetTraceIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	meta, _ := ctx.Value(requestMetaKey{}).(requestMeta)
	return meta.traceID
}

// Security-header values are request-invariant. Rather than allocate a fresh
// []string per header on every response (what http.Header.Set does), the
// middleware assigns these shared, read-only backing slices directly into the
// response header map (keys are already in canonical MIME form). That saves
// six allocations per response on the hot path.
//
// http.Header is a mutable map of mutable slices, so sharing is safe only
// under its documented mutation APIs, and that constraint is load-bearing:
//   - Set and Del replace or delete the map entry, never writing through the
//     shared slice.
//   - Add appends, but these slices have len == cap == 1, so it must
//     reallocate rather than grow one in place.
//
// These slices MUST be treated as read-only. An in-place write —
// Header.Values(k)[0] = ... or append(h[k][:0], ...) — writes straight through
// to the process-global value, corrupting it for every other request and
// racing with them. Override a security header with Set (as
// applySPAContentSecurityPolicy does), never an in-place slice write.
// TestSecurityHeaderSharedValueContract locks all three paths.
var (
	cspHeaderValue          = []string{"default-src 'self'"}
	referrerPolicyValue     = []string{"strict-origin-when-cross-origin"}
	hstsHeaderValue         = []string{"max-age=315360000; includeSubDomains"}
	contentTypeOptionsValue = []string{"nosniff"}
	downloadOptionsValue    = []string{"noopen"}
	frameOptionsValue       = []string{"DENY"}
)

func securityHeadersMiddleware(includeHSTS bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.Writer.Header()
		header["Content-Security-Policy"] = cspHeaderValue
		header["Referrer-Policy"] = referrerPolicyValue
		if includeHSTS {
			header["Strict-Transport-Security"] = hstsHeaderValue
		}
		header["X-Content-Type-Options"] = contentTypeOptionsValue
		header["X-Download-Options"] = downloadOptionsValue
		header["X-Frame-Options"] = frameOptionsValue
		c.Next()
	}
}

type httpMetrics struct {
	mu       sync.Mutex
	active   int64
	requests map[metricsKey]int64
	latency  map[metricsKey]time.Duration
}

type metricsKey struct {
	method string
	route  string
	status int
}

// knownHTTPMethods bounds the cardinality of the metrics `method` label. The
// raw request method is an arbitrary RFC 7230 token that an unauthenticated
// client can vary without limit, and the metrics maps are never evicted, so
// recording it verbatim lets a remote caller mint unbounded distinct series
// and exhaust memory (issue #197). The metrics middleware runs on every
// request — including unmatched routes and unregistered methods — so any
// method outside this set collapses to metricsMethodOther.
var knownHTTPMethods = map[string]struct{}{
	http.MethodGet:     {},
	http.MethodHead:    {},
	http.MethodPost:    {},
	http.MethodPut:     {},
	http.MethodPatch:   {},
	http.MethodDelete:  {},
	http.MethodOptions: {},
	http.MethodConnect: {},
	http.MethodTrace:   {},
}

const metricsMethodOther = "other"

// normalizeMetricsMethod maps any non-standard HTTP method to a single bucket
// so the metrics `method` label stays bounded by a small constant. See
// knownHTTPMethods.
func normalizeMetricsMethod(method string) string {
	if _, ok := knownHTTPMethods[method]; ok {
		return method
	}
	return metricsMethodOther
}

func newHTTPMetrics() *httpMetrics {
	return &httpMetrics{
		requests: make(map[metricsKey]int64),
		latency:  make(map[metricsKey]time.Duration),
	}
}

func metricsMiddleware(metrics *httpMetrics) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		metrics.addActive(1)
		defer metrics.addActive(-1)

		c.Next()

		route := c.FullPath()
		if route == "" {
			route = "unmatched"
		}
		metrics.observe(metricsKey{
			method: normalizeMetricsMethod(c.Request.Method),
			route:  route,
			status: c.Writer.Status(),
		}, time.Since(start))
	}
}

func (m *httpMetrics) addActive(delta int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.active += delta
}

func (m *httpMetrics) observe(key metricsKey, duration time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[key]++
	m.latency[key] += duration
}

func (m *httpMetrics) handler(c *gin.Context) {
	c.Data(http.StatusOK, "text/plain; version=0.0.4; charset=utf-8", []byte(m.render()))
}

func (m *httpMetrics) render() string {
	m.mu.Lock()
	active := m.active
	requests := make(map[metricsKey]int64, len(m.requests))
	latency := make(map[metricsKey]time.Duration, len(m.latency))
	for key, value := range m.requests {
		requests[key] = value
	}
	for key, value := range m.latency {
		latency[key] = value
	}
	m.mu.Unlock()

	keys := make([]metricsKey, 0, len(requests))
	for key := range requests {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].route != keys[j].route {
			return keys[i].route < keys[j].route
		}
		if keys[i].method != keys[j].method {
			return keys[i].method < keys[j].method
		}
		return keys[i].status < keys[j].status
	})

	var b strings.Builder
	b.WriteString("# HELP gombit_http_active_requests Active HTTP requests currently in flight.\n")
	b.WriteString("# TYPE gombit_http_active_requests gauge\n")
	fmt.Fprintf(&b, "gombit_http_active_requests %d\n", active)
	b.WriteString("# HELP gombit_http_requests_total Total HTTP requests handled by Gombit.\n")
	b.WriteString("# TYPE gombit_http_requests_total counter\n")
	for _, key := range keys {
		fmt.Fprintf(
			&b,
			"gombit_http_requests_total{method=%q,route=%q,status=%q} %d\n",
			key.method,
			key.route,
			strconv.Itoa(key.status),
			requests[key],
		)
	}
	b.WriteString("# HELP gombit_http_request_duration_seconds_sum Total request latency observed by Gombit.\n")
	b.WriteString("# TYPE gombit_http_request_duration_seconds_sum counter\n")
	for _, key := range keys {
		fmt.Fprintf(
			&b,
			"gombit_http_request_duration_seconds_sum{method=%q,route=%q,status=%q} %.9f\n",
			key.method,
			key.route,
			strconv.Itoa(key.status),
			latency[key].Seconds(),
		)
	}
	return b.String()
}

func traceIDFromTraceparent(value string) string {
	match := traceparentPattern.FindStringSubmatch(strings.ToLower(strings.TrimSpace(value)))
	if len(match) != 2 {
		return ""
	}
	if match[1] == "00000000000000000000000000000000" {
		return ""
	}
	return match[1]
}

func newTraceID() string {
	b := randomBytes16()
	if b == [16]byte{} {
		b[15] = 1
	}
	buf := make([]byte, 32)
	hex.Encode(buf, b[:])
	return string(buf)
}
