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
// so the caller can bury it, raw bytes kept.
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
	if err := q.Bury(ctx, d, jobs.Failure{Reason: jobs.ReasonUndecodable, Kind: jobs.KindDecode, Error: d.Err.Error()}); err != nil {
		t.Fatalf("Bury(poison) error = %v", err)
	}
	expectEmpty(t, q, "default")
	f, err := q.FailedJob(ctx, "default", "poison")
	if err != nil || string(f.RawEnvelope) != "{not json" || f.Envelope.ID != "poison" || f.Failure.Reason != jobs.ReasonUndecodable {
		t.Fatalf("FailedJob(poison) = %+v, %v; want the raw stored bytes kept", f, err)
	}
}

func TestRedisPurgeCrossesBatches(t *testing.T) {
	addr := redisTestAddr(t)
	defer jobs.SetPurgeBatch(3)()
	ns := testNamespace()
	ctx := context.Background()
	q := jobs.NewRedisQueue(redisClient(t, addr, ns), ns)
	for i := 0; i < 7; i++ {
		if err := q.Push(ctx, "default", envelope(fmt.Sprintf("p%d", i)), time.Time{}); err != nil {
			t.Fatal(err)
		}
		d := mustReserve(t, q, "default")
		if err := q.Bury(ctx, d, jobs.Failure{Reason: jobs.ReasonExhausted}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := q.PurgeFailed(ctx, "default", time.Time{}); err != nil || n != 7 {
		t.Fatalf("PurgeFailed across batches of 3 = %d, %v; want 7", n, err)
	}
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

// TestRedisRetryOfAnOrphanedFailedID: a failed ID whose job hash is gone
// (evicted, deleted by hand) is dropped, not queued as a job with no
// envelope.
func TestRedisRetryOfAnOrphanedFailedID(t *testing.T) {
	addr := redisTestAddr(t)
	ns := testNamespace()
	ctx := context.Background()
	client := redisClient(t, addr, ns)
	q := jobs.NewRedisQueue(client, ns)
	if err := q.Push(ctx, "default", envelope("orphan"), time.Time{}); err != nil {
		t.Fatal(err)
	}
	d := mustReserve(t, q, "default")
	if err := q.Bury(ctx, d, jobs.Failure{Reason: jobs.ReasonExhausted}); err != nil {
		t.Fatal(err)
	}
	if err := client.Del(ctx, fmt.Sprintf("{%s:jobs:default}:job:orphan", ns)).Err(); err != nil {
		t.Fatal(err)
	}
	if err := q.RetryFailed(ctx, "default", "orphan"); !errors.Is(err, jobs.ErrNotFailed) {
		t.Fatalf("RetryFailed(orphan) = %v, want ErrNotFailed", err)
	}
	expectEmpty(t, q, "default")
	if failed, _ := q.Failed(ctx, "default", 0); len(failed) != 0 {
		t.Fatalf("the orphan is still listed: %+v", failed)
	}
}

// TestRedisFailedReadsInPages: an unbounded or large listing is read in
// pages, in order, with nothing lost at the page seams.
func TestRedisFailedReadsInPages(t *testing.T) {
	addr := redisTestAddr(t)
	defer jobs.SetFailedPageSize(3)()
	ns := testNamespace()
	ctx := context.Background()
	clock := newFakeClock()
	q := jobs.NewRedisQueue(redisClient(t, addr, ns), ns, jobs.WithRedisClock(clock.Now))
	for i := 0; i < 7; i++ {
		if err := q.Push(ctx, "default", envelope(fmt.Sprintf("f%d", i)), time.Time{}); err != nil {
			t.Fatal(err)
		}
		d := mustReserve(t, q, "default")
		if err := q.Bury(ctx, d, jobs.Failure{Reason: jobs.ReasonExhausted, At: clock.Now()}); err != nil {
			t.Fatal(err)
		}
		clock.Advance(time.Second)
	}
	all, err := q.Failed(ctx, "default", 0)
	if err != nil || len(all) != 7 || all[0].Envelope.ID != "f6" || all[6].Envelope.ID != "f0" {
		t.Fatalf("Failed(all) over pages of 3 = %d jobs, %v", len(all), err)
	}
	five, err := q.Failed(ctx, "default", 5)
	if err != nil || len(five) != 5 || five[4].Envelope.ID != "f2" {
		t.Fatalf("Failed(5) over pages of 3 = %d jobs, %v", len(five), err)
	}
}
