package auth

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestRemoteHost(t *testing.T) {
	for addr, want := range map[string]string{
		"192.0.2.1:5000":    "192.0.2.1",
		"[2001:db8::1]:443": "2001:db8::1",
		"[2001:db8::1]":     "2001:db8::1",
		"192.0.2.1":         "192.0.2.1",
		"":                  "",
	} {
		if got := remoteHost(addr); got != want {
			t.Errorf("remoteHost(%q) = %q, want %q", addr, got, want)
		}
	}
}

// TestCompleteAuditEventBounds: the request's own values are cut to the
// columns' sizes, and Details is always a JSON object.
func TestCompleteAuditEventBounds(t *testing.T) {
	ctx := context.WithValue(context.Background(), requestInfoKey{}, requestInfo{
		ip:        strings.Repeat("1", 100),
		userAgent: strings.Repeat("u", 1000),
	})
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	e := completeAuditEvent(ctx, AuditEvent{
		Action:     AuditAction("app." + strings.Repeat("a", 100)),
		TargetType: strings.Repeat("t", 100),
		TargetID:   strings.Repeat("k", 300),
		RequestID:  strings.Repeat("r", 200),
	}, now)
	if len(e.IP) != 64 || len(e.UserAgent) != 255 || len(e.RequestID) != 64 {
		t.Fatalf("lengths ip=%d ua=%d request=%d, want 64/255/64", len(e.IP), len(e.UserAgent), len(e.RequestID))
	}
	if len(e.Action) != 64 || len(e.TargetType) != 64 || len(e.TargetID) != 128 {
		t.Fatalf("lengths action=%d target_type=%d target_id=%d, want 64/64/128", len(e.Action), len(e.TargetType), len(e.TargetID))
	}
	if e.Details != "{}" || !e.OccurredAt.Equal(now) || e.ActorUserID != nil {
		t.Fatalf("completed = %+v", e)
	}
	// A cut inside a multi-byte character backs off to its start.
	if got := truncate(strings.Repeat("a", 254)+"é", 255); got != strings.Repeat("a", 254) || !utf8.ValidString(got) {
		t.Fatalf("truncate split a character: %q", got)
	}
	if got := details("scope", "all", "dangling"); got != `{"scope":"all"}` {
		t.Fatalf("details = %s", got)
	}
}
