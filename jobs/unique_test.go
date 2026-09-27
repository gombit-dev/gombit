package jobs_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/gombit-dev/gombit/jobs"
)

func TestUniqueDispatch(t *testing.T) {
	reg := newRegistry()
	jobs.MustRegister(reg, func(context.Context, sendWelcome) error { return nil })
	q := jobs.NewMemoryQueue()
	d := jobs.NewDispatcher(reg, q)
	ctx := context.Background()
	first, err := d.Dispatch(ctx, sendWelcome{UserID: 1}, jobs.Unique("welcome:1", time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.Dispatch(ctx, sendWelcome{UserID: 1}, jobs.Unique("welcome:1", time.Hour))
	var dup *jobs.DuplicateError
	if !errors.As(err, &dup) || dup.HolderID != first.ID {
		t.Fatalf("second Dispatch = %v, want a DuplicateError naming %s", err, first.ID)
	}
	if q.Len() != 1 {
		t.Fatalf("queue holds %d jobs, want 1", q.Len())
	}
	if _, err := d.Dispatch(ctx, sendWelcome{}, jobs.Unique("", time.Hour)); err == nil {
		t.Fatal("Dispatch accepted an empty uniqueness key")
	}
	if _, err := d.Dispatch(ctx, sendWelcome{}, jobs.UniqueFor("k", 0)); err == nil {
		t.Fatal("Dispatch accepted a zero window")
	}
	// The sync driver has nothing to hold a key: it runs every dispatch.
	runs := 0
	syncReg := newRegistry()
	jobs.MustRegister(syncReg, func(context.Context, sendWelcome) error { runs++; return nil })
	sync := jobs.NewDispatcher(syncReg, nil)
	for i := 0; i < 2; i++ {
		if _, err := sync.Dispatch(ctx, sendWelcome{}, jobs.Unique("k", time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if runs != 2 {
		t.Fatalf("sync ran %d times, want 2", runs)
	}
}

// TestUniqueKeysExpireOnTheMemoryClock: a claim whose job vanished (here:
// never acked) frees after its TTL, and a window ends when it says.
func TestUniqueKeysExpireOnTheMemoryClock(t *testing.T) {
	clock := newFakeClock()
	q := jobs.NewMemoryQueue(jobs.WithMemoryClock(clock.Now))
	ctx := context.Background()
	if err := q.PushUnique(ctx, "default", envelope("a"), time.Time{}, jobs.UniqueKey{Key: "k", TTL: time.Minute, UntilDone: true}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	if err := q.PushUnique(ctx, "default", envelope("b"), time.Time{}, jobs.UniqueKey{Key: "k", TTL: time.Minute, UntilDone: true}); err != nil {
		t.Fatalf("after the TTL = %v, want the key free", err)
	}
	store := jobs.OnceStore(q)
	if _, err := store.BeginOnce(ctx, "e", "t1", time.Minute); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute) // the run holding the lock died
	if state, _ := store.BeginOnce(ctx, "e", "t2", time.Minute); state != jobs.OnceAcquired {
		t.Fatalf("after the lock's TTL = %v, want acquired", state)
	}
}

func TestRedisUniqueKeysExpire(t *testing.T) {
	addr := redisTestAddr(t)
	ns := testNamespace()
	ctx := context.Background()
	q := jobs.NewRedisQueue(redisClient(t, addr, ns), ns)
	key := jobs.UniqueKey{Key: "k", TTL: 150 * time.Millisecond, UntilDone: true}
	if err := q.PushUnique(ctx, "default", envelope("a"), time.Time{}, key); err != nil {
		t.Fatal(err)
	}
	if err := q.PushUnique(ctx, "default", envelope("b"), time.Time{}, key); !errors.Is(err, jobs.ErrDuplicateDispatch) {
		t.Fatalf("inside the TTL = %v", err)
	}
	time.Sleep(250 * time.Millisecond)
	if err := q.PushUnique(ctx, "default", envelope("b"), time.Time{}, key); err != nil {
		t.Fatalf("after the TTL = %v, want the key free", err)
	}
	if _, err := q.BeginOnce(ctx, "e", "t1", 150*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	if state, _ := q.BeginOnce(ctx, "e", "t2", time.Minute); state != jobs.OnceAcquired {
		t.Fatalf("after the lock's TTL = %v, want acquired", state)
	}
}

// TestOnceSkipsARepeatedEffect: a job that fails after its side effect is
// retried, and the retry does not repeat the effect.
func TestOnceSkipsARepeatedEffect(t *testing.T) {
	reg := jobs.NewRegistry()
	var effects, runs atomic.Int32
	jobs.MustRegister(reg, func(ctx context.Context, _ sendWelcome) error {
		info, _ := jobs.InfoFromContext(ctx)
		runs.Add(1)
		if err := jobs.Once(ctx, "welcome-email:"+info.ID, func(context.Context) error {
			effects.Add(1)
			return nil
		}); err != nil {
			return err
		}
		if info.Attempt == 1 {
			return errors.New("crashed after sending")
		}
		return nil
	}, jobs.WithOptions(jobs.Options{Backoff: jobs.Constant(5 * time.Millisecond)}))
	q := jobs.NewMemoryQueue()
	if _, err := jobs.NewDispatcher(reg, q).Dispatch(context.Background(), sendWelcome{UserID: 1}); err != nil {
		t.Fatal(err)
	}
	w, err := jobs.NewWorker(reg, q, jobs.WorkerOptions{Queues: []string{"default"}, PollInterval: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	startWorker(t, w)
	eventually(t, "the job to succeed", func() bool { return q.Len() == 0 })
	if runs.Load() != 2 || effects.Load() != 1 {
		t.Fatalf("ran %d times with %d effects, want 2 runs and 1 effect", runs.Load(), effects.Load())
	}
}

// TestOnceWhileAnotherRunHoldsTheLock: a concurrent duplicate is a
// retryable failure; a failing effect releases the lock for the retry.
func TestOnceWhileAnotherRunHoldsTheLock(t *testing.T) {
	q := jobs.NewMemoryQueue()
	reg := jobs.NewRegistry()
	var effects atomic.Int32
	failFirst := atomic.Bool{}
	failFirst.Store(true)
	jobs.MustRegister(reg, func(ctx context.Context, _ sendWelcome) error {
		return jobs.Once(ctx, "shared", func(context.Context) error {
			if failFirst.CompareAndSwap(true, false) {
				return errors.New("smtp down")
			}
			effects.Add(1)
			return nil
		})
	}, jobs.WithOptions(jobs.Options{MaxAttempts: 20, Backoff: jobs.Constant(20 * time.Millisecond)}))
	// Another run holds the lock on "shared" right now.
	if _, err := jobs.OnceStore(q).BeginOnce(context.Background(), "shared", "other-run", 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.NewDispatcher(reg, q).Dispatch(context.Background(), sendWelcome{}); err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zap.InfoLevel)
	w, err := jobs.NewWorker(reg, q, jobs.WorkerOptions{Queues: []string{"default"}, PollInterval: 5 * time.Millisecond, Logger: zap.New(core)})
	if err != nil {
		t.Fatal(err)
	}
	startWorker(t, w)
	eventually(t, "the job to succeed", func() bool { return q.Len() == 0 })
	if effects.Load() != 1 {
		t.Fatalf("effects = %d, want 1", effects.Load())
	}
	sawBusy := false
	for _, e := range logs.FilterMessage("job failed").All() {
		if msg, _ := e.ContextMap()["error"].(string); strings.Contains(msg, jobs.ErrInProgress.Error()) {
			sawBusy = true
		}
	}
	if !sawBusy {
		t.Fatal("no attempt reported the effect in progress")
	}
}

func TestOnceWithoutAStoreJustRuns(t *testing.T) {
	calls := 0
	for i := 0; i < 2; i++ {
		if err := jobs.Once(context.Background(), "k", func(context.Context) error { calls++; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (no store: nothing to remember)", calls)
	}
	if err := jobs.Once(context.Background(), "", func(context.Context) error { return nil }); err == nil {
		t.Fatal("Once accepted an empty key")
	}
}

func TestMemoryClaimsAreSwept(t *testing.T) {
	clock := newFakeClock()
	q := jobs.NewMemoryQueue(jobs.WithMemoryClock(clock.Now))
	ctx := context.Background()
	for i := 0; i < 300; i++ {
		key := fmt.Sprintf("effect:%d", i)
		if _, err := q.BeginOnce(ctx, key, "t", time.Minute); err != nil {
			t.Fatal(err)
		}
		if err := q.FinishOnce(ctx, key, "t", time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	clock.Advance(2 * time.Hour) // every record has expired
	for i := 0; i < 300; i++ {
		if _, err := q.BeginOnce(ctx, fmt.Sprintf("fresh:%d", i), "t", time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if n := q.ClaimCount(); n >= 600 {
		t.Fatalf("%d claims held, want the expired ones swept", n)
	}
}

// failingFinish records nothing: its FinishOnce always fails.
type failingFinish struct{ jobs.OnceStore }

func (failingFinish) FinishOnce(context.Context, string, string, time.Duration) error {
	return errors.New("redis: connection reset")
}

// TestOnceSucceedsWhenOnlyTheRecordFails: failing the job after the effect ran
// would retry it and repeat the effect.
func TestOnceSucceedsWhenOnlyTheRecordFails(t *testing.T) {
	reg := jobs.NewRegistry()
	var effects atomic.Int32
	jobs.MustRegister(reg, func(ctx context.Context, _ sendWelcome) error {
		return jobs.Once(ctx, "e", func(context.Context) error { effects.Add(1); return nil })
	})
	mem := jobs.NewMemoryQueue()
	q := &onceOverride{MemoryQueue: mem, store: failingFinish{mem}}
	if _, err := jobs.NewDispatcher(reg, mem).Dispatch(context.Background(), sendWelcome{}); err != nil {
		t.Fatal(err)
	}
	w, err := jobs.NewWorker(reg, q, jobs.WorkerOptions{Queues: []string{"default"}, PollInterval: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	startWorker(t, w)
	eventually(t, "the job to succeed", func() bool { return mem.Len() == 0 })
	if effects.Load() != 1 {
		t.Fatalf("effects = %d, want 1", effects.Load())
	}
}

// onceOverride is a MemoryQueue whose OnceStore is replaced.
type onceOverride struct {
	*jobs.MemoryQueue
	store jobs.OnceStore
}

func (o *onceOverride) BeginOnce(ctx context.Context, key, token string, lock time.Duration) (jobs.OnceState, error) {
	return o.store.BeginOnce(ctx, key, token, lock)
}

func (o *onceOverride) FinishOnce(ctx context.Context, key, token string, keep time.Duration) error {
	return o.store.FinishOnce(ctx, key, token, keep)
}

// TestOncePanicReleasesTheLock: a panicking effect is retried (Run recovers
// it), and the retry must not find the failed attempt's lock.
func TestOncePanicReleasesTheLock(t *testing.T) {
	reg := jobs.NewRegistry()
	var calls atomic.Int32
	jobs.MustRegister(reg, func(ctx context.Context, _ sendWelcome) error {
		return jobs.Once(ctx, "e", func(context.Context) error {
			if calls.Add(1) == 1 {
				panic("nil map")
			}
			return nil
		})
	}, jobs.WithOptions(jobs.Options{Backoff: jobs.Constant(5 * time.Millisecond)}))
	q := jobs.NewMemoryQueue()
	if _, err := jobs.NewDispatcher(reg, q).Dispatch(context.Background(), sendWelcome{}); err != nil {
		t.Fatal(err)
	}
	w, err := jobs.NewWorker(reg, q, jobs.WorkerOptions{Queues: []string{"default"}, PollInterval: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	startWorker(t, w)
	eventually(t, "the retry to run the effect", func() bool { return q.Len() == 0 })
	if calls.Load() != 2 {
		t.Fatalf("effect ran %d times, want the panic then the retry", calls.Load())
	}
}

func TestMemoryFinishOnceRefusesAnExpiredLock(t *testing.T) {
	clock := newFakeClock()
	q := jobs.NewMemoryQueue(jobs.WithMemoryClock(clock.Now))
	ctx := context.Background()
	if _, err := q.BeginOnce(ctx, "e", "t1", time.Minute); err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Minute) // the run outlasted its lock
	if err := q.FinishOnce(ctx, "e", "t1", time.Hour); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatalf("FinishOnce(expired lock) = %v, want ErrLeaseLost, as on Redis", err)
	}
	if state, _ := q.BeginOnce(ctx, "e", "t2", time.Minute); state != jobs.OnceAcquired {
		t.Fatalf("after the refused finish = %v, want the key free", state)
	}
}

// TestOnceLockOutlivesMaxAttempts: a lock held by a crashed run can outlast
// every attempt the job is allowed. Waiting on it is not failing: the job is
// retried past MaxAttempts, runs the effect once the lock expires, and is
// never buried.
func TestOnceLockOutlivesMaxAttempts(t *testing.T) {
	q := jobs.NewMemoryQueue()
	reg := jobs.NewRegistry()
	var effects atomic.Int32
	jobs.MustRegister(reg, func(ctx context.Context, _ sendWelcome) error {
		return jobs.Once(ctx, "crashed", func(context.Context) error { effects.Add(1); return nil })
	}, jobs.WithOptions(jobs.Options{MaxAttempts: 2, Backoff: jobs.Constant(10 * time.Millisecond)}))
	// A run that crashed after taking the lock: nothing releases it before
	// it expires, long after two attempts.
	if _, err := jobs.OnceStore(q).BeginOnce(context.Background(), "crashed", "dead-run", 300*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.NewDispatcher(reg, q).Dispatch(context.Background(), sendWelcome{}); err != nil {
		t.Fatal(err)
	}
	w, err := jobs.NewWorker(reg, q, jobs.WorkerOptions{Queues: []string{"default"}, PollInterval: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	startWorker(t, w)
	eventually(t, "the job to succeed", func() bool { return q.Len() == 0 })
	if effects.Load() != 1 {
		t.Fatalf("effects = %d, want 1", effects.Load())
	}
	if failed, _ := q.Failed(context.Background(), "default", 0); len(failed) != 0 {
		t.Fatalf("a job waiting on a lock was buried: %+v", failed)
	}
}
