package cache

import (
	"context"
	"errors"
	"fmt"
	"reflect"
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

func TestMemoryIncrementRejectsNonIntegerValue(t *testing.T) {
	ctx := context.Background()
	c := NewMemory()
	if err := c.Set(ctx, "counter", "not-an-int", 0); err != nil {
		t.Fatalf("Set() error = %v, want nil", err)
	}

	_, err := c.Increment(ctx, "counter", 1)
	if err == nil {
		t.Fatal("Increment() error = nil, want error")
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
