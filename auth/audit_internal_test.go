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

// TestCompleteAuditEventBounds: identifiers are kept whole, and an event
// one does not fit is refused, not cut; only the User-Agent (free text) is
// cut to its column. Details is always a JSON object.
func TestCompleteAuditEventBounds(t *testing.T) {
	ctx := context.WithValue(context.Background(), requestInfoKey{}, requestInfo{
		ip:        "2001:db8::1",
		userAgent: strings.Repeat("u", 1000),
	})
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	longKey := strings.Repeat("k", 300)
	e := completeAuditEvent(ctx, AuditEvent{Action: AuditAdminUpdated, TargetType: "widgets", TargetID: longKey, RequestID: strings.Repeat("r", 128)}, now)
	if e.TargetID != longKey || len(e.RequestID) != 128 || e.IP != "2001:db8::1" || len(e.UserAgent) != 255 {
		t.Fatalf("completed: target_id %d bytes, request_id %d, ip %q, user_agent %d", len(e.TargetID), len(e.RequestID), e.IP, len(e.UserAgent))
	}
	if err := validateAuditEvent(e); err != nil {
		t.Fatalf("an event whose identifiers fit = %v", err)
	}
	for name, bad := range map[string]AuditEvent{
		"action":      {Action: AuditAction("app." + strings.Repeat("a", 100))},
		"target_type": {Action: AuditLogout, TargetType: strings.Repeat("t", 65)},
		"request_id":  {Action: AuditLogout, RequestID: strings.Repeat("r", 129)},
		"ip":          {Action: AuditLogout, IP: strings.Repeat("1", 65)},
	} {
		if err := validateAuditEvent(bad); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("an over-long %s = %v, want it refused", name, err)
		}
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
