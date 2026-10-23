package auth

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Background work the auth routes accept and finish after answering (the
// password reset message, AUTH-2). It is owned by the Service: admission
// decided before the work starts (a request that is not admitted is
// answered as such), a fixed set of workers, a deadline on every task, and
// Close, which drains and then cancels. framework.App closes its Service
// when it stops, before its stop hooks run.
//
// Admission is a token bucket, and only that: it depends on when requests
// arrived, never on how busy the workers are. A known account's task holds
// a worker longer than an unknown address's, so admission by queue room
// would let a client tell them apart by saturating the queue; the bucket
// spends one token per request either way. Its rate is what the workers
// finish in the worst case (backgroundWorkers tasks every
// backgroundWorstCase), so the admitted work never outgrows them, and the
// queue behind it is unbounded: an admitted task is never dropped.

const (
	// backgroundWorkers is how many background tasks run at once.
	backgroundWorkers = 16
	// backgroundBurst is how many requests the bucket admits at once.
	backgroundBurst = 32
	// backgroundTaskTimeout bounds one task: a Mailer that never returns
	// is cancelled, and its worker freed.
	backgroundTaskTimeout = 30 * time.Second
	// backgroundWorstCase is the longest a task can hold a worker: its
	// deadline, then its tail steps (activation, withdrawal, the audit
	// event), each under its own tailTimeout.
	backgroundWorstCase = backgroundTaskTimeout + 3*tailTimeout
	// backgroundCloseGrace is how long Close waits for the tasks it
	// cancelled to return.
	backgroundCloseGrace = 5 * time.Second
	// backgroundClosedRetryAfter is the Retry-After of a request a closed
	// Service refuses.
	backgroundClosedRetryAfter = 5 * time.Second
)

// backgroundBusyError is a task the Service did not admit: the bucket is
// empty (retryAfter until its next token), or the Service is closed.
type backgroundBusyError struct{ retryAfter time.Duration }

func (backgroundBusyError) Error() string {
	return "auth: too much background work at once; try again shortly"
}

type backgroundTask struct {
	// values is the request's context, cancellation removed: its values (the
	// request id, the client address) go with the task.
	values context.Context
	run    func(context.Context)
}

type backgroundRunner struct {
	mu      sync.Mutex
	ready   *sync.Cond
	pending []backgroundTask
	started bool
	closed  bool
	workers sync.WaitGroup
	// stop cancels every task, queued or running (Close, once draining
	// runs out of time).
	stop    context.Context
	cancel  context.CancelFunc
	timeout time.Duration

	// The admission bucket: tokens, refilled at rate per second up to
	// burst, as of last (read from now).
	burst, rate, tokens float64
	last                time.Time
	now                 func() time.Time
	workerCount         int
}

func newBackgroundRunner() *backgroundRunner {
	stop, cancel := context.WithCancel(context.Background()) // #nosec G118 -- cancel is kept on the runner and called by close.
	r := &backgroundRunner{
		stop:        stop,
		cancel:      cancel,
		timeout:     backgroundTaskTimeout,
		burst:       backgroundBurst,
		rate:        float64(backgroundWorkers) / backgroundWorstCase.Seconds(),
		tokens:      backgroundBurst,
		now:         time.Now,
		workerCount: backgroundWorkers,
	}
	r.ready = sync.NewCond(&r.mu)
	r.last = r.now()
	return r
}

// admit takes a token, or reports how long until the next one. r.mu held.
func (r *backgroundRunner) admit() (time.Duration, bool) {
	now := r.now()
	if elapsed := now.Sub(r.last).Seconds(); elapsed > 0 {
		r.tokens = min(r.burst, r.tokens+elapsed*r.rate)
	}
	r.last = now
	if r.tokens >= 1 {
		r.tokens--
		return 0, true
	}
	return time.Duration((1 - r.tokens) / r.rate * float64(time.Second)), false
}

// submit admits run, to be called with a context carrying ctx's values, a
// deadline, and the runner's cancellation. It returns a
// backgroundBusyError, and does not run it, when the bucket is empty or the
// runner closed: nothing else, so the answer never depends on the work
// already admitted.
func (r *backgroundRunner) submit(ctx context.Context, run func(context.Context)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return backgroundBusyError{retryAfter: backgroundClosedRetryAfter}
	}
	if wait, ok := r.admit(); !ok {
		return backgroundBusyError{retryAfter: wait}
	}
	if !r.started {
		r.started = true
		for range r.workerCount {
			r.workers.Add(1)
			go r.work()
		}
	}
	r.pending = append(r.pending, backgroundTask{values: context.WithoutCancel(ctx), run: run})
	r.ready.Signal()
	return nil
}

// next waits for a task; false once the runner is closed and drained.
func (r *backgroundRunner) next() (backgroundTask, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for len(r.pending) == 0 && !r.closed {
		r.ready.Wait()
	}
	if len(r.pending) == 0 {
		return backgroundTask{}, false
	}
	task := r.pending[0]
	r.pending[0] = backgroundTask{}
	r.pending = r.pending[1:]
	return task, true
}

func (r *backgroundRunner) work() {
	defer r.workers.Done()
	for {
		task, ok := r.next()
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(task.values, r.timeout)
		unhook := context.AfterFunc(r.stop, cancel)
		task.run(ctx)
		unhook()
		cancel()
	}
}

// close stops admitting work, lets the admitted tasks finish until ctx is
// done, then cancels the rest and waits a short grace for them. It returns
// ctx's error when it had to cancel, and an error naming the tasks that
// still had not returned after the grace.
func (r *backgroundRunner) close(ctx context.Context) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.ready.Broadcast()
	r.mu.Unlock()

	done := make(chan struct{})
	go func() {
		r.workers.Wait()
		close(done)
	}()
	select {
	case <-done:
		r.cancel()
		return nil
	case <-ctx.Done():
	}
	r.cancel()
	timer := time.NewTimer(backgroundCloseGrace)
	defer timer.Stop()
	select {
	case <-done:
		return ctx.Err()
	case <-timer.C:
		return errors.Join(ctx.Err(), errors.New("auth: background work did not stop after it was cancelled"))
	}
}

// tailTimeout bounds one piece of work that must outlive its request: a
// token's activation or withdrawal after its message (detach).
const tailTimeout = 5 * time.Second

// detach is the context for work that must outlive the request that
// started it (the operation happened; the client hanging up must not stop
// its bookkeeping): ctx's values without its cancellation, a deadline of
// timeout, and cancelled when the Service is closed (Close, once draining
// runs out), so it never outlives its owner either.
func (s *Service) detach(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	unhook := context.AfterFunc(s.background.stop, cancel)
	return dctx, func() {
		unhook()
		cancel()
	}
}

// Close finishes the Service's background work (the password reset
// messages POST /auth/password/forgot accepted): it stops admitting more,
// waits for the admitted work until ctx is done, then cancels what is left.
// framework.App calls it when it stops; an application that mounts the
// routes itself calls it, on the Service Mount returned, on shutdown. Close is
// idempotent; after it, the routes answer background requests with 503.
func (s *Service) Close(ctx context.Context) error {
	return s.background.close(ctx)
}
