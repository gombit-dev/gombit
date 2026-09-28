package jobs_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/jobs"
)

// TestDelayedDispatch drives delays with one injected clock for the
// dispatcher and the queue: nothing becomes available until its time.
func TestDelayedDispatch(t *testing.T) {
	clock := newFakeClock()
	reg := newRegistry()
	jobs.MustRegister(reg, func(context.Context, sendWelcome) error { return nil })
	q := jobs.NewMemoryQueue(jobs.WithMemoryClock(clock.Now))
	d := jobs.NewDispatcher(reg, q, jobs.WithDispatcherClock(clock.Now))
	ctx := context.Background()

	later, err := d.Dispatch(ctx, sendWelcome{UserID: 1}, jobs.Delay(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	at, err := d.DispatchAt(ctx, sendWelcome{UserID: 2}, clock.Now().Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	past, err := d.Dispatch(ctx, sendWelcome{UserID: 3}, jobs.At(clock.Now().Add(-time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	takeOne := func(want jobs.Envelope, when string) {
		t.Helper()
		got := mustReserve(t, q, "default")
		if got.Envelope.ID != want.ID {
			t.Fatalf("%s reserved %s, want %s", when, got.Envelope.ID, want.ID)
		}
		if err := q.Ack(ctx, got); err != nil {
			t.Fatal(err)
		}
	}
	takeOne(past, "first")
	expectEmpty(t, q, "default")
	clock.Advance(5 * time.Minute)
	takeOne(at, "at +5m (DispatchAt)")
	expectEmpty(t, q, "default")
	clock.Advance(5 * time.Minute)
	takeOne(later, "at +10m (Delay)")

	// The last option wins between Delay and At.
	clock.Advance(time.Hour)
	both, err := d.Dispatch(ctx, sendWelcome{UserID: 4}, jobs.At(clock.Now().Add(time.Hour)), jobs.Delay(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	takeOne(both, "Delay after At, a minute on")

	if _, err := d.Dispatch(ctx, sendWelcome{}, jobs.Delay(-time.Second)); err == nil {
		t.Fatal("Dispatch accepted a negative delay")
	}
}

func TestSyncDispatchRunsDelayedJobsAtOnce(t *testing.T) {
	reg := newRegistry()
	ran := false
	jobs.MustRegister(reg, func(context.Context, sendWelcome) error { ran = true; return nil })
	if _, err := jobs.NewDispatcher(reg, nil).Dispatch(context.Background(), sendWelcome{}, jobs.Delay(time.Hour)); err != nil || !ran {
		t.Fatalf("sync Dispatch(Delay) = %v, ran %v; want it run at once", err, ran)
	}
	if _, err := jobs.NewDispatcher(reg, nil).Dispatch(context.Background(), sendWelcome{}, jobs.Delay(-time.Second)); err == nil {
		t.Fatal("sync Dispatch accepted a negative delay")
	}
}

// TestRedisDelayedJobsSurviveRestarts: a scheduled job lives in Redis, so
// a new process finds it at its time, not before.
func TestRedisDelayedJobsSurviveRestarts(t *testing.T) {
	addr := redisTestAddr(t)
	ns := testNamespace()
	clock := newFakeClock()
	reg := newRegistry()
	jobs.MustRegister(reg, func(context.Context, sendWelcome) error { return nil })

	before := jobs.NewRedisQueue(redisClient(t, addr, ns), ns, jobs.WithRedisClock(clock.Now))
	env, err := jobs.NewDispatcher(reg, before, jobs.WithDispatcherClock(clock.Now)).Dispatch(context.Background(), sendWelcome{UserID: 9}, jobs.Delay(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	_ = before.Close()

	after := jobs.NewRedisQueue(redisClient(t, addr, ns), ns, jobs.WithRedisClock(clock.Now))
	clock.Advance(59 * time.Minute)
	if _, err := after.Reserve(context.Background(), []string{"default"}, time.Minute); !errors.Is(err, jobs.ErrNoJob) {
		t.Fatalf("a minute early after a restart: %v, want ErrNoJob", err)
	}
	clock.Advance(time.Minute)
	if got := mustReserve(t, after, "default"); got.Envelope.ID != env.ID {
		t.Fatalf("at its time after a restart reserved %s, want %s", got.Envelope.ID, env.ID)
	}
}

// TestWorkerRunsADelayedJobAtItsTime: the worker leaves a delayed job alone
// until its time, then runs it.
func TestWorkerRunsADelayedJobAtItsTime(t *testing.T) {
	reg := jobs.NewRegistry()
	ranAt := make(chan time.Time, 1)
	jobs.MustRegister(reg, func(context.Context, sendWelcome) error { ranAt <- time.Now(); return nil })
	q := jobs.NewMemoryQueue()
	w, err := jobs.NewWorker(reg, q, jobs.WorkerOptions{Queues: []string{"default"}, PollInterval: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	startWorker(t, w)
	const delay = 150 * time.Millisecond
	dispatched := time.Now()
	if _, err := jobs.NewDispatcher(reg, q).Dispatch(context.Background(), sendWelcome{UserID: 1}, jobs.Delay(delay)); err != nil {
		t.Fatal(err)
	}
	select {
	case at := <-ranAt:
		if early := delay - at.Sub(dispatched); early > 0 {
			t.Fatalf("the worker ran the job %s before its time", early)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the delayed job never ran")
	}
}
