package jobs_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/jobs"
)

// redisTestAddr is a Redis server for the durable-driver tests. CI's
// "Jobs (redis)" job sets it; without it the tests skip.
func redisTestAddr(t *testing.T) string {
	t.Helper()
	addr := strings.TrimSpace(os.Getenv("GOMBIT_TEST_REDIS_ADDR"))
	if addr == "" {
		t.Skip("GOMBIT_TEST_REDIS_ADDR not set; skipping Redis queue tests")
	}
	return addr
}

// redisClient connects to the test server and removes the namespace's keys
// when the test ends.
func redisClient(t *testing.T, addr, namespace string) *redis.Client {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: addr})
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("ping Redis at %s: %v", addr, err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		iter := client.Scan(ctx, 0, "{"+namespace+":*", 100).Iterator()
		for iter.Next(ctx) {
			client.Del(ctx, iter.Val())
		}
		_ = client.Close()
	})
	return client
}

func testNamespace() string { return "gombit-test-" + uuid.NewString()[:8] }

func TestRedisQueueConformance(t *testing.T) {
	addr := redisTestAddr(t)
	runQueueConformance(t, func(t *testing.T, clock *fakeClock) jobs.Queue {
		ns := testNamespace()
		return jobs.NewRedisQueue(redisClient(t, addr, ns), ns, jobs.WithRedisClock(clock.Now))
	})
}

// TestRedisQueueSurvivesRestarts: queued, delayed, and leased jobs live in
// Redis, so a new process (a new client and queue) finds them, including the
// attempt count and a crashed worker's job.
func TestRedisQueueSurvivesRestarts(t *testing.T) {
	addr := redisTestAddr(t)
	ctx := context.Background()
	ns := testNamespace()
	clock := newFakeClock()

	before := jobs.NewRedisQueue(redisClient(t, addr, ns), ns, jobs.WithRedisClock(clock.Now))
	for _, id := range []string{"queued", "crashed"} {
		if err := before.Push(ctx, "default", envelope(id), time.Time{}); err != nil {
			t.Fatal(err)
		}
		clock.Advance(time.Millisecond)
	}
	if err := before.Push(ctx, "default", envelope("delayed"), clock.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// A worker leases "queued" and the process dies with it.
	if d := mustReserve(t, before, "default"); d.Envelope.ID != "queued" {
		t.Fatalf("reserved %s, want queued", d.Envelope.ID)
	}
	if err := before.Close(); err != nil {
		t.Fatal(err)
	}

	after := jobs.NewRedisQueue(redisClient(t, addr, ns), ns, jobs.WithRedisClock(clock.Now))
	crashed := mustReserve(t, after, "default")
	if crashed.Envelope.ID != "crashed" || crashed.Envelope.Attempt != 1 {
		t.Fatalf("after restart reserved %+v, want crashed at attempt 1", crashed.Envelope)
	}
	if err := after.Ack(ctx, crashed); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute) // the dead worker's lease on "queued" runs out
	d := mustReserve(t, after, "default")
	if d.Envelope.ID != "queued" || d.Envelope.Attempt != 2 {
		t.Fatalf("after the lease expired reserved %+v, want queued at attempt 2", d.Envelope)
	}
	if err := after.Ack(ctx, d); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Hour)
	if d := mustReserve(t, after, "default"); d.Envelope.ID != "delayed" {
		t.Fatalf("reserved %s, want the delayed job at its time", d.Envelope.ID)
	}
}

// TestRedisQueueHandsBackAnUndecodableEnvelope: a stored envelope that no
// longer decodes comes back as a leased delivery carrying the decode error,
// so the caller can ack it away.
func TestRedisQueueHandsBackAnUndecodableEnvelope(t *testing.T) {
	addr := redisTestAddr(t)
	ctx := context.Background()
	ns := testNamespace()
	client := redisClient(t, addr, ns)
	q := jobs.NewRedisQueue(client, ns)
	if err := q.Push(ctx, "default", envelope("poison"), time.Time{}); err != nil {
		t.Fatal(err)
	}
	jobKey := fmt.Sprintf("{%s:jobs:default}:job:poison", ns)
	if err := client.HSet(ctx, jobKey, "env", "{not json").Err(); err != nil {
		t.Fatal(err)
	}

	// A plain `if err != nil` caller still holds the lease: the failure rides
	// on the delivery, not the error.
	d, err := q.Reserve(ctx, []string{"default"}, time.Minute)
	if err != nil || !errors.Is(d.Err, jobs.ErrDecode) || d.Envelope.ID != "poison" || d.Receipt == "" || d.Envelope.Attempt != 1 {
		t.Fatalf("Reserve(poison) = %+v, %v; want a leased delivery carrying the decode error", d, err)
	}
	if err := q.Ack(ctx, d); err != nil {
		t.Fatalf("Ack(poison) error = %v", err)
	}
	expectEmpty(t, q, "default")
}

func TestRedisQueueCloseOwnership(t *testing.T) {
	addr := redisTestAddr(t)
	ns := testNamespace()
	borrowed := redisClient(t, addr, ns)
	if err := jobs.NewRedisQueue(borrowed, ns).Close(); err != nil {
		t.Fatal(err)
	}
	if err := borrowed.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("Close closed a client the queue does not own: %v", err)
	}
	if err := jobs.NewRedisQueue(borrowed, ns).Push(context.Background(), "default", envelope("x"), time.Time{}); err != nil {
		t.Fatalf("a new queue on the borrowed client: %v", err)
	}
	owned := redis.NewClient(&redis.Options{Addr: addr})
	if err := jobs.NewRedisQueue(owned, ns, jobs.WithRedisClientOwned()).Close(); err != nil {
		t.Fatal(err)
	}
	if err := owned.Ping(context.Background()).Err(); err == nil {
		t.Fatal("Close left an owned client open")
	}
}

// TestRedisQueueCallsHonorTheirDeadline: go-redis ignores the caller's
// context unless ContextTimeoutEnabled is set. The queue's client sets it,
// so a stalled server cannot hold a call past its deadline; the worker's
// shutdown budget depends on that.
func TestRedisQueueCallsHonorTheirDeadline(t *testing.T) {
	addr := redisTestAddr(t)
	ns := testNamespace()
	admin := redisClient(t, addr, ns)
	cfg := config.JobsConfig{Driver: config.JobsDriverRedis, Queue: "default", Namespace: ns}
	d, err := jobs.OpenWithRedis(cfg, admin, jobs.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	q := d.Queue()
	if err := q.Push(context.Background(), "default", envelope("x"), time.Time{}); err != nil {
		t.Fatal(err)
	}

	// Stall every client for 2s (longer than the call's 200ms deadline and
	// shorter than the socket read timeout, so only the deadline can end it).
	if err := admin.Do(context.Background(), "CLIENT", "PAUSE", 2000, "ALL").Err(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Do(context.Background(), "CLIENT", "UNPAUSE").Err() })
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	begin := time.Now()
	_, err = q.Reserve(ctx, []string{"default"}, time.Minute)
	if took := time.Since(begin); err == nil || took > time.Second {
		t.Fatalf("Reserve on a stalled server = %v after %s, want an error at its 200ms deadline", err, took)
	}

	opts := jobs.RedisClientOptions(&redis.Options{Addr: addr, MaxRetries: 3})
	if !opts.ContextTimeoutEnabled || opts.MaxRetries != -1 || opts.Addr != addr {
		t.Fatalf("RedisClientOptions = %+v", opts)
	}
}
