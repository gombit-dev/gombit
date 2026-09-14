package framework

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// RequestIDHeader is the HTTP header carrying a stable per-request ID.
const RequestIDHeader = "X-Request-Id"

// requestIDLen is the canonical UUIDv4 textual length (8-4-4-4-12).
const requestIDLen = 36

// requestMeta carries the per-request correlation IDs propagated through the
// request context by requestContextMiddleware, plus the [2]string backing array
// the response correlation headers alias.
//
// One heap allocation (this struct) now holds everything the layer needs: both
// IDs, the two response-header slices (sub-slices of hdr), and — because a
// *requestMeta boxes into an interface word for free — the context value. That
// collapses what used to be two boxed Gin Keys entries, two http.Header.Set
// slices, and a separately boxed value into a single allocation (issue #268).
//
// hdr's slices are handed to the response header map read-only, exactly like
// the shared security-header values: header[RequestIDHeader] = hdr[0:1:1] and
// header[TraceIDHeader] = hdr[1:2:2] each have len == cap == 1. Cap 1 is
// load-bearing — http.Header.Add must reallocate rather than write the appended
// value into the adjacent array slot, which would otherwise corrupt the *other*
// correlation header (both alias the same array). Set/Del replace the map
// entry. Callers MUST NOT write through these slices in place.
// TestRequestCorrelationHeaderSharedArrayContract locks this, mirroring
// TestSecurityHeaderSharedValueContract.
type requestMeta struct {
	requestID string
	traceID   string
	hdr       [2]string
}

type requestMetaKey struct{}

// requestContextMiddleware assigns the request and trace correlation IDs
// (honoring an inbound X-Request-Id and W3C traceparent when present), exposes
// them on the response headers and through the request context under a single
// key, and — when timeout > 0 — imposes the per-handler deadline in the same
// pass.
//
// Folding the former request_timeout layer in here means one
// Request.WithContext for the context value and the deadline together, not two
// (issue #268). The middleware no longer writes to Gin's Keys map either:
// GetRequestID/GetTraceID read the same context value the *FromContext
// accessors do, so the two c.Set calls (a lazily-allocated map plus two boxed
// strings) are gone.
func requestContextMiddleware(timeout time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := strings.TrimSpace(c.GetHeader(RequestIDHeader))
		traceID := traceIDFromTraceparent(c.GetHeader(TraceparentHeader))

		meta := &requestMeta{}
		if requestID == "" && traceID == "" {
			// Common path: both IDs are generated. Encode them into one stack
			// buffer and take a single string(); two sub-slices of that string
			// become the IDs, so the pair costs one allocation instead of two.
			var buf [requestIDLen + traceIDLen]byte
			encodeRequestID(buf[:requestIDLen])
			encodeTraceID(buf[requestIDLen:])
			s := string(buf[:])
			requestID = s[:requestIDLen]
			traceID = s[requestIDLen:]
		} else {
			// Rare path: an inbound X-Request-Id or traceparent is honored and the
			// missing side (if any) generated on its own. Two strings here is fine
			// — it is not the hot path.
			if requestID == "" {
				requestID = newRequestID()
			}
			if traceID == "" {
				traceID = newTraceID()
			}
		}
		if requestID == "" {
			requestID = "unknown"
		}
		if traceID == "" {
			traceID = "unknown"
		}

		meta.requestID = requestID
		meta.traceID = traceID
		meta.hdr[0] = requestID
		meta.hdr[1] = traceID

		header := c.Writer.Header()
		header[RequestIDHeader] = meta.hdr[0:1:1]
		header[TraceIDHeader] = meta.hdr[1:2:2]

		ctx := context.WithValue(c.Request.Context(), requestMetaKey{}, meta)
		ctx, cancel := applyTimeout(ctx, timeout)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)

		c.Next()
	}
}

// GetRequestID returns the request ID for the current request. It reads the
// value requestContextMiddleware propagated through the request context, so it
// agrees with GetRequestIDFromContext by construction.
func GetRequestID(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	return GetRequestIDFromContext(c.Request.Context())
}

// GetRequestIDFromContext reads the request ID from a request context.
func GetRequestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	meta, _ := ctx.Value(requestMetaKey{}).(*requestMeta)
	if meta == nil {
		return ""
	}
	return meta.requestID
}

// randomBytes16 fills a 16-byte array with non-cryptographic randomness from
// math/rand/v2, whose top-level generator is per-P and lock-free — unlike
// crypto/rand's globally locked reader, which also incurs a getrandom syscall.
// Request and trace IDs are opaque correlation tokens, not secrets, so they do
// not need a CSPRNG; paying crypto/rand's syscall + global lock on every
// request was a measurable hot-path CPU and cross-core contention cost
// (issue #240). Unlike crypto/rand.Read this cannot fail, so callers no longer
// have an error path to handle.
func randomBytes16() [16]byte {
	var b [16]byte
	for i := 0; i < len(b); i += 8 {
		// G404 is intentional here: see this function's doc comment. Correlation
		// IDs are not secrets, and a CSPRNG's syscall + global lock is exactly
		// the cost issue #240 removes.
		binary.LittleEndian.PutUint64(b[i:i+8], rand.Uint64()) //nolint:gosec // G404: non-secret correlation IDs (issue #240)
	}
	return b
}

// encodeRequestID writes a UUIDv4 textual form into dst, which must be
// requestIDLen bytes long. It writes in place so the caller can share one
// backing buffer with encodeTraceID (issue #268).
func encodeRequestID(dst []byte) {
	b := randomBytes16()

	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80

	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
}

func newRequestID() string {
	var buf [requestIDLen]byte
	encodeRequestID(buf[:])
	return string(buf[:])
}
