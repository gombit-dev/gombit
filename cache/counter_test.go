package cache

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// TestMemoryIncrementTTL: the first increment gives the counter its ttl, the
// next ones keep that deadline (a window is not stretched by its hits), and
// once it expires the counter starts again.
func TestMemoryIncrementTTL(t *testing.T) {
	ctx := context.Background()
	c := NewMemory()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }

	for want := int64(1); want <= 3; want++ {
		got, err := c.IncrementTTL(ctx, "hits", 1, time.Minute)
		if err != nil || got != want {
			t.Fatalf("IncrementTTL() = %d, %v; want %d", got, err, want)
		}
		now = now.Add(10 * time.Second)
	}
	now = now.Add(30 * time.Second) // 60s after the first hit
	if got, err := c.IncrementTTL(ctx, "hits", 1, time.Minute); err != nil || got != 1 {
		t.Fatalf("IncrementTTL() after the deadline = %d, %v; want a new counter at 1", got, err)
	}
}

// TestMemoryIncrementTTLGivesAnExpiryToAPlainCounter: a counter written by
// Increment (no expiry) gets ttl from its first IncrementTTL, so it cannot
// live forever.
func TestMemoryIncrementTTLGivesAnExpiryToAPlainCounter(t *testing.T) {
	ctx := context.Background()
	c := NewMemory()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }

	if _, err := c.Increment(ctx, "hits", 5); err != nil {
		t.Fatal(err)
	}
	if got, err := c.IncrementTTL(ctx, "hits", 1, time.Minute); err != nil || got != 6 {
		t.Fatalf("IncrementTTL() = %d, %v; want 6", got, err)
	}
	now = now.Add(time.Minute)
	if found, err := c.Get(ctx, "hits", new(int64)); err != nil || found {
		t.Fatalf("Get() after the ttl = found %v, %v; want expired", found, err)
	}
	for _, ttl := range []time.Duration{-time.Second, 0} {
		if _, err := c.IncrementTTL(ctx, "hits", 1, ttl); err == nil {
			t.Fatalf("IncrementTTL() with ttl %s = nil error", ttl)
		}
	}
	if _, err := (Noop{}).IncrementTTL(ctx, "hits", 1, 0); err == nil {
		t.Fatal("Noop.IncrementTTL() with no ttl = nil error")
	}
}

func TestMemoryIncrementTTLConcurrent(t *testing.T) {
	ctx := context.Background()
	c := NewMemory()
	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() {
			if _, err := c.IncrementTTL(ctx, "hits", 1, time.Hour); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	var got int64
	if found, err := c.Get(ctx, "hits", &got); err != nil || !found || got != 64 {
		t.Fatalf("after 64 concurrent increments = %d (found %v, %v), want 64", got, found, err)
	}
	if c.items["hits"].expiresAt.IsZero() {
		t.Fatal("the counter has no expiry")
	}
}

// plainCache hides a Memory's Counter: a third-party Cache.
type plainCache struct{ Cache }

// TestIncrementTTLWithoutCounter: the fallback still gives the counter its
// ttl, through Namespace and Store too.
func TestIncrementTTLWithoutCounter(t *testing.T) {
	ctx := context.Background()
	mem := NewMemory()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	mem.now = func() time.Time { return now }
	caches := map[string]Cache{
		"plain":     plainCache{mem},
		"namespace": Namespace("app", plainCache{mem}),
		"store":     &Store{impl: plainCache{mem}},
	}
	for name, c := range caches {
		key := "hits-" + name
		for want := int64(1); want <= 2; want++ {
			if got, err := IncrementTTL(ctx, c, key, 1, time.Minute); err != nil || got != want {
				t.Fatalf("%s: IncrementTTL() = %d, %v; want %d", name, got, err, want)
			}
		}
	}
	now = now.Add(time.Minute)
	for name, c := range caches {
		if got, err := IncrementTTL(ctx, c, "hits-"+name, 1, time.Minute); err != nil || got != 1 {
			t.Fatalf("%s: IncrementTTL() after the ttl = %d, %v; want 1", name, got, err)
		}
	}
	if _, err := IncrementTTL(ctx, mem, "hits", 1, 0); err == nil {
		t.Fatal("IncrementTTL() with no ttl = nil error")
	}
}

// TestNamespaceIncrementTTL: the namespace's prefix and the Counter of the
// cache under it.
func TestNamespaceIncrementTTL(t *testing.T) {
	ctx := context.Background()
	mem := NewMemory()
	c := Namespace("app", mem)
	if _, ok := c.(Counter); !ok {
		t.Fatal("Namespace() is not a Counter")
	}
	if got, err := IncrementTTL(ctx, c, "hits", 2, time.Minute); err != nil || got != 2 {
		t.Fatalf("IncrementTTL() = %d, %v; want 2", got, err)
	}
	item, ok := mem.items["app:hits"]
	if !ok || item.expiresAt.IsZero() {
		t.Fatalf("the namespaced counter = %+v (found %v), want app:hits with an expiry", item, ok)
	}
}

func TestNoopIncrementTTL(t *testing.T) {
	if got, err := (Noop{}).IncrementTTL(context.Background(), "hits", 1, time.Minute); err != nil || got != 0 {
		t.Fatalf("Noop.IncrementTTL() = %d, %v; want 0", got, err)
	}
}

// TestRedisIncrementTTL runs against a real server when
// GOMBIT_TEST_REDIS_ADDR is set (CI's "Jobs (redis)" job sets it).
func TestRedisIncrementTTL(t *testing.T) {
	addr := strings.TrimSpace(os.Getenv("GOMBIT_TEST_REDIS_ADDR"))
	if addr == "" {
		t.Skip("GOMBIT_TEST_REDIS_ADDR not set; skipping the Redis counter test")
	}
	ctx := context.Background()
	client := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("ping Redis at %s: %v", addr, err)
	}
	c := NewRedis(client)
	key := fmt.Sprintf("gombit-test-counter-%d", time.Now().UnixNano())
	t.Cleanup(func() { client.Del(context.Background(), key, key+"-plain") })

	for want := int64(1); want <= 3; want++ {
		if got, err := c.IncrementTTL(ctx, key, 1, time.Minute); err != nil || got != want {
			t.Fatalf("IncrementTTL() = %d, %v; want %d", got, err, want)
		}
	}
	ttl := client.PTTL(ctx, key).Val()
	if ttl <= 0 || ttl > time.Minute {
		t.Fatalf("PTTL after IncrementTTL = %s, want (0, 1m]", ttl)
	}
	// A deadline is kept: a longer ttl does not stretch it.
	if _, err := c.IncrementTTL(ctx, key, 1, time.Hour); err != nil {
		t.Fatal(err)
	}
	if again := client.PTTL(ctx, key).Val(); again > time.Minute {
		t.Fatalf("PTTL after another IncrementTTL = %s, want the first deadline kept", again)
	}
	// A counter written without an expiry gets one.
	if _, err := c.Increment(ctx, key+"-plain", 1); err != nil {
		t.Fatal(err)
	}
	if got, err := c.IncrementTTL(ctx, key+"-plain", 1, time.Minute); err != nil || got != 2 {
		t.Fatalf("IncrementTTL() on a plain counter = %d, %v; want 2", got, err)
	}
	if ttl := client.PTTL(ctx, key+"-plain").Val(); ttl <= 0 {
		t.Fatalf("PTTL of the plain counter = %s, want an expiry", ttl)
	}
	// DecrementExisting: of a counter that exists, keeping its expiry; a
	// missing one is not created.
	if got, found, err := c.DecrementExisting(ctx, key, 1); err != nil || !found || got != 3 {
		t.Fatalf("DecrementExisting() = %d, %v, %v; want 3", got, found, err)
	}
	if ttl := client.PTTL(ctx, key).Val(); ttl <= 0 {
		t.Fatalf("PTTL after DecrementExisting = %s, want the expiry kept", ttl)
	}
	missing := key + "-missing"
	if _, found, err := c.DecrementExisting(ctx, missing, 1); err != nil || found {
		t.Fatalf("DecrementExisting() of a missing key = found %v, %v", found, err)
	}
	if n := client.Exists(ctx, missing).Val(); n != 0 {
		t.Fatal("DecrementExisting created the missing key")
	}
	var wg sync.WaitGroup
	conc := key + "-concurrent"
	t.Cleanup(func() { client.Del(context.Background(), conc) })
	for range 32 {
		wg.Go(func() {
			if _, err := c.IncrementTTL(ctx, conc, 1, time.Minute); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if got := client.Get(ctx, conc).Val(); got != "32" {
		t.Fatalf("after 32 concurrent increments = %q, want 32", got)
	}
}

// TestDecrementExisting: a decrement only of a counter that exists, keeping
// its expiry; a missing or expired one is not created (memory, namespaced,
// Store, noop, and a Cache without a Counter, which writes nothing).
func TestDecrementExisting(t *testing.T) {
	ctx := context.Background()
	mem := NewMemory()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	mem.now = func() time.Time { return now }
	for name, c := range map[string]Cache{"memory": mem, "namespace": Namespace("app", mem), "store": &Store{impl: mem}} {
		key := "hits-" + name
		if _, found, err := DecrementExisting(ctx, c, key, 1); err != nil || found {
			t.Fatalf("%s: a missing counter = found %v, %v", name, found, err)
		}
		if found, _ := c.Get(ctx, key, new(int64)); found {
			t.Fatalf("%s: DecrementExisting created the counter", name)
		}
		if _, err := IncrementTTL(ctx, c, key, 3, time.Minute); err != nil {
			t.Fatal(err)
		}
		if got, found, err := DecrementExisting(ctx, c, key, 1); err != nil || !found || got != 2 {
			t.Fatalf("%s: DecrementExisting() = %d, %v, %v; want 2", name, got, found, err)
		}
	}
	// The expiry is kept: once it passes, the counter is gone, not
	// decremented into a new one.
	now = now.Add(time.Minute)
	if _, found, err := mem.DecrementExisting(ctx, "hits-memory", 1); err != nil || found {
		t.Fatalf("an expired counter = found %v, %v", found, err)
	}
	if _, found, _ := (Noop{}).DecrementExisting(ctx, "x", 1); found {
		t.Fatal("Noop found a counter")
	}
	plain := plainCache{NewMemory()}
	if _, err := plain.Increment(ctx, "p", 1); err != nil {
		t.Fatal(err)
	}
	if _, found, err := DecrementExisting(ctx, plain, "p", 1); err != nil || found {
		t.Fatalf("a Cache without a Counter = found %v, %v; want nothing done", found, err)
	}
	var left int64
	if _, err := plain.Get(ctx, "p", &left); err != nil || left != 1 {
		t.Fatalf("a Cache without a Counter's value = %d, %v; want it untouched", left, err)
	}
}
