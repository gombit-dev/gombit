package jobs_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/jobs"
)

// TestRedisRetriesSurviveWorkerRestarts: the attempt count and the retry
// time live in Redis. A job that failed under one worker runs its next
// attempt under another, not before its backoff, and is given up at
// MaxAttempts counted across both.
func TestRedisRetriesSurviveWorkerRestarts(t *testing.T) {
	addr := redisTestAddr(t)
	ns := testNamespace()
	const backoff = 300 * time.Millisecond
	var mu sync.Mutex
	var attempts []int
	var ranAt []time.Time
	newReg := func() *jobs.Registry {
		reg := jobs.NewRegistry()
		jobs.MustRegister(reg, func(ctx context.Context, _ failingJob) error {
			info, _ := jobs.InfoFromContext(ctx)
			mu.Lock()
			attempts = append(attempts, info.Attempt)
			ranAt = append(ranAt, time.Now())
			mu.Unlock()
			return errors.New("smtp down")
		}, jobs.WithOptions(jobs.Options{MaxAttempts: 2, Backoff: jobs.Constant(backoff)}))
		return reg
	}
	opts := jobs.WorkerOptions{Queues: []string{"default"}, PollInterval: 10 * time.Millisecond}
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(attempts) }

	first := jobs.NewRedisQueue(redisClient(t, addr, ns), ns)
	reg := newReg()
	if _, err := jobs.NewDispatcher(reg, first).Dispatch(context.Background(), failingJob{}); err != nil {
		t.Fatal(err)
	}
	w, err := jobs.NewWorker(reg, first, opts)
	if err != nil {
		t.Fatal(err)
	}
	stop := startWorker(t, w)
	eventually(t, "the first attempt", func() bool { return count() == 1 })
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	_ = first.Close()

	second := jobs.NewRedisQueue(redisClient(t, addr, ns), ns)
	w, err = jobs.NewWorker(newReg(), second, opts)
	if err != nil {
		t.Fatal(err)
	}
	startWorker(t, w)
	eventually(t, "the second attempt, in a new worker", func() bool { return count() == 2 })
	time.Sleep(3 * backoff) // a third would be a bug: MaxAttempts is 2

	mu.Lock()
	defer mu.Unlock()
	if len(attempts) != 2 || attempts[0] != 1 || attempts[1] != 2 {
		t.Fatalf("attempts = %v, want 1 then 2 across the restart and no third", attempts)
	}
	if gap := ranAt[1].Sub(ranAt[0]); gap < backoff {
		t.Fatalf("the retry ran %s after the failure, before its %s backoff", gap, backoff)
	}
	if _, err := second.Reserve(context.Background(), []string{"default"}, time.Minute); !errors.Is(err, jobs.ErrNoJob) {
		t.Fatalf("after giving up the queue still holds the job: %v", err)
	}
}
