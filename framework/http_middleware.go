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
	"github.com/gombit-dev/gombit/contract"
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
// response header map (keys are already in canonical MIME form). That saves an
// allocation per header on the hot path.
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
// applyBrowserSecurityHeaders does), never an in-place slice write.
// TestSecurityHeaderSharedValueContract locks all three paths.
var (
	// apiContentSecurityPolicy is the JSON/API default (issue #267 / PERF-9):
	// the OWASP REST minimum. default-src 'none' forbids the response from
	// loading or executing anything (a JSON body needs no resources), and
	// frame-ancestors 'none' is the modern anti-clickjacking directive that
	// subsumes X-Frame-Options: DENY — so an API response needs neither
	// X-Frame-Options nor Referrer-Policy, keeping it well under the 8-header
	// swiss-map threshold that used to cost the layer ~5 allocs/op.
	apiContentSecurityPolicyValue = []string{"default-src 'none'; frame-ancestors 'none'"}
	// browserContentSecurityPolicy is the policy for HTML documents the
	// framework itself serves but whose handler cannot set its own headers —
	// today only Huma's interactive docs at /docs. It matches the pre-#267
	// full browser policy so nothing about the docs page regresses.
	browserContentSecurityPolicyValue = []string{"default-src 'self'"}
	referrerPolicyValue               = []string{"strict-origin-when-cross-origin"}
	hstsHeaderValue                   = []string{"max-age=315360000; includeSubDomains"}
	contentTypeOptionsValue           = []string{"nosniff"}
	frameOptionsValue                 = []string{"DENY"}
)

// securityHeadersMiddleware sets the framework's baseline security headers on
// every response, scoped by response kind (issue #267 / PERF-9). See
// docs/security.md for the full per-response-kind header table.
//
// X-Content-Type-Options and (in production) Strict-Transport-Security apply
// to every response. The Content-Security-Policy differs by kind:
//
//   - API/JSON (the default, and the hot path): default-src 'none';
//     frame-ancestors 'none'. Nothing else — this holds the common JSON
//     response with correlation IDs and Content-Type at <= 6 headers, one
//     under the swiss-map 8-slot group boundary, so the layer allocates
//     nothing (the map and net/http's WriteHeader Header.Clone no longer grow).
//   - Interactive docs (/docs): the full browser policy, applied here by path
//     because Huma owns that handler. Huma's docs renderer sets its own
//     Content-Security-Policy (it must allow the Swagger UI assets), so it
//     overrides the CSP set here; what this branch contributes that Huma does
//     not is Referrer-Policy and the legacy X-Frame-Options: DENY.
//
// HTML documents the framework serves through its own handlers — the admin SPA
// and the embedded frontend — start from the API baseline set here and then
// override to the browser policy via applyBrowserSecurityHeaders (see embed.go
// and adminui.go), which is where the richer SPA CSP, Referrer-Policy, and
// X-Frame-Options are added. X-Download-Options ("noopen") is IE8-only and set
// nowhere: no supported browser honors it (issue #267).
func securityHeadersMiddleware(includeHSTS bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.Writer.Header()
		header["X-Content-Type-Options"] = contentTypeOptionsValue
		if includeHSTS {
			header["Strict-Transport-Security"] = hstsHeaderValue
		}
		if isDocsPath(c.Request.URL.Path) {
			header["Content-Security-Policy"] = browserContentSecurityPolicyValue
			header["Referrer-Policy"] = referrerPolicyValue
			header["X-Frame-Options"] = frameOptionsValue
		} else {
			header["Content-Security-Policy"] = apiContentSecurityPolicyValue
		}
		c.Next()
	}
}

// isDocsPath reports whether urlPath is Huma's interactive docs UI. Docs is an
// HTML document served by Huma (contract.DocsPath, root-mounted), so its
// response kind is decided here by path rather than by an override in a
// framework handler.
func isDocsPath(urlPath string) bool {
	return urlPath == contract.DocsPath || strings.HasPrefix(urlPath, contract.DocsPath+"/")
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
