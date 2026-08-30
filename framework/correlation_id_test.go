package framework

import (
	"regexp"
	"testing"
)

// uuidV4Pattern matches the canonical UUIDv4 textual form: version nibble 4 and
// variant nibble in [89ab]. newRequestID must keep emitting this shape after
// switching its randomness source from crypto/rand to math/rand/v2 (issue #240).
var uuidV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// traceIDPattern is a 32-char lowercase hex string, the W3C trace-id form.
var traceIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func TestNewRequestIDFormatAndUniqueness(t *testing.T) {
	const n = 10000
	seen := make(map[string]struct{}, n)
	for range n {
		id := newRequestID()
		if !uuidV4Pattern.MatchString(id) {
			t.Fatalf("newRequestID() = %q, want UUIDv4 form", id)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("newRequestID() produced a duplicate %q within %d draws", id, n)
		}
		seen[id] = struct{}{}
	}
}

func TestNewTraceIDFormatAndUniqueness(t *testing.T) {
	const n = 10000
	seen := make(map[string]struct{}, n)
	for range n {
		id := newTraceID()
		if !traceIDPattern.MatchString(id) {
			t.Fatalf("newTraceID() = %q, want 32-char lowercase hex", id)
		}
		if id == "00000000000000000000000000000000" {
			t.Fatalf("newTraceID() returned the all-zero trace ID, which is invalid")
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("newTraceID() produced a duplicate %q within %d draws", id, n)
		}
		seen[id] = struct{}{}
	}
}

// TestGeneratedTraceIDSurvivesTraceparentRoundTrip pins that a generated trace
// ID is accepted by the inbound traceparent parser — i.e. the two ends of the
// trace-context path agree on the 32-hex-lowercase shape.
func TestGeneratedTraceIDSurvivesTraceparentRoundTrip(t *testing.T) {
	id := newTraceID()
	got := traceIDFromTraceparent("00-" + id + "-00f067aa0ba902b7-01")
	if got != id {
		t.Fatalf("traceIDFromTraceparent round-trip = %q, want %q", got, id)
	}
}
