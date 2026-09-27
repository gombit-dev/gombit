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
