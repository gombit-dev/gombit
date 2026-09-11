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
	"sync/atomic"
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
		ctx := c.Request.Context()
		// If the request already carries a deadline at or before the one we
		// would impose, wrapping it again only allocates a second timer and a
		// shallow Request copy that can never take effect first — context
		// always honors the earliest deadline, which is the existing one. Skip
		// the wrap in that case (issue #242). When no deadline (or a later one)
		// is present — the common case — the context.WithTimeout timer is
		// intrinsic to propagating cancellation to handlers and DB calls, so it
		// stays.
		if deadline, ok := ctx.Deadline(); ok && !deadline.After(time.Now().Add(timeout)) {
			c.Next()
			return
		}

		ctx, cancel := context.WithTimeout(ctx, timeout)
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

// httpMetrics accumulates request metrics without locking the request path.
// The per-request writers (addActive, observe) use atomics only, so the
// metrics middleware no longer serializes concurrent requests on a single
// process-global mutex (issue #239). The label set — method × route × status —
// is bounded (normalizeMetricsMethod caps method cardinality, FullPath caps
// route), so the sync.Map holding the per-series counters reaches a steady
// state after warmup and the hot path settles into a lock-free Load.
type httpMetrics struct {
	active   atomic.Int64
	counters sync.Map // metricsKey -> *metricCounter
}

// metricCounter holds the two accumulators for a single metrics series. Both
// are updated with atomic adds and take no lock. latency is stored as int64
// nanoseconds (time.Duration's underlying unit) so render can convert it back
// with time.Duration(ns).Seconds() exactly as before.
type metricCounter struct {
	requests atomic.Int64
	latency  atomic.Int64
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
	// The zero value is ready: atomic.Int64 starts at 0 and sync.Map needs no
	// initialization. Counters are created lazily by observe on first sight of
	// a series.
	return &httpMetrics{}
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
	m.active.Add(delta)
}

func (m *httpMetrics) observe(key metricsKey, duration time.Duration) {
	counter := m.counterFor(key)
	counter.requests.Add(1)
	counter.latency.Add(int64(duration))
}

// counterFor returns the per-series counter for key, creating it on first use.
// The common case (series already seen) is a lock-free sync.Map.Load; only the
// first request for a never-before-seen series takes the LoadOrStore slow path,
// and the bounded label set means that stops happening after warmup.
func (m *httpMetrics) counterFor(key metricsKey) *metricCounter {
	if existing, ok := m.counters.Load(key); ok {
		return existing.(*metricCounter)
	}
	actual, _ := m.counters.LoadOrStore(key, &metricCounter{})
	return actual.(*metricCounter)
}

// series returns the number of distinct metrics series recorded so far. It
// backs the cardinality-bounding tests, which previously read len() on the
// underlying map directly.
func (m *httpMetrics) series() int {
	count := 0
	m.counters.Range(func(_, _ any) bool {
		count++
		return true
	})
	return count
}

func (m *httpMetrics) handler(c *gin.Context) {
	c.Data(http.StatusOK, "text/plain; version=0.0.4; charset=utf-8", []byte(m.render()))
}

func (m *httpMetrics) render() string {
	active := m.active.Load()
	requests := make(map[metricsKey]int64)
	latency := make(map[metricsKey]time.Duration)
	m.counters.Range(func(k, v any) bool {
		key := k.(metricsKey)
		counter := v.(*metricCounter)
		requests[key] = counter.requests.Load()
		latency[key] = time.Duration(counter.latency.Load())
		return true
	})

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
