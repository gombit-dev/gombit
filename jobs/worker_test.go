package jobs_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/gombit-dev/gombit/jobs"
)

type blockJob struct {
	N int `json:"n"`
}

func (blockJob) JobName() string { return "block_job" }

// startWorker runs w until the test ends (or stop is called) and returns a
// stop func that reports Run's error.
func startWorker(t *testing.T, w *jobs.Worker) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	var once sync.Once
	var err error
	stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case err = <-done:
			case <-time.After(10 * time.Second):
				err = errors.New("worker did not stop")
			}
		})
		return err
	}
	t.Cleanup(func() { _ = stop() })
	return stop
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func dispatchN(t *testing.T, d *jobs.Dispatcher, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := d.Dispatch(context.Background(), blockJob{N: i}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWorkerRunsJobsWithBoundedConcurrency(t *testing.T) {
	reg := jobs.NewRegistry()
	var running, maxRunning, done atomic.Int32
	release := make(chan struct{})
	jobs.MustRegister(reg, func(context.Context, blockJob) error {
		n := running.Add(1)
		for {
			m := maxRunning.Load()
			if n <= m || maxRunning.CompareAndSwap(m, n) {
				break
			}
		}
		<-release
		running.Add(-1)
		done.Add(1)
		return nil
	})
	q := jobs.NewMemoryQueue()
	dispatchN(t, jobs.NewDispatcher(reg, q), 10)

	w, err := jobs.NewWorker(reg, q, jobs.WorkerOptions{Queues: []string{"default"}, Concurrency: 3, PollInterval: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	stop := startWorker(t, w)
	eventually(t, "three jobs running", func() bool { return running.Load() == 3 })
	time.Sleep(30 * time.Millisecond) // a fourth must not start
	if running.Load() != 3 {
		t.Fatalf("running = %d, want the concurrency limit 3", running.Load())
	}
	close(release)
	eventually(t, "all jobs done", func() bool { return done.Load() == 10 && q.Len() == 0 })
	if maxRunning.Load() != 3 {
		t.Fatalf("max concurrent = %d, want 3", maxRunning.Load())
	}
	if err := stop(); err != nil {
		t.Fatalf("Run() = %v", err)
	}
}

func TestWorkerRetriesAFailedJob(t *testing.T) {
	reg := jobs.NewRegistry()
	var attempts []int
	var delays []int
	var mu sync.Mutex
	jobs.MustRegister(reg, func(ctx context.Context, _ blockJob) error {
		info, _ := jobs.InfoFromContext(ctx)
		mu.Lock()
		attempts = append(attempts, info.Attempt)
		mu.Unlock()
		if info.Attempt < 3 {
			return errors.New("smtp down")
		}
		return nil
	}, jobs.WithOptions(jobs.Options{Backoff: func(attempt int) time.Duration {
		mu.Lock()
		delays = append(delays, attempt)
		mu.Unlock()
		return 20 * time.Millisecond
	}}))
	q := jobs.NewMemoryQueue()
	dispatchN(t, jobs.NewDispatcher(reg, q), 1)
	core, logs := observer.New(zap.InfoLevel)
	w, err := jobs.NewWorker(reg, q, jobs.WorkerOptions{
		Queues: []string{"default"}, PollInterval: 5 * time.Millisecond, Logger: zap.New(core),
	})
	if err != nil {
		t.Fatal(err)
	}
	startWorker(t, w)
	eventually(t, "the job to succeed", func() bool { return q.Len() == 0 })
	mu.Lock()
	defer mu.Unlock()
	if len(attempts) != 3 || attempts[2] != 3 || len(delays) != 2 || delays[1] != 2 {
		t.Fatalf("attempts = %v, retry delays asked for attempts %v; want 1,2,3 and 1,2", attempts, delays)
	}
	failed := logs.FilterMessage("job failed").All()
	if len(failed) != 2 || failed[0].ContextMap()["kind"] != "handler" || failed[0].ContextMap()["job"] != "block_job" || failed[0].ContextMap()["metadata"] != nil {
		t.Fatalf("failure logs = %+v", failed)
	}
	if len(logs.FilterMessage("job succeeded").All()) != 1 {
		t.Fatal("no success log")
	}
}

// TestWorkerShutdownFinishesInFlightJobs: shutdown stops reserving at once and
// lets the running job finish and be acknowledged.
func TestWorkerShutdownFinishesInFlightJobs(t *testing.T) {
	reg := jobs.NewRegistry()
	started := make(chan struct{}, 10)
	release := make(chan struct{})
	var finished atomic.Int32
	jobs.MustRegister(reg, func(context.Context, blockJob) error {
		started <- struct{}{}
		<-release
		finished.Add(1)
		return nil
	})
	q := jobs.NewMemoryQueue()
	dispatchN(t, jobs.NewDispatcher(reg, q), 3)
	w, err := jobs.NewWorker(reg, q, jobs.WorkerOptions{Queues: []string{"default"}, PollInterval: 5 * time.Millisecond, ShutdownTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	stop := startWorker(t, w)
	<-started
	stopped := make(chan error, 1)
	go func() { stopped <- stop() }()
	time.Sleep(30 * time.Millisecond)
	close(release)
	if err := <-stopped; err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if finished.Load() != 1 || q.Len() != 2 {
		t.Fatalf("finished %d jobs, %d left queued; want the in-flight one done and the other two untouched", finished.Load(), q.Len())
	}
}

// TestWorkerShutdownCancelsAtTheTimeout: a job still running at the shutdown
// timeout sees its context canceled and goes back to the queue, available
// at once.
func TestWorkerShutdownCancelsAtTheTimeout(t *testing.T) {
	reg := jobs.NewRegistry()
	started := make(chan struct{}, 1)
	jobs.MustRegister(reg, func(ctx context.Context, _ blockJob) error {
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	})
	q := jobs.NewMemoryQueue()
	dispatchN(t, jobs.NewDispatcher(reg, q), 1)
	w, err := jobs.NewWorker(reg, q, jobs.WorkerOptions{Queues: []string{"default"}, PollInterval: 5 * time.Millisecond, ShutdownTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	stop := startWorker(t, w)
	<-started
	begin := time.Now()
	if err := stop(); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if took := time.Since(begin); took < 50*time.Millisecond || took > 2*time.Second {
		t.Fatalf("shutdown took %s, want about the 50ms timeout", took)
	}
	d, err := q.Reserve(context.Background(), []string{"default"}, time.Minute)
	if err != nil || d.Envelope.Attempt != 2 {
		t.Fatalf("after shutdown Reserve = %+v, %v; want the interrupted job back at once (attempt 2)", d, err)
	}
}

// TestACrashedWorkerLosesNoJob: a worker that stops without acknowledging (a
// handler that ignores cancellation here, a killed process in production)
// leaves the job leased; the lease expires and another worker gets it.
func TestACrashedWorkerLosesNoJob(t *testing.T) {
	defer jobs.SetShutdownGrace(20 * time.Millisecond)()
	reg := jobs.NewRegistry()
	hang := make(chan struct{})
	defer close(hang)
	var calls atomic.Int32
	jobs.MustRegister(reg, func(ctx context.Context, _ blockJob) error {
		if calls.Add(1) == 1 {
			<-hang // ignores cancellation
		}
		return nil
	})
	q := jobs.NewMemoryQueue()
	dispatchN(t, jobs.NewDispatcher(reg, q), 1)
	opts := jobs.WorkerOptions{Queues: []string{"default"}, PollInterval: 5 * time.Millisecond, ShutdownTimeout: 20 * time.Millisecond, Lease: 150 * time.Millisecond}
	first, err := jobs.NewWorker(reg, q, opts)
	if err != nil {
		t.Fatal(err)
	}
	stopFirst := startWorker(t, first)
	eventually(t, "the first attempt", func() bool { return calls.Load() == 1 })
	if err := stopFirst(); err == nil {
		t.Fatal("Run() = nil, want an error naming the job left to its lease")
	}

	second, err := jobs.NewWorker(reg, q, opts)
	if err != nil {
		t.Fatal(err)
	}
	startWorker(t, second)
	eventually(t, "redelivery after the lease", func() bool { return calls.Load() == 2 && q.Len() == 0 })
}

// TestWorkerRenewsTheLeaseOfALongJob: a job running for several leases is not
// delivered to a second worker.
func TestWorkerRenewsTheLeaseOfALongJob(t *testing.T) {
	reg := jobs.NewRegistry()
	var calls atomic.Int32
	jobs.MustRegister(reg, func(context.Context, blockJob) error {
		calls.Add(1)
		time.Sleep(300 * time.Millisecond)
		return nil
	})
	q := jobs.NewMemoryQueue()
	dispatchN(t, jobs.NewDispatcher(reg, q), 1)
	opts := jobs.WorkerOptions{Queues: []string{"default"}, PollInterval: 5 * time.Millisecond, Lease: 60 * time.Millisecond}
	for i := 0; i < 2; i++ {
		w, err := jobs.NewWorker(reg, q, opts)
		if err != nil {
			t.Fatal(err)
		}
		startWorker(t, w)
	}
	eventually(t, "the job to finish", func() bool { return q.Len() == 0 })
	if calls.Load() != 1 {
		t.Fatalf("the job ran %d times across two workers, want once", calls.Load())
	}
}

// lossyQueue drops every lease renewal as lost and records releases.
type lossyQueue struct {
	*jobs.MemoryQueue
	released atomic.Int32
}

func (*lossyQueue) Extend(context.Context, jobs.Delivery, time.Duration) error {
	return jobs.ErrLeaseLost
}

func (q *lossyQueue) Release(ctx context.Context, d jobs.Delivery, at time.Time) error {
	q.released.Add(1)
	return q.MemoryQueue.Release(ctx, d, at)
}

func TestWorkerCancelsAJobWhoseLeaseWasLost(t *testing.T) {
	reg := jobs.NewRegistry()
	canceled := make(chan struct{})
	jobs.MustRegister(reg, func(ctx context.Context, _ blockJob) error {
		<-ctx.Done()
		close(canceled)
		return ctx.Err()
	})
	q := &lossyQueue{MemoryQueue: jobs.NewMemoryQueue()}
	dispatchN(t, jobs.NewDispatcher(reg, q), 1)
	core, logs := observer.New(zap.InfoLevel)
	w, err := jobs.NewWorker(reg, q, jobs.WorkerOptions{Queues: []string{"default"}, PollInterval: 5 * time.Millisecond, Lease: 30 * time.Millisecond, Logger: zap.New(core)})
	if err != nil {
		t.Fatal(err)
	}
	startWorker(t, w)
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("a job whose lease was lost kept running")
	}
	eventually(t, "the abandoned log", func() bool { return logs.FilterMessage("job abandoned after its lease was lost").Len() == 1 })
	if q.released.Load() != 0 || logs.FilterMessage("job failed").Len() != 0 {
		t.Fatalf("released %d times, failure logs %d; a lost lease is another worker's job, not a failure", q.released.Load(), logs.FilterMessage("job failed").Len())
	}
}

func TestReserveBackoff(t *testing.T) {
	for failures, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 6: 30 * time.Second, 50: 30 * time.Second} {
		if got := jobs.ReserveBackoff(time.Second, failures); got != want {
			t.Errorf("ReserveBackoff(1s, %d) = %s, want %s", failures, got, want)
		}
	}
}

// TestWorkerSurvivesATinyLease: the heartbeat interval has a floor, so a
// nanosecond lease cannot make time.NewTicker panic.
func TestWorkerSurvivesATinyLease(t *testing.T) {
	reg := jobs.NewRegistry()
	var ran atomic.Bool
	jobs.MustRegister(reg, func(context.Context, blockJob) error { ran.Store(true); return nil })
	q := jobs.NewMemoryQueue()
	dispatchN(t, jobs.NewDispatcher(reg, q), 1)
	w, err := jobs.NewWorker(reg, q, jobs.WorkerOptions{Queues: []string{"default"}, PollInterval: 5 * time.Millisecond, Lease: 2})
	if err != nil {
		t.Fatal(err)
	}
	startWorker(t, w)
	eventually(t, "the job to run", ran.Load)
}

// poisonQueue hands out one undecodable delivery, as the Redis driver does
// for a stored envelope that no longer decodes.
type poisonQueue struct {
	*jobs.MemoryQueue
	once    sync.Once
	buried  atomic.Bool
	failure atomic.Value
}

func (p *poisonQueue) Reserve(ctx context.Context, queues []string, lease time.Duration) (jobs.Delivery, error) {
	first := false
	p.once.Do(func() { first = true })
	if first {
		return jobs.Delivery{Queue: "default", Envelope: jobs.Envelope{ID: "poison"}, Receipt: "r",
			Err: &jobs.Error{Kind: jobs.KindDecode, Err: errors.New("bad envelope")}}, nil
	}
	return p.MemoryQueue.Reserve(ctx, queues, lease)
}

func (p *poisonQueue) Bury(ctx context.Context, d jobs.Delivery, f jobs.Failure) error {
	if d.Envelope.ID == "poison" {
		p.failure.Store(f)
		p.buried.Store(true)
		return nil
	}
	return p.MemoryQueue.Bury(ctx, d, f)
}

func TestWorkerSetsAsideAnUndecodableEnvelope(t *testing.T) {
	q := &poisonQueue{MemoryQueue: jobs.NewMemoryQueue()}
	reg := jobs.NewRegistry()
	var ran atomic.Bool
	jobs.MustRegister(reg, func(context.Context, blockJob) error { ran.Store(true); return nil })
	dispatchN(t, jobs.NewDispatcher(reg, q.MemoryQueue), 1)
	core, logs := observer.New(zap.ErrorLevel)
	// An hour's poll: the job behind the poison one must run without an idle wait.
	w, err := jobs.NewWorker(reg, q, jobs.WorkerOptions{Queues: []string{"default"}, PollInterval: time.Hour, Logger: zap.New(core)})
	if err != nil {
		t.Fatal(err)
	}
	startWorker(t, w)
	eventually(t, "the poison envelope set aside", q.buried.Load)
	eventually(t, "the next job to run at once", ran.Load)
	if f, _ := q.failure.Load().(jobs.Failure); f.Reason != jobs.ReasonUndecodable || f.Kind != jobs.KindDecode || f.Error == "" {
		t.Fatalf("failure = %+v, want an undecodable-envelope record", f)
	}
	if logs.FilterMessage("job failed for good: its envelope does not decode").Len() != 1 {
		t.Fatalf("logs = %v", logs.All())
	}
}

func TestNewWorkerValidates(t *testing.T) {
	reg, q := jobs.NewRegistry(), jobs.NewMemoryQueue()
	for name, tc := range map[string]struct {
		queue jobs.Queue
		opts  jobs.WorkerOptions
		want  error
	}{
		"sync driver":     {nil, jobs.WorkerOptions{Queues: []string{"default"}}, jobs.ErrNoQueue},
		"no queues":       {q, jobs.WorkerOptions{}, nil},
		"bad queue name":  {q, jobs.WorkerOptions{Queues: []string{"Bad"}}, jobs.ErrInvalidQueue},
		"negative values": {q, jobs.WorkerOptions{Queues: []string{"default"}, Concurrency: -1}, nil},
	} {
		_, err := jobs.NewWorker(reg, tc.queue, tc.opts)
		if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
			t.Errorf("%s: NewWorker() error = %v, want %v", name, err, tc.want)
		}
	}
	if _, err := jobs.NewWorker(nil, q, jobs.WorkerOptions{Queues: []string{"default"}}); err == nil {
		t.Error("NewWorker accepted a nil registry")
	}
}

// ctxQueue wraps a MemoryQueue whose Reserve and Extend are slow and honor
// cancellation. (The Redis client does not honor cancellation mid-call; see
// stubbornQueue.)
type ctxQueue struct {
	*jobs.MemoryQueue
	reserveBlocks bool          // Reserve waits for ctx
	reserveDelay  time.Duration // Reserve ignores ctx, then answers
	extendBlocks  bool          // Extend waits for ctx
	released      atomic.Int32
}

func (q *ctxQueue) Reserve(ctx context.Context, queues []string, lease time.Duration) (jobs.Delivery, error) {
	if q.reserveBlocks {
		<-ctx.Done()
		return jobs.Delivery{}, ctx.Err()
	}
	time.Sleep(q.reserveDelay)
	return q.MemoryQueue.Reserve(ctx, queues, lease)
}

func (q *ctxQueue) Extend(ctx context.Context, d jobs.Delivery, lease time.Duration) error {
	if q.extendBlocks {
		<-ctx.Done()
		return ctx.Err()
	}
	return q.MemoryQueue.Extend(ctx, d, lease)
}

func (q *ctxQueue) Release(ctx context.Context, d jobs.Delivery, at time.Time) error {
	q.released.Add(1)
	return q.MemoryQueue.Release(ctx, d, at)
}

func TestShutdownCancelsAReserveInFlight(t *testing.T) {
	q := &ctxQueue{MemoryQueue: jobs.NewMemoryQueue(), reserveBlocks: true}
	core, logs := observer.New(zap.ErrorLevel)
	w, err := jobs.NewWorker(jobs.NewRegistry(), q, jobs.WorkerOptions{Queues: []string{"default"}, Logger: zap.New(core)})
	if err != nil {
		t.Fatal(err)
	}
	stop := startWorker(t, w)
	time.Sleep(20 * time.Millisecond) // inside Reserve
	begin := time.Now()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(begin); took > time.Second {
		t.Fatalf("stopping took %s; a Reserve in flight must see the cancellation", took)
	}
	if logs.Len() != 0 {
		t.Fatalf("shutdown logged errors: %v", logs.All())
	}
}

// TestADeliveryAfterShutdownIsHandedBack: a Reserve that answers after the
// worker was told to stop does not start the job; it goes back to the queue.
func TestADeliveryAfterShutdownIsHandedBack(t *testing.T) {
	reg := jobs.NewRegistry()
	var ran atomic.Bool
	jobs.MustRegister(reg, func(context.Context, blockJob) error { ran.Store(true); return nil })
	q := &ctxQueue{MemoryQueue: jobs.NewMemoryQueue(), reserveDelay: 150 * time.Millisecond}
	dispatchN(t, jobs.NewDispatcher(reg, q.MemoryQueue), 1)
	w, err := jobs.NewWorker(reg, q, jobs.WorkerOptions{Queues: []string{"default"}})
	if err != nil {
		t.Fatal(err)
	}
	stop := startWorker(t, w)
	time.Sleep(30 * time.Millisecond) // inside the slow Reserve
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if ran.Load() || q.released.Load() != 1 {
		t.Fatalf("ran = %v, released %d; want the late delivery handed back unstarted", ran.Load(), q.released.Load())
	}
	if d, err := q.MemoryQueue.Reserve(context.Background(), []string{"default"}, time.Minute); err != nil || d.Envelope.ID == "" {
		t.Fatalf("the job is not back in the queue: %+v, %v", d, err)
	}
}

// TestAFinishedJobIsNotHeldByARenewal: a handler that returns while a lease
// renewal is in flight is acked at once; the renewal is canceled, not waited
// out.
func TestAFinishedJobIsNotHeldByARenewal(t *testing.T) {
	reg := jobs.NewRegistry()
	jobs.MustRegister(reg, func(context.Context, blockJob) error {
		time.Sleep(60 * time.Millisecond) // long enough for a renewal to start
		return nil
	})
	q := &ctxQueue{MemoryQueue: jobs.NewMemoryQueue(), extendBlocks: true}
	dispatchN(t, jobs.NewDispatcher(reg, q.MemoryQueue), 1)
	w, err := jobs.NewWorker(reg, q, jobs.WorkerOptions{Queues: []string{"default"}, PollInterval: 5 * time.Millisecond, Lease: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	begin := time.Now()
	startWorker(t, w)
	eventually(t, "the ack", func() bool { return q.Len() == 0 })
	if took := time.Since(begin); took > time.Second {
		t.Fatalf("the ack came %s after start; it waited out the renewal", took)
	}
}

type failingJob struct {
	Permanent bool `json:"permanent"`
}

func (failingJob) JobName() string { return "failing_job" }

// TestWorkerGivesUp: a job is retried until MaxAttempts, and a Permanent
// failure is not retried at all; either way it leaves the queue with an
// error log.
func TestWorkerGivesUp(t *testing.T) {
	reg := jobs.NewRegistry()
	var runs atomic.Int32
	jobs.MustRegister(reg, func(_ context.Context, job failingJob) error {
		runs.Add(1)
		if job.Permanent {
			return jobs.Permanent(errors.New("user 7 was deleted"))
		}
		return errors.New("smtp down")
	}, jobs.WithOptions(jobs.Options{MaxAttempts: 3, Backoff: jobs.Constant(5 * time.Millisecond)}))
	q := jobs.NewMemoryQueue()
	d := jobs.NewDispatcher(reg, q)
	core, logs := observer.New(zap.InfoLevel)
	w, err := jobs.NewWorker(reg, q, jobs.WorkerOptions{Queues: []string{"default"}, PollInterval: 5 * time.Millisecond, Logger: zap.New(core)})
	if err != nil {
		t.Fatal(err)
	}
	startWorker(t, w)

	if _, err := d.Dispatch(context.Background(), failingJob{}); err != nil {
		t.Fatal(err)
	}
	failedCount := func() int {
		failed, err := q.Failed(context.Background(), "default", 0)
		if err != nil {
			t.Fatal(err)
		}
		return len(failed)
	}
	eventually(t, "the job to be given up", func() bool { return failedCount() == 1 })
	if runs.Load() != 3 {
		t.Fatalf("ran %d times, want MaxAttempts 3", runs.Load())
	}
	failed, _ := q.Failed(context.Background(), "default", 0)
	if f := failed[0]; f.Attempts != 3 || f.Failure.Reason != jobs.ReasonExhausted || f.Failure.Kind != jobs.KindHandler ||
		f.Failure.Error == "" || string(f.Envelope.Payload) != `{"permanent":false}` {
		t.Fatalf("failed job = %+v, want the original payload, 3 attempts, and why", f)
	}
	gaveUp := logs.FilterMessage("job failed for good").All()
	if len(gaveUp) != 1 || gaveUp[0].ContextMap()["reason"] != "attempts exhausted" || gaveUp[0].ContextMap()["attempt"] != int64(3) {
		t.Fatalf("give-up logs = %+v", gaveUp)
	}
	if _, ok := gaveUp[0].ContextMap()["payload"]; ok {
		t.Fatal("the give-up log carries the payload")
	}

	runs.Store(0)
	if _, err := d.Dispatch(context.Background(), failingJob{Permanent: true}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the permanent failure to be given up", func() bool { return failedCount() == 2 && logs.FilterMessage("job failed for good").Len() == 2 })
	if runs.Load() != 1 || logs.FilterMessage("job failed for good").All()[1].ContextMap()["reason"] != "permanent failure" {
		t.Fatalf("a permanent failure ran %d times", runs.Load())
	}
}

type slowJob struct{}

func (slowJob) JobName() string { return "slow_job" }

// TestWorkerRetriesTimeoutsAndBoundsUnknownJobs: a timeout is retried like
// any failure, and a job no handler knows (a newer deploy's) is retried
// under the registry defaults, then given up.
func TestWorkerRetriesTimeoutsAndBoundsUnknownJobs(t *testing.T) {
	reg := jobs.NewRegistry(jobs.WithDefaultOptions(jobs.Options{MaxAttempts: 2, Backoff: jobs.Constant(5 * time.Millisecond)}))
	var slowRuns atomic.Int32
	jobs.MustRegister(reg, func(ctx context.Context, _ slowJob) error {
		if slowRuns.Add(1) == 1 {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}, jobs.WithOptions(jobs.Options{Timeout: 20 * time.Millisecond}))
	q := jobs.NewMemoryQueue()
	if _, err := jobs.NewDispatcher(reg, q).Dispatch(context.Background(), slowJob{}); err != nil {
		t.Fatal(err)
	}
	if err := q.Push(context.Background(), "default", jobs.Envelope{ID: "u1", Name: "from_a_newer_deploy", Version: 1, Payload: []byte(`{}`)}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zap.InfoLevel)
	w, err := jobs.NewWorker(reg, q, jobs.WorkerOptions{Queues: []string{"default"}, PollInterval: 5 * time.Millisecond, Logger: zap.New(core)})
	if err != nil {
		t.Fatal(err)
	}
	startWorker(t, w)
	eventually(t, "both jobs settled", func() bool {
		failed, _ := q.Failed(context.Background(), "default", 0)
		return slowRuns.Load() == 2 && len(failed) == 1 && q.Len() == 1
	})
	if slowRuns.Load() != 2 {
		t.Fatalf("the timed-out job ran %d times, want a retry after the timeout", slowRuns.Load())
	}
	kinds := map[string]int{}
	for _, e := range logs.FilterMessage("job failed").All() {
		kinds[e.ContextMap()["kind"].(string)]++
	}
	if kinds["timeout"] != 1 || kinds["unknown_job"] != 1 {
		t.Fatalf("retried failure kinds = %v, want one timeout and one unknown_job", kinds)
	}
	gaveUp := logs.FilterMessage("job failed for good").All()
	if len(gaveUp) != 1 || gaveUp[0].ContextMap()["job"] != "from_a_newer_deploy" || gaveUp[0].ContextMap()["max_attempts"] != int64(2) {
		t.Fatalf("give-ups = %+v, want the unknown job after the default 2 attempts", gaveUp)
	}
}

// stubbornQueue behaves like the Redis driver on a slow or lossy network:
// calls ignore their context, and a reserve can lease a job and still fail
// (the reply was lost after the script ran).
type stubbornQueue struct {
	*jobs.MemoryQueue
	loseReply     atomic.Bool   // the next Reserve leases, then errors
	extendTakes   time.Duration // Extend runs this long, whatever its context
	extendStarted chan struct{}
}

func (q *stubbornQueue) Reserve(ctx context.Context, queues []string, lease time.Duration) (jobs.Delivery, error) {
	d, err := q.MemoryQueue.Reserve(context.WithoutCancel(ctx), queues, lease)
	if err == nil && q.loseReply.CompareAndSwap(true, false) {
		return jobs.Delivery{}, errors.New("i/o timeout")
	}
	return d, err
}

func (q *stubbornQueue) Extend(ctx context.Context, d jobs.Delivery, lease time.Duration) error {
	if q.extendStarted != nil {
		select {
		case q.extendStarted <- struct{}{}:
		default:
		}
	}
	time.Sleep(q.extendTakes)
	return q.MemoryQueue.Extend(context.WithoutCancel(ctx), d, lease)
}

// TestALostReserveReplyLosesNoJob: a reserve that leased a job but whose
// reply was lost leaves no receipt to release; the lease brings the job back
// and it runs.
func TestALostReserveReplyLosesNoJob(t *testing.T) {
	reg := jobs.NewRegistry()
	var runs atomic.Int32
	jobs.MustRegister(reg, func(context.Context, blockJob) error { runs.Add(1); return nil })
	q := &stubbornQueue{MemoryQueue: jobs.NewMemoryQueue()}
	q.loseReply.Store(true)
	dispatchN(t, jobs.NewDispatcher(reg, q.MemoryQueue), 1)
	w, err := jobs.NewWorker(reg, q, jobs.WorkerOptions{Queues: []string{"default"}, PollInterval: 5 * time.Millisecond, Lease: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	startWorker(t, w)
	eventually(t, "the job to run once its lost lease expired", func() bool { return runs.Load() == 1 && q.Len() == 0 })
}

// TestShutdownOutlastsAnUncancelableRenewal: a handler that returns while a
// renewal it cannot cancel is on the wire is still acknowledged before Run
// returns, because ShutdownGrace covers that call plus the ack.
func TestShutdownOutlastsAnUncancelableRenewal(t *testing.T) {
	reg := jobs.NewRegistry()
	release := make(chan struct{})
	jobs.MustRegister(reg, func(context.Context, blockJob) error { <-release; return nil })
	q := &stubbornQueue{MemoryQueue: jobs.NewMemoryQueue(), extendTakes: 300 * time.Millisecond, extendStarted: make(chan struct{}, 1)}
	dispatchN(t, jobs.NewDispatcher(reg, q.MemoryQueue), 1)
	w, err := jobs.NewWorker(reg, q, jobs.WorkerOptions{Queues: []string{"default"}, PollInterval: 5 * time.Millisecond, Lease: 30 * time.Millisecond, ShutdownTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	stop := startWorker(t, w)
	<-q.extendStarted // a renewal is on the wire
	stopped := make(chan error, 1)
	go func() { stopped <- stop() }()
	close(release) // the handler returns mid-renewal
	if err := <-stopped; err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if q.Len() != 0 {
		t.Fatal("the finished job was not acknowledged before the worker stopped")
	}
}

// TestAnInterruptedLastAttemptIsNotGivenUp: an attempt the worker canceled at
// the shutdown timeout goes back to the queue whatever the handler returned,
// even on its last allowed attempt and even as Permanent; it was interrupted,
// not failed.
func TestAnInterruptedLastAttemptIsNotGivenUp(t *testing.T) {
	for name, fail := range map[string]func(error) error{
		"a message that does not wrap it": func(err error) error { return errors.New("aborted: " + err.Error()) },
		"Permanent":                       func(error) error { return jobs.Permanent(errors.New("user gone")) },
	} {
		t.Run(name, func(t *testing.T) {
			reg := jobs.NewRegistry()
			started := make(chan struct{}, 1)
			jobs.MustRegister(reg, func(ctx context.Context, _ blockJob) error {
				started <- struct{}{}
				<-ctx.Done()
				return fail(ctx.Err())
			}, jobs.WithOptions(jobs.Options{MaxAttempts: 1}))
			q := jobs.NewMemoryQueue()
			dispatchN(t, jobs.NewDispatcher(reg, q), 1)
			w, err := jobs.NewWorker(reg, q, jobs.WorkerOptions{Queues: []string{"default"}, PollInterval: 5 * time.Millisecond, ShutdownTimeout: 30 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			stop := startWorker(t, w)
			<-started
			if err := stop(); err != nil {
				t.Fatal(err)
			}
			d, err := q.Reserve(context.Background(), []string{"default"}, time.Minute)
			if err != nil || d.Envelope.Attempt != 2 {
				t.Fatalf("after shutdown: %+v, %v; want the job back, as attempt 2", d, err)
			}
		})
	}
}

type upgradedJob struct {
	N int `json:"n"`
}

func (upgradedJob) JobName() string { return "upgraded_job" }
func (upgradedJob) JobVersion() int { return 2 }

// TestUpgradeStepErrorsAreRetried: a step that returns an error is retried
// (a deploy can fix it) like a step that panics, while bytes that cannot be
// a job (an unmarshal failure after the steps) are given up at once.
func TestUpgradeStepErrorsAreRetried(t *testing.T) {
	reg := jobs.NewRegistry()
	var stepCalls, runs atomic.Int32
	jobs.MustRegister(reg, func(context.Context, upgradedJob) error { runs.Add(1); return nil },
		jobs.WithOptions(jobs.Options{MaxAttempts: 3, Backoff: jobs.Constant(5 * time.Millisecond)}),
		jobs.UpgradeFrom(1, func(p json.RawMessage) (json.RawMessage, error) {
			if stepCalls.Add(1) == 1 {
				return nil, errors.New("temporary: lookup table unavailable")
			}
			return p, nil
		}))
	q := jobs.NewMemoryQueue()
	ctx := context.Background()
	for id, payload := range map[string]string{"step": `{"n":1}`, "bytes": `{"n":"not a number"}`} {
		version := 1
		if id == "bytes" {
			version = 2 // no step: straight to the decoder
		}
		if err := q.Push(ctx, "default", jobs.Envelope{ID: id, Name: "upgraded_job", Version: version, Payload: json.RawMessage(payload)}, time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	core, logs := observer.New(zap.InfoLevel)
	w, err := jobs.NewWorker(reg, q, jobs.WorkerOptions{Queues: []string{"default"}, PollInterval: 5 * time.Millisecond, Logger: zap.New(core)})
	if err != nil {
		t.Fatal(err)
	}
	startWorker(t, w)
	eventually(t, "the step job to succeed and the bad bytes to be given up", func() bool {
		return runs.Load() == 1 && logs.FilterMessage("job failed for good").Len() == 1
	})
	gaveUp := logs.FilterMessage("job failed for good").All()[0].ContextMap()
	if gaveUp["job_id"] != "bytes" || gaveUp["kind"] != "decode" || gaveUp["attempt"] != int64(1) {
		t.Fatalf("given up = %v, want the undecodable payload on its first attempt", gaveUp)
	}
	retried := logs.FilterMessage("job failed").All()
	if len(retried) != 1 || retried[0].ContextMap()["kind"] != "upgrade" || retried[0].ContextMap()["job_id"] != "step" {
		t.Fatalf("retried failures = %v, want the step error once", retried)
	}
}
