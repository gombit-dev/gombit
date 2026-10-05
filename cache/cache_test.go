package cache

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/config"
)

type cachedWidget struct {
	ID   int
	Name string
}

func TestMemoryGetSetDeleteValueSemantics(t *testing.T) {
	ctx := context.Background()
	c := NewMemory()
	want := cachedWidget{ID: 1, Name: "stored"}

	if err := c.Set(ctx, "widgets:1", want, time.Minute); err != nil {
		t.Fatalf("Set() error = %v, want nil", err)
	}

	var got cachedWidget
	found, err := c.Get(ctx, "widgets:1", &got)
	if err != nil {
		t.Fatalf("Get() error = %v, want nil", err)
	}
	if !found {
		t.Fatal("Get() found = false, want true")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Get() value = %#v, want %#v", got, want)
	}

	if err := c.Delete(ctx, "widgets:1"); err != nil {
		t.Fatalf("Delete() error = %v, want nil", err)
	}
	found, err = c.Get(ctx, "widgets:1", &got)
	if err != nil {
		t.Fatalf("Get() after Delete() error = %v, want nil", err)
	}
	if found {
		t.Fatal("Get() after Delete() found = true, want false")
	}
}

func TestMemoryExpiresValues(t *testing.T) {
	ctx := context.Background()
	c := NewMemory()
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }

	if err := c.Set(ctx, "short", "value", time.Second); err != nil {
		t.Fatalf("Set() error = %v, want nil", err)
	}
	now = now.Add(time.Second)

	var got string
	found, err := c.Get(ctx, "short", &got)
	if err != nil {
		t.Fatalf("Get() error = %v, want nil", err)
	}
	if found {
		t.Fatal("Get() found = true, want false after ttl")
	}
}

func TestMemoryIncrementPreservesExistingTTL(t *testing.T) {
	ctx := context.Background()
	c := NewMemory()
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }

	if err := c.Set(ctx, "counter", int64(1), time.Second); err != nil {
		t.Fatalf("Set() error = %v, want nil", err)
	}
	count, err := c.Increment(ctx, "counter", 1)
	if err != nil {
		t.Fatalf("Increment() error = %v, want nil", err)
	}
	if count != 2 {
		t.Fatalf("Increment() = %d, want 2", count)
	}

	now = now.Add(time.Second)
	var got int64
	found, err := c.Get(ctx, "counter", &got)
	if err != nil {
		t.Fatalf("Get() error = %v, want nil", err)
	}
	if found {
		t.Fatalf("Get() found = true, want false after original ttl; value = %d", got)
	}
}

func TestMemoryIncrementValueSemantics(t *testing.T) {
	ctx := context.Background()
	c := NewMemory()

	got, err := c.Increment(ctx, "rate:client", 1)
	if err != nil {
		t.Fatalf("Increment() error = %v, want nil", err)
	}
	if got != 1 {
		t.Fatalf("Increment() = %d, want 1", got)
	}

	got, err = c.Increment(ctx, "rate:client", 4)
	if err != nil {
		t.Fatalf("Increment() error = %v, want nil", err)
	}
	if got != 5 {
		t.Fatalf("Increment() = %d, want 5", got)
	}
}

// Memory refuses to wrap a counter past the int64 range, as Redis INCRBY
// does ("increment or decrement would overflow"), and leaves the stored value
// as it was, so the dev driver and the production one agree (issue #437).
func TestMemoryIncrementRefusesToOverflow(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		start, delta int64
		wantErr      bool
		want         int64
	}{
		"MaxInt64 + 1":        {start: math.MaxInt64, delta: 1, wantErr: true},
		"MinInt64 - 1":        {start: math.MinInt64, delta: -1, wantErr: true},
		"-1 + MinInt64":       {start: -1, delta: math.MinInt64, wantErr: true},
		"1 + MaxInt64":        {start: 1, delta: math.MaxInt64, wantErr: true},
		"MaxInt64 + 0":        {start: math.MaxInt64, delta: 0, want: math.MaxInt64},
		"0 + MinInt64":        {start: 0, delta: math.MinInt64, want: math.MinInt64},
		"MaxInt64 - 1 + 1":    {start: math.MaxInt64 - 1, delta: 1, want: math.MaxInt64},
		"MinInt64 + MaxInt64": {start: math.MinInt64, delta: math.MaxInt64, want: -1},
		"MaxInt64 + MinInt64": {start: math.MaxInt64, delta: math.MinInt64, want: -1},
		"MinInt64 + 1 + (-1)": {start: math.MinInt64 + 1, delta: -1, want: math.MinInt64},
	} {
		t.Run(name, func(t *testing.T) {
			c := NewMemory()
			if err := c.Set(ctx, "counter", tc.start, 0); err != nil {
				t.Fatal(err)
			}
			got, err := c.Increment(ctx, "counter", tc.delta)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "would overflow") {
					t.Fatalf("Increment(%d, %d) = %d, %v; want an overflow error", tc.start, tc.delta, got, err)
				}
				var stored int64
				if ok, err := c.Get(ctx, "counter", &stored); err != nil || !ok || stored != tc.start {
					t.Fatalf("stored value = %d (found %v, err %v), want it left at %d", stored, ok, err, tc.start)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("Increment(%d, %d) = %d, %v; want %d", tc.start, tc.delta, got, err, tc.want)
			}
		})
	}
}

// A stored value that is not an integer is refused and left as it was, as
// Redis INCRBY refuses it ("value is not an integer"). A stored nil is JSON
// null, which json.Unmarshal into an int64 silently skips; it must be refused
// too, not overwritten with the delta.
func TestMemoryIncrementRejectsNonIntegerValue(t *testing.T) {
	ctx := context.Background()
	for name, value := range map[string]any{
		"string": "not-an-int",
		"float":  5.5,
		"bool":   true,
		"nil":    nil,
	} {
		t.Run(name, func(t *testing.T) {
			c := NewMemory()
			if err := c.Set(ctx, "counter", value, 0); err != nil {
				t.Fatalf("Set() error = %v, want nil", err)
			}
			if got, err := c.Increment(ctx, "counter", 1); err == nil || !strings.Contains(err.Error(), "not an integer") {
				t.Fatalf("Increment() = %d, %v; want a not-an-integer error", got, err)
			}
			var stored any
			if ok, err := c.Get(ctx, "counter", &stored); err != nil || !ok || stored != value {
				t.Fatalf("stored value = %v (found %v, err %v), want it left at %v", stored, ok, err, value)
			}
		})
	}
}

func TestNoopDriver(t *testing.T) {
	ctx := context.Background()
	var c Cache = Noop{}

	if err := c.Set(ctx, "ignored", cachedWidget{ID: 1}, time.Minute); err != nil {
		t.Fatalf("Set() error = %v, want nil", err)
	}
	var got cachedWidget
	found, err := c.Get(ctx, "ignored", &got)
	if err != nil {
		t.Fatalf("Get() error = %v, want nil", err)
	}
	if found {
		t.Fatal("Get() found = true, want false")
	}
	count, err := c.Increment(ctx, "rate:client", 1)
	if err != nil {
		t.Fatalf("Increment() error = %v, want nil", err)
	}
	if count != 0 {
		t.Fatalf("Increment() = %d, want 0", count)
	}
}

func TestOpenMemoryDriverFromConfig(t *testing.T) {
	cfg := config.Default().Cache
	cfg.Driver = config.CacheDriverMemory
	cfg.Namespace = "test"

	store, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open() error = %v, want nil", err)
	}
	if store.Driver() != DriverMemory {
		t.Fatalf("Driver() = %q, want %q", store.Driver(), DriverMemory)
	}
	if store.Redis() != nil {
		t.Fatalf("Redis() = %v, want nil", store.Redis())
	}

	if err := store.Set(context.Background(), "key", "value", 0); err != nil {
		t.Fatalf("Set() error = %v, want nil", err)
	}
	var got string
	found, err := store.Get(context.Background(), "key", &got)
	if err != nil {
		t.Fatalf("Get() error = %v, want nil", err)
	}
	if !found || got != "value" {
		t.Fatalf("Get() = (%t, %q), want (true, value)", found, got)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("second Close() error = %v, want nil", err)
	}
}

// TestMemorySweepExpiredRemovesUnreadKeys is the regression test for #199:
// a key that is set and never read again (rate limits, one-time tokens,
// nonces) must still be reclaimed once expired. Deterministic — no
// goroutine, no real sleep — via the same fake-clock pattern
// TestMemoryExpiresValues uses, calling sweepExpired directly.
func TestMemorySweepExpiredRemovesUnreadKeys(t *testing.T) {
	ctx := context.Background()
	c := NewMemory()
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }

	if err := c.Set(ctx, "expiring:1", "value", time.Second); err != nil {
		t.Fatalf("Set(expiring:1) error = %v, want nil", err)
	}
	if err := c.Set(ctx, "expiring:2", "value", time.Second); err != nil {
		t.Fatalf("Set(expiring:2) error = %v, want nil", err)
	}
	if err := c.Set(ctx, "keeps", "value", time.Minute); err != nil {
		t.Fatalf("Set(keeps) error = %v, want nil", err)
	}
	if err := c.Set(ctx, "forever", "value", 0); err != nil {
		t.Fatalf("Set(forever) error = %v, want nil", err)
	}

	now = now.Add(time.Second)
	c.sweepExpired()

	c.mu.RLock()
	remaining := len(c.items)
	_, expiring1 := c.items["expiring:1"]
	_, expiring2 := c.items["expiring:2"]
	_, keeps := c.items["keeps"]
	_, forever := c.items["forever"]
	c.mu.RUnlock()

	if expiring1 || expiring2 {
		t.Fatalf("sweepExpired() left expired keys behind; items = %d", remaining)
	}
	if !keeps || !forever {
		t.Fatalf("sweepExpired() removed a non-expired key; keeps=%t forever=%t", keeps, forever)
	}
	if remaining != 2 {
		t.Fatalf("len(items) after sweep = %d, want 2", remaining)
	}
}

// TestMemorySweepExpiredBatchesLargeMaps proves sweepExpired never holds
// the write lock for a whole large map in one acquisition: with a batch
// size of 10 and 25 keys, it must take multiple sweepBatch calls (3, not
// 1), so no single lock hold is proportional to the full map size — the
// fix for the lock-contention finding on #199's own high-cardinality
// scenario. Deterministic: only counts batches and checks final contents,
// no timing or concurrency involved.
func TestMemorySweepExpiredBatchesLargeMaps(t *testing.T) {
	ctx := context.Background()
	c := NewMemory()
	c.sweepBatchSize = 10
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }

	const total = 25
	for i := range total {
		key := fmt.Sprintf("rate:%d", i)
		if err := c.Set(ctx, key, "1", time.Second); err != nil {
			t.Fatalf("Set(%s) error = %v, want nil", key, err)
		}
	}
	// One key survives the sweep so batching (not just an emptied map) is
	// what's under test.
	if err := c.Set(ctx, "keeps", "value", time.Minute); err != nil {
		t.Fatalf("Set(keeps) error = %v, want nil", err)
	}

	now = now.Add(time.Second)
	batches := c.sweepExpired()

	if batches != 3 {
		t.Fatalf("sweepExpired() batches = %d, want 3 (25 expired keys + 1 kept key, batch size 10)", batches)
	}
	c.mu.RLock()
	remaining := len(c.items)
	_, keeps := c.items["keeps"]
	c.mu.RUnlock()
	if remaining != 1 || !keeps {
		t.Fatalf("items after sweep = %d (keeps=%t), want 1 (keeps=true)", remaining, keeps)
	}
}

// TestMemoryJanitorReclaimsExpiredKeys proves the background goroutine
// started by WithJanitor is actually wired to sweepExpired and that Close
// stops it (and is idempotent). The sweep logic itself is already covered
// deterministically above; this only exercises the real-timing wiring, with
// a bounded poll instead of a fixed sleep.
func TestMemoryJanitorReclaimsExpiredKeys(t *testing.T) {
	ctx := context.Background()
	c := NewMemory(WithJanitor(5 * time.Millisecond))
	t.Cleanup(func() { _ = c.Close() })

	if err := c.Set(ctx, "rate:client", "1", 10*time.Millisecond); err != nil {
		t.Fatalf("Set() error = %v, want nil", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.RLock()
		_, ok := c.items["rate:client"]
		c.mu.RUnlock()
		if !ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	c.mu.RLock()
	_, stillThere := c.items["rate:client"]
	c.mu.RUnlock()
	if stillThere {
		t.Fatal("janitor did not reclaim the expired key within the deadline")
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close() error = %v, want nil", err)
	}
}

// TestMemoryNoJanitorCloseIsNoop confirms NewMemory() with no option never
// starts a goroutine and Close is still safe to call on it, so the ~10
// existing direct NewMemory() call sites in this file and namespace_test.go
// stay unaffected by #199's fix.
func TestMemoryNoJanitorCloseIsNoop(t *testing.T) {
	c := NewMemory()
	if err := c.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}
}

func TestCacheAndRateLimiterUsersCompileAgainstInterface(t *testing.T) {
	ctx := context.Background()
	c := NewMemory()

	if err := writeCacheUser(ctx, c); err != nil {
		t.Fatalf("writeCacheUser() error = %v, want nil", err)
	}
	limited, err := rateLimiterUser(ctx, c, "client:1", 2)
	if err != nil {
		t.Fatalf("rateLimiterUser() error = %v, want nil", err)
	}
	if limited {
		t.Fatal("rateLimiterUser() limited = true, want false")
	}
	limited, err = rateLimiterUser(ctx, c, "client:1", 2)
	if err != nil {
		t.Fatalf("rateLimiterUser() error = %v, want nil", err)
	}
	if limited {
		t.Fatal("rateLimiterUser() limited = true, want false")
	}
	limited, err = rateLimiterUser(ctx, c, "client:1", 2)
	if err != nil {
		t.Fatalf("rateLimiterUser() error = %v, want nil", err)
	}
	if !limited {
		t.Fatal("rateLimiterUser() limited = false, want true")
	}
}

func TestContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := NewMemory()

	if err := c.Set(ctx, "key", "value", 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("Set() error = %v, want context.Canceled", err)
	}
	if _, err := c.Get(ctx, "key", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Get() error = %v, want context.Canceled", err)
	}
	if err := c.Delete(ctx, "key"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Delete() error = %v, want context.Canceled", err)
	}
	if _, err := c.Increment(ctx, "key", 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("Increment() error = %v, want context.Canceled", err)
	}
}

func writeCacheUser(ctx context.Context, c Cache) error {
	return c.Set(ctx, "widgets:list", []cachedWidget{{ID: 1, Name: "one"}}, time.Minute)
}

func rateLimiterUser(ctx context.Context, c Cache, key string, limit int64) (bool, error) {
	count, err := c.Increment(ctx, "rate:"+key, 1)
	if err != nil {
		return false, err
	}
	return count > limit, nil
}

// TestMemoryJanitorNonPositiveIntervalIsOff locks issue #436: a zero or
// negative interval (an unset or misparsed config value) used to reach
// time.NewTicker inside the janitor goroutine and panic there, where the caller
// cannot recover it, killing the process. It now means no janitor at all, as if
// the option were omitted: the cache works, an expired key is still reclaimed
// on Get, and Close is a no-op.
func TestMemoryJanitorNonPositiveIntervalIsOff(t *testing.T) {
	ctx := context.Background()
	for _, interval := range []time.Duration{0, -time.Second} {
		c := NewMemory(WithJanitor(interval))
		if c.stop != nil || c.done != nil {
			t.Fatalf("WithJanitor(%v) started a janitor, want none", interval)
		}
		now := time.Now()
		c.now = func() time.Time { return now }
		if err := c.Set(ctx, "k", "v", time.Second); err != nil {
			t.Fatalf("WithJanitor(%v): Set() error = %v", interval, err)
		}
		var got string
		if ok, err := c.Get(ctx, "k", &got); err != nil || !ok || got != "v" {
			t.Fatalf("WithJanitor(%v): Get() = %v, %q, %v, want a hit on v", interval, ok, got, err)
		}
		now = now.Add(2 * time.Second)
		if ok, err := c.Get(ctx, "k", &got); err != nil || ok {
			t.Fatalf("WithJanitor(%v): Get() after expiry = %v, %v, want a miss", interval, ok, err)
		}
		c.mu.RLock()
		_, kept := c.items["k"]
		c.mu.RUnlock()
		if kept {
			t.Fatalf("WithJanitor(%v): Get() did not reclaim the expired key", interval)
		}
		if err := c.Close(); err != nil {
			t.Fatalf("WithJanitor(%v): Close() error = %v, want nil", interval, err)
		}
	}
}
