package jobs_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/jobs"
)

// fakeClock is a settable clock shared by a queue under test.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: fixedNow} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// queueFactory returns a fresh, empty queue driven by clock.
type queueFactory func(t *testing.T, clock *fakeClock) jobs.Queue

func envelope(id string) jobs.Envelope {
	return jobs.Envelope{
		ID:         id,
		Name:       "send_welcome_email",
		Version:    1,
		Payload:    json.RawMessage(`{"user_id":1}`),
		Metadata:   map[string]string{"gombit.request_id": "req-" + id},
		EnqueuedAt: fixedNow,
	}
}

func mustReserve(t *testing.T, q jobs.Queue, queues ...string) jobs.Delivery {
	t.Helper()
	d, err := q.Reserve(context.Background(), queues, time.Minute)
	if err != nil {
		t.Fatalf("Reserve(%v) error = %v", queues, err)
	}
	return d
}

func expectEmpty(t *testing.T, q jobs.Queue, queues ...string) {
	t.Helper()
	if d, err := q.Reserve(context.Background(), queues, time.Minute); !errors.Is(err, jobs.ErrNoJob) {
		t.Fatalf("Reserve(%v) = %+v, %v; want ErrNoJob", queues, d, err)
	}
}

// runQueueConformance is the contract every driver keeps.
func runQueueConformance(t *testing.T, newQueue queueFactory) {
	ctx := context.Background()

	t.Run("fifo within a queue, queues in the order given", func(t *testing.T) {
		clock := newFakeClock()
		q := newQueue(t, clock)
		for _, id := range []string{"a1", "a2"} {
			if err := q.Push(ctx, "mail", envelope(id), time.Time{}); err != nil {
				t.Fatal(err)
			}
			clock.Advance(time.Millisecond)
		}
		if err := q.Push(ctx, "critical", envelope("c1"), time.Time{}); err != nil {
			t.Fatal(err)
		}
		expectEmpty(t, q)
		expectEmpty(t, q, "other")
		for _, want := range []string{"c1", "a1", "a2"} {
			d := mustReserve(t, q, "critical", "mail")
			if d.Envelope.ID != want {
				t.Fatalf("reserved %s, want %s", d.Envelope.ID, want)
			}
			if err := q.Ack(ctx, d); err != nil {
				t.Fatal(err)
			}
		}
		expectEmpty(t, q, "critical", "mail")
	})

	t.Run("the envelope round-trips and attempts count deliveries", func(t *testing.T) {
		clock := newFakeClock()
		q := newQueue(t, clock)
		sent := envelope("e1")
		sent.Attempt = 7 // a producer's value is not the delivery count
		if err := q.Push(ctx, "default", sent, time.Time{}); err != nil {
			t.Fatal(err)
		}
		d := mustReserve(t, q, "default")
		got := d.Envelope
		if d.Queue != "default" || d.Receipt == "" || got.Attempt != 1 {
			t.Fatalf("delivery = %+v, want default queue, a receipt, attempt 1", d)
		}
		if got.ID != sent.ID || got.Name != sent.Name || got.Version != sent.Version || string(got.Payload) != string(sent.Payload) ||
			got.Metadata["gombit.request_id"] != "req-e1" || !got.EnqueuedAt.Equal(sent.EnqueuedAt) {
			t.Fatalf("envelope = %+v, want %+v", got, sent)
		}

		if err := q.Release(ctx, d, time.Time{}); err != nil {
			t.Fatalf("Release() error = %v", err)
		}
		d = mustReserve(t, q, "default")
		if d.Envelope.Attempt != 2 {
			t.Fatalf("attempt after a release = %d, want 2", d.Envelope.Attempt)
		}
		if err := q.Ack(ctx, d); err != nil {
			t.Fatal(err)
		}
		expectEmpty(t, q, "default")
	})

	t.Run("delayed jobs wait for their time", func(t *testing.T) {
		clock := newFakeClock()
		q := newQueue(t, clock)
		if err := q.Push(ctx, "default", envelope("later"), clock.Now().Add(10*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := q.Push(ctx, "default", envelope("past"), clock.Now().Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		d := mustReserve(t, q, "default")
		if d.Envelope.ID != "past" {
			t.Fatalf("reserved %s first, want the job available now", d.Envelope.ID)
		}
		if err := q.Ack(ctx, d); err != nil {
			t.Fatal(err)
		}
		expectEmpty(t, q, "default")
		clock.Advance(10*time.Minute - time.Second)
		expectEmpty(t, q, "default")
		clock.Advance(time.Second)
		if d := mustReserve(t, q, "default"); d.Envelope.ID != "later" {
			t.Fatalf("reserved %s, want the delayed job at its time", d.Envelope.ID)
		}
	})

	t.Run("a job due earlier goes before one pushed later", func(t *testing.T) {
		clock := newFakeClock()
		q := newQueue(t, clock)
		if err := q.Push(ctx, "default", envelope("retry"), clock.Now().Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		clock.Advance(5 * time.Minute)
		if err := q.Push(ctx, "default", envelope("fresh"), time.Time{}); err != nil {
			t.Fatal(err)
		}
		if d := mustReserve(t, q, "default"); d.Envelope.ID != "retry" {
			t.Fatalf("reserved %s, want the job available since a minute in", d.Envelope.ID)
		}
	})

	t.Run("a job already waiting goes before one that became due after it", func(t *testing.T) {
		clock := newFakeClock()
		q := newQueue(t, clock)
		if err := q.Push(ctx, "default", envelope("waiting"), time.Time{}); err != nil {
			t.Fatal(err)
		}
		if err := q.Push(ctx, "default", envelope("later"), clock.Now().Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		clock.Advance(2 * time.Minute)
		for _, want := range []string{"waiting", "later"} {
			d := mustReserve(t, q, "default")
			if d.Envelope.ID != want {
				t.Fatalf("reserved %s, want %s (available first)", d.Envelope.ID, want)
			}
			if err := q.Ack(ctx, d); err != nil {
				t.Fatal(err)
			}
		}
	})

	t.Run("an expired lease is ready since its deadline, not before", func(t *testing.T) {
		clock := newFakeClock()
		q := newQueue(t, clock)
		if err := q.Push(ctx, "default", envelope("crashed"), time.Time{}); err != nil {
			t.Fatal(err)
		}
		if _, err := q.Reserve(ctx, []string{"default"}, time.Minute); err != nil {
			t.Fatal(err)
		}
		clock.Advance(30 * time.Second)
		if err := q.Push(ctx, "default", envelope("queued"), time.Time{}); err != nil { // ready at +30s
			t.Fatal(err)
		}
		if err := q.Push(ctx, "default", envelope("after"), clock.Now().Add(time.Minute)); err != nil { // ready at +90s
			t.Fatal(err)
		}
		clock.Advance(2 * time.Minute) // the lease expired at +60s
		for _, want := range []string{"queued", "crashed", "after"} {
			d := mustReserve(t, q, "default")
			if d.Envelope.ID != want {
				t.Fatalf("reserved %s, want %s", d.Envelope.ID, want)
			}
			if err := q.Ack(ctx, d); err != nil {
				t.Fatal(err)
			}
		}
	})

	t.Run("delayed jobs come out in the order they became due", func(t *testing.T) {
		clock := newFakeClock()
		q := newQueue(t, clock)
		for i, id := range []string{"d3", "d1", "d2"} {
			offsets := []time.Duration{3 * time.Second, time.Second, 2 * time.Second}
			if err := q.Push(ctx, "default", envelope(id), clock.Now().Add(offsets[i])); err != nil {
				t.Fatal(err)
			}
		}
		clock.Advance(time.Minute)
		for _, want := range []string{"d1", "d2", "d3"} {
			d := mustReserve(t, q, "default")
			if d.Envelope.ID != want {
				t.Fatalf("reserved %s, want %s", d.Envelope.ID, want)
			}
			if err := q.Ack(ctx, d); err != nil {
				t.Fatal(err)
			}
		}
	})

	t.Run("an ID is unique per queue", func(t *testing.T) {
		q := newQueue(t, newFakeClock())
		for _, queue := range []string{"mail", "critical"} {
			if err := q.Push(ctx, queue, envelope("same"), time.Time{}); err != nil {
				t.Fatalf("Push(same ID, %s) error = %v", queue, err)
			}
		}
		if err := q.Push(ctx, "mail", envelope("same"), time.Time{}); !errors.Is(err, jobs.ErrDuplicateJob) {
			t.Fatalf("Push(same ID, same queue) error = %v, want ErrDuplicateJob", err)
		}
		a, b := mustReserve(t, q, "mail"), mustReserve(t, q, "critical")
		if err := q.Ack(ctx, a); err != nil {
			t.Fatal(err)
		}
		if err := q.Ack(ctx, b); err != nil {
			t.Fatalf("Ack on the other queue after the first was acked: %v", err)
		}
		expectEmpty(t, q, "mail", "critical")
	})

	t.Run("close is final", func(t *testing.T) {
		q := newQueue(t, newFakeClock())
		if err := q.Push(ctx, "default", envelope("x"), time.Time{}); err != nil {
			t.Fatal(err)
		}
		d := mustReserve(t, q, "default")
		if err := q.Close(); err != nil {
			t.Fatal(err)
		}
		if err := q.Push(ctx, "default", envelope("y"), time.Time{}); !errors.Is(err, jobs.ErrClosed) {
			t.Fatalf("Push after Close = %v, want ErrClosed", err)
		}
		if _, err := q.Reserve(ctx, []string{"default"}, time.Minute); !errors.Is(err, jobs.ErrClosed) {
			t.Fatalf("Reserve after Close = %v, want ErrClosed", err)
		}
		if err := q.Ack(ctx, d); !errors.Is(err, jobs.ErrClosed) {
			t.Fatalf("Ack after Close = %v, want ErrClosed", err)
		}
		if err := q.Release(ctx, d, time.Time{}); !errors.Is(err, jobs.ErrClosed) {
			t.Fatalf("Release after Close = %v, want ErrClosed", err)
		}
	})

	t.Run("an expired lease redelivers, and the stale holder loses it", func(t *testing.T) {
		clock := newFakeClock()
		q := newQueue(t, clock)
		if err := q.Push(ctx, "default", envelope("l1"), time.Time{}); err != nil {
			t.Fatal(err)
		}
		first, err := q.Reserve(ctx, []string{"default"}, 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		clock.Advance(29 * time.Second)
		expectEmpty(t, q, "default")
		clock.Advance(time.Second) // the first worker died

		second := mustReserve(t, q, "default")
		if second.Envelope.ID != "l1" || second.Envelope.Attempt != 2 || second.Receipt == first.Receipt {
			t.Fatalf("redelivery = %+v, want l1 at attempt 2 under a new receipt", second)
		}
		if err := q.Ack(ctx, first); !errors.Is(err, jobs.ErrLeaseLost) {
			t.Fatalf("Ack(stale) error = %v, want ErrLeaseLost", err)
		}
		if err := q.Release(ctx, first, time.Time{}); !errors.Is(err, jobs.ErrLeaseLost) {
			t.Fatalf("Release(stale) error = %v, want ErrLeaseLost", err)
		}
		if err := q.Ack(ctx, second); err != nil {
			t.Fatalf("Ack(current) error = %v", err)
		}
		if err := q.Ack(ctx, second); !errors.Is(err, jobs.ErrLeaseLost) {
			t.Fatalf("Ack twice error = %v, want ErrLeaseLost", err)
		}
		expectEmpty(t, q, "default")
	})

	t.Run("a late ack still completes a job nobody took over", func(t *testing.T) {
		clock := newFakeClock()
		q := newQueue(t, clock)
		if err := q.Push(ctx, "default", envelope("slow"), time.Time{}); err != nil {
			t.Fatal(err)
		}
		d, err := q.Reserve(ctx, []string{"default"}, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		clock.Advance(time.Minute)
		if err := q.Ack(ctx, d); err != nil {
			t.Fatalf("Ack after the lease expired, before a redelivery: %v", err)
		}
		expectEmpty(t, q, "default")
	})

	t.Run("extend keeps a long job from a second worker", func(t *testing.T) {
		clock := newFakeClock()
		q := newQueue(t, clock)
		if err := q.Push(ctx, "default", envelope("long"), time.Time{}); err != nil {
			t.Fatal(err)
		}
		d, err := q.Reserve(ctx, []string{"default"}, 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 4; i++ { // two minutes of work, renewing every 20s
			clock.Advance(20 * time.Second)
			if err := q.Extend(ctx, d, 30*time.Second); err != nil {
				t.Fatalf("Extend() #%d error = %v", i, err)
			}
			expectEmpty(t, q, "default")
		}
		if err := q.Ack(ctx, d); err != nil {
			t.Fatalf("Ack after extending = %v", err)
		}

		// A lease that ran out and was taken over cannot be extended.
		if err := q.Push(ctx, "default", envelope("lost"), time.Time{}); err != nil {
			t.Fatal(err)
		}
		stale, err := q.Reserve(ctx, []string{"default"}, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		clock.Advance(2 * time.Second)
		mustReserve(t, q, "default")
		if err := q.Extend(ctx, stale, time.Minute); !errors.Is(err, jobs.ErrLeaseLost) {
			t.Fatalf("Extend(stale) error = %v, want ErrLeaseLost", err)
		}
		if err := q.Extend(ctx, stale, 0); err == nil {
			t.Fatal("Extend accepted a zero lease")
		}
	})

	t.Run("extend reclaims a lapsed lease nobody took", func(t *testing.T) {
		clock := newFakeClock()
		q := newQueue(t, clock)
		if err := q.Push(ctx, "default", envelope("lapsed"), time.Time{}); err != nil {
			t.Fatal(err)
		}
		if err := q.Push(ctx, "other", envelope("x"), time.Time{}); err != nil {
			t.Fatal(err)
		}
		d, err := q.Reserve(ctx, []string{"default"}, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		clock.Advance(2 * time.Second)
		// Another queue's reserve does not touch this one; a reserve on
		// "default" would take the lapsed job over.
		mustReserve(t, q, "other")
		if err := q.Extend(ctx, d, time.Minute); err != nil {
			t.Fatalf("Extend(lapsed, untaken) error = %v", err)
		}
		expectEmpty(t, q, "default")
		if err := q.Ack(ctx, d); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("release can delay the retry", func(t *testing.T) {
		clock := newFakeClock()
		q := newQueue(t, clock)
		if err := q.Push(ctx, "default", envelope("r1"), time.Time{}); err != nil {
			t.Fatal(err)
		}
		d := mustReserve(t, q, "default")
		if err := q.Release(ctx, d, clock.Now().Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		expectEmpty(t, q, "default")
		clock.Advance(time.Minute)
		if again := mustReserve(t, q, "default"); again.Envelope.ID != "r1" || again.Envelope.Attempt != 2 {
			t.Fatalf("retry = %+v, want r1 at attempt 2", again)
		}
	})

	t.Run("pushes are validated", func(t *testing.T) {
		q := newQueue(t, newFakeClock())
		if err := q.Push(ctx, "default", envelope("dup"), time.Time{}); err != nil {
			t.Fatal(err)
		}
		if err := q.Push(ctx, "default", envelope("dup"), time.Time{}); !errors.Is(err, jobs.ErrDuplicateJob) {
			t.Fatalf("Push(duplicate ID) error = %v, want ErrDuplicateJob", err)
		}
		if err := q.Push(ctx, "Bad Queue", envelope("x"), time.Time{}); !errors.Is(err, jobs.ErrInvalidQueue) {
			t.Fatalf("Push(invalid queue) error = %v, want ErrInvalidQueue", err)
		}
		if err := q.Push(ctx, "default", jobs.Envelope{Name: "send_welcome_email"}, time.Time{}); err == nil {
			t.Fatal("Push accepted an envelope with no ID")
		}
		if _, err := q.Reserve(ctx, []string{"default"}, 0); err == nil {
			t.Fatal("Reserve accepted a zero lease")
		}
		if _, err := q.Reserve(ctx, []string{"default", "Bad Queue"}, time.Minute); !errors.Is(err, jobs.ErrInvalidQueue) {
			t.Fatalf("Reserve(invalid queue) error = %v, want ErrInvalidQueue", err)
		}
		if err := q.Ack(ctx, jobs.Delivery{Queue: "default", Envelope: envelope("dup")}); !errors.Is(err, jobs.ErrLeaseLost) {
			t.Fatalf("Ack(no receipt) error = %v, want ErrLeaseLost", err)
		}
	})

	t.Run("failed jobs are kept, inspectable, retryable, and forgettable", func(t *testing.T) {
		clock := newFakeClock()
		q := newQueue(t, clock)
		for _, id := range []string{"f1", "f2", "f3"} {
			if err := q.Push(ctx, "default", envelope(id), time.Time{}); err != nil {
				t.Fatal(err)
			}
		}
		bury := func(reason string) jobs.Delivery {
			t.Helper()
			d := mustReserve(t, q, "default")
			if err := q.Bury(ctx, d, jobs.Failure{Reason: reason, Kind: jobs.KindHandler, Error: "smtp down: " + d.Envelope.ID, At: clock.Now()}); err != nil {
				t.Fatalf("Bury(%s) error = %v", d.Envelope.ID, err)
			}
			clock.Advance(time.Minute)
			return d
		}
		first := bury(jobs.ReasonExhausted)
		second := bury(jobs.ReasonPermanent)
		expectEmpty(t, q, "other")
		if err := q.Bury(ctx, first, jobs.Failure{}); !errors.Is(err, jobs.ErrLeaseLost) {
			t.Fatalf("Bury twice = %v, want ErrLeaseLost", err)
		}

		failed, err := q.Failed(ctx, "default", 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(failed) != 2 || failed[0].Envelope.ID != second.Envelope.ID || failed[1].Envelope.ID != first.Envelope.ID {
			t.Fatalf("Failed() = %+v, want the two buried jobs, most recent first", failed)
		}
		if limited, _ := q.Failed(ctx, "default", 1); len(limited) != 1 {
			t.Fatalf("Failed(limit 1) = %d jobs", len(limited))
		}
		got, err := q.FailedJob(ctx, "default", first.Envelope.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Queue != "default" || got.Attempts != 1 || string(got.Envelope.Payload) != `{"user_id":1}` ||
			got.Envelope.Metadata["gombit.request_id"] != "req-"+first.Envelope.ID || got.Failure.Reason != jobs.ReasonExhausted ||
			got.Failure.Error != "smtp down: "+first.Envelope.ID || !got.Failure.At.Equal(fixedNow) {
			t.Fatalf("FailedJob() = %+v, want the original envelope and the failure", got)
		}

		// The third job is still queued, and a failed job is not reservable.
		third := mustReserve(t, q, "default")
		expectEmpty(t, q, "default")
		if err := q.Ack(ctx, third); err != nil {
			t.Fatal(err)
		}

		if err := q.RetryFailed(ctx, "default", first.Envelope.ID); err != nil {
			t.Fatalf("RetryFailed() error = %v", err)
		}
		again := mustReserve(t, q, "default")
		if again.Envelope.ID != first.Envelope.ID || again.Envelope.Attempt != 1 {
			t.Fatalf("after RetryFailed reserved %+v, want it back with fresh attempts", again.Envelope)
		}
		if err := q.Ack(ctx, again); err != nil {
			t.Fatal(err)
		}
		if _, err := q.FailedJob(ctx, "default", first.Envelope.ID); !errors.Is(err, jobs.ErrNotFailed) {
			t.Fatalf("FailedJob(retried and done) = %v, want ErrNotFailed", err)
		}

		if err := q.ForgetFailed(ctx, "default", second.Envelope.ID); err != nil {
			t.Fatalf("ForgetFailed() error = %v", err)
		}
		for _, err := range []error{
			q.ForgetFailed(ctx, "default", second.Envelope.ID),
			q.RetryFailed(ctx, "default", "nope"),
		} {
			if !errors.Is(err, jobs.ErrNotFailed) {
				t.Fatalf("on a job that is not failed: %v, want ErrNotFailed", err)
			}
		}
		// A queued (not failed) job cannot be retried or forgotten as failed.
		if err := q.Push(ctx, "default", envelope("live"), time.Time{}); err != nil {
			t.Fatal(err)
		}
		if err := q.ForgetFailed(ctx, "default", "live"); !errors.Is(err, jobs.ErrNotFailed) {
			t.Fatalf("ForgetFailed(queued job) = %v, want ErrNotFailed", err)
		}
		if left, _ := q.Failed(ctx, "default", 0); len(left) != 0 {
			t.Fatalf("failed jobs left: %+v", left)
		}
		if _, err := q.Failed(ctx, "Bad Queue", 0); !errors.Is(err, jobs.ErrInvalidQueue) {
			t.Fatalf("Failed(invalid queue) = %v", err)
		}
	})

	t.Run("purge deletes failed jobs, optionally only older ones", func(t *testing.T) {
		clock := newFakeClock()
		q := newQueue(t, clock)
		for i, id := range []string{"old1", "old2", "new1"} {
			if err := q.Push(ctx, "default", envelope(id), time.Time{}); err != nil {
				t.Fatal(err)
			}
			d := mustReserve(t, q, "default")
			if err := q.Bury(ctx, d, jobs.Failure{Reason: jobs.ReasonExhausted, At: clock.Now()}); err != nil {
				t.Fatal(err)
			}
			if i == 1 {
				clock.Advance(24 * time.Hour)
			}
		}
		n, err := q.PurgeFailed(ctx, "default", clock.Now().Add(-time.Hour))
		if err != nil || n != 2 {
			t.Fatalf("PurgeFailed(older than an hour) = %d, %v; want 2", n, err)
		}
		if left, _ := q.Failed(ctx, "default", 0); len(left) != 1 || left[0].Envelope.ID != "new1" {
			t.Fatalf("after the purge: %+v, want new1 left", left)
		}
		if n, err := q.PurgeFailed(ctx, "default", time.Time{}); err != nil || n != 1 {
			t.Fatalf("PurgeFailed(all) = %d, %v; want 1", n, err)
		}
		// The IDs are free again.
		if err := q.Push(ctx, "default", envelope("old1"), time.Time{}); err != nil {
			t.Fatalf("Push after purge: %v", err)
		}
	})

	t.Run("a unique key admits one job until it is done", func(t *testing.T) {
		q := newQueue(t, newFakeClock())
		key := jobs.UniqueKey{Key: "report:42", TTL: time.Hour, UntilDone: true}
		if err := q.PushUnique(ctx, "default", envelope("u1"), time.Time{}, key); err != nil {
			t.Fatal(err)
		}
		err := q.PushUnique(ctx, "default", envelope("u2"), time.Time{}, key)
		var dup *jobs.DuplicateError
		if !errors.As(err, &dup) || !errors.Is(err, jobs.ErrDuplicateDispatch) || dup.HolderID != "u1" || dup.Key != "report:42" {
			t.Fatalf("second PushUnique = %v, want a DuplicateError naming u1", err)
		}
		if err := q.PushUnique(ctx, "other", envelope("u3"), time.Time{}, key); err != nil {
			t.Fatalf("the same key on another queue = %v, want it free", err)
		}
		d := mustReserve(t, q, "default")
		if err := q.PushUnique(ctx, "default", envelope("u2"), time.Time{}, key); !errors.Is(err, jobs.ErrDuplicateDispatch) {
			t.Fatalf("while running = %v, want the key still held", err)
		}
		if err := q.Ack(ctx, d); err != nil {
			t.Fatal(err)
		}
		if err := q.PushUnique(ctx, "default", envelope("u2"), time.Time{}, key); err != nil {
			t.Fatalf("after the holder was acked = %v, want the key free", err)
		}
		// Giving up on the holder frees the key too.
		d = mustReserve(t, q, "default")
		if err := q.Bury(ctx, d, jobs.Failure{Reason: jobs.ReasonPermanent}); err != nil {
			t.Fatal(err)
		}
		if err := q.PushUnique(ctx, "default", envelope("u4"), time.Time{}, key); err != nil {
			t.Fatalf("after the holder was buried = %v, want the key free", err)
		}
		// A key held for a window outlives its job.
		window := jobs.UniqueKey{Key: "digest", TTL: time.Hour}
		if err := q.PushUnique(ctx, "digest", envelope("w1"), time.Time{}, window); err != nil {
			t.Fatal(err)
		}
		if err := q.Ack(ctx, mustReserve(t, q, "digest")); err != nil {
			t.Fatal(err)
		}
		if err := q.PushUnique(ctx, "digest", envelope("w2"), time.Time{}, window); !errors.Is(err, jobs.ErrDuplicateDispatch) {
			t.Fatalf("inside the window after the job ran = %v, want a duplicate", err)
		}
		if err := q.PushUnique(ctx, "default", envelope("v"), time.Time{}, jobs.UniqueKey{Key: "k"}); err == nil {
			t.Fatal("PushUnique accepted a zero TTL")
		}
	})

	t.Run("retrying a failed unique job reclaims its key", func(t *testing.T) {
		q := newQueue(t, newFakeClock())
		key := jobs.UniqueKey{Key: "rebuild:7", TTL: time.Hour, UntilDone: true}
		if err := q.PushUnique(ctx, "default", envelope("r1"), time.Time{}, key); err != nil {
			t.Fatal(err)
		}
		if err := q.Bury(ctx, mustReserve(t, q, "default"), jobs.Failure{Reason: jobs.ReasonExhausted}); err != nil {
			t.Fatal(err)
		}
		// The key was released; another job took it.
		if err := q.PushUnique(ctx, "default", envelope("r2"), time.Time{}, key); err != nil {
			t.Fatal(err)
		}
		err := q.RetryFailed(ctx, "default", "r1")
		var dup *jobs.DuplicateError
		if !errors.As(err, &dup) || dup.HolderID != "r2" || dup.Key != "rebuild:7" {
			t.Fatalf("RetryFailed while r2 holds the key = %v, want a DuplicateError naming r2", err)
		}
		if err := q.Ack(ctx, mustReserve(t, q, "default")); err != nil { // r2 done
			t.Fatal(err)
		}
		if err := q.RetryFailed(ctx, "default", "r1"); err != nil {
			t.Fatalf("RetryFailed after r2 finished = %v", err)
		}
		if err := q.PushUnique(ctx, "default", envelope("r3"), time.Time{}, key); !errors.Is(err, jobs.ErrDuplicateDispatch) {
			t.Fatalf("a dispatch while the retried r1 is queued = %v, want the key held again", err)
		}
	})

	t.Run("the once store remembers completed effects", func(t *testing.T) {
		store, ok := newQueue(t, newFakeClock()).(jobs.OnceStore)
		if !ok {
			t.Fatal("the queue is not a OnceStore")
		}
		state, err := store.BeginOnce(ctx, "email:1", "t1", time.Minute)
		if err != nil || state != jobs.OnceAcquired {
			t.Fatalf("BeginOnce(fresh) = %v, %v", state, err)
		}
		if state, _ := store.BeginOnce(ctx, "email:1", "t2", time.Minute); state != jobs.OnceBusy {
			t.Fatalf("BeginOnce(locked) = %v, want busy", state)
		}
		if err := store.FinishOnce(ctx, "email:1", "t2", time.Hour); !errors.Is(err, jobs.ErrLeaseLost) {
			t.Fatalf("FinishOnce by a non-holder = %v", err)
		}
		if err := store.FinishOnce(ctx, "email:1", "t1", time.Hour); err != nil {
			t.Fatal(err)
		}
		if state, _ := store.BeginOnce(ctx, "email:1", "t3", time.Minute); state != jobs.OnceDone {
			t.Fatalf("BeginOnce(done) = %v, want done", state)
		}
		// An abandoned lock frees the key; another run's abandon does not.
		if state, _ := store.BeginOnce(ctx, "email:2", "t4", time.Minute); state != jobs.OnceAcquired {
			t.Fatal("email:2 not acquired")
		}
		if err := store.AbandonOnce(ctx, "email:2", "someone-else"); err != nil {
			t.Fatal(err)
		}
		if state, _ := store.BeginOnce(ctx, "email:2", "t5", time.Minute); state != jobs.OnceBusy {
			t.Fatalf("after a foreign abandon = %v, want still busy", state)
		}
		if err := store.AbandonOnce(ctx, "email:2", "t4"); err != nil {
			t.Fatal(err)
		}
		if state, _ := store.BeginOnce(ctx, "email:2", "t6", time.Minute); state != jobs.OnceAcquired {
			t.Fatalf("after the holder abandoned = %v, want acquired", state)
		}
	})

	t.Run("concurrent workers never share a job", func(t *testing.T) {
		q := newQueue(t, newFakeClock())
		const n = 60
		for i := 0; i < n; i++ {
			if err := q.Push(ctx, "default", envelope("c"+string(rune('A'+i))), time.Time{}); err != nil {
				t.Fatal(err)
			}
		}
		var mu sync.Mutex
		seen := map[string]int{}
		var wg sync.WaitGroup
		for w := 0; w < 6; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					d, err := q.Reserve(ctx, []string{"default"}, time.Minute)
					if errors.Is(err, jobs.ErrNoJob) {
						return
					}
					if err != nil {
						t.Error(err)
						return
					}
					mu.Lock()
					seen[d.Envelope.ID]++
					mu.Unlock()
					if err := q.Ack(ctx, d); err != nil {
						t.Error(err)
						return
					}
				}
			}()
		}
		wg.Wait()
		if len(seen) != n {
			t.Fatalf("delivered %d distinct jobs, want %d", len(seen), n)
		}
		for id, count := range seen {
			if count != 1 {
				t.Fatalf("job %s delivered %d times", id, count)
			}
		}
	})
}

func TestMemoryQueueConformance(t *testing.T) {
	runQueueConformance(t, func(t *testing.T, clock *fakeClock) jobs.Queue {
		q := jobs.NewMemoryQueue(jobs.WithMemoryClock(clock.Now))
		t.Cleanup(func() { _ = q.Close() })
		return q
	})
}
