package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/cache"
	"go.uber.org/zap"
)

func TestAddressSubject(t *testing.T) {
	for in, want := range map[string]string{
		"198.51.100.7":            "198.51.100.7",
		"::ffff:198.51.100.7":     "198.51.100.7",
		"2001:db8:1:2::1":         "2001:db8:1:2::/64",
		"2001:db8:1:2:ffff:1:2:3": "2001:db8:1:2::/64",
		"fe80::1%eth0":            "fe80::/64",
		"not an address":          "not an address",
	} {
		if got := addressSubject(in); got != want {
			t.Errorf("addressSubject(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestThrottleRefundAfterClear: a rule cleared between an attempt's place
// and its refund (a reset during a login) leaves no counter behind, and the
// instance keeps counting in the shared store.
func TestThrottleRefundAfterClear(t *testing.T) {
	ctx := context.Background()
	shared := cache.NewMemory()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	th := &throttler{policy: DefaultThrottlePolicy(), store: shared, now: func() time.Time { return now }, logger: zap.NewNop}
	rule := throttleRule{name: "LoginPerAccount", limit: ThrottleLimit{Limit: 3, Window: time.Hour}, subject: "ada@example.com"}

	res, err := th.reserve(ctx, rule)
	if err != nil {
		t.Fatal(err)
	}
	th.clear(ctx, rule) // the reset
	res.refund()        // the login, after its hash
	current, _, _ := th.buckets(rule)
	if found, err := shared.Get(ctx, current, new(int64)); err != nil || found {
		t.Fatalf("the cleared bucket after the refund: found %v (%v), want no counter recreated", found, err)
	}
	// The next attempts count in the shared store, from one, to the limit.
	for i := range 3 {
		if _, err := th.reserve(ctx, rule); err != nil {
			t.Fatalf("attempt %d = %v", i+1, err)
		}
	}
	if _, err := th.reserve(ctx, rule); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("the fourth attempt = %v, want refused", err)
	}
	var count int64
	if found, _ := shared.Get(ctx, current, &count); !found || count != 3 || th.keepsNothing.Load() {
		t.Fatalf("shared count = %d (found %v), keepsNothing = %v; want 3 in the shared store", count, found, th.keepsNothing.Load())
	}
}
