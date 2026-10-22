package auth

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Background work the auth routes accept and finish after answering (the
// password reset message, AUTH-2). It is owned by the Service: a bounded
// queue (admission is decided before the work starts, and a request that
// is not admitted is answered as such), a fixed set of workers, a deadline
// on every task, and Close, which drains and then cancels. framework.App
// closes its Service when it stops, before its stop hooks run.

const (
	// backgroundWorkers is how many background tasks run at once.
	backgroundWorkers = 4
	// backgroundQueue is how many admitted tasks may wait for a worker.
	backgroundQueue = 64
	// backgroundTaskTimeout bounds one task: a Mailer that never returns
	// is cancelled, and its worker freed.
	backgroundTaskTimeout = 30 * time.Second
	// backgroundCloseGrace is how long Close waits for the tasks it
	// cancelled to return.
	backgroundCloseGrace = 5 * time.Second
	// backgroundRetryAfter is the Retry-After of a request not admitted.
	backgroundRetryAfter = 5 * time.Second
)

// errBackgroundBusy is a task the Service did not admit: its queue is full,
// or it is closed.
var errBackgroundBusy = errors.New("auth: too much background work at once; try again shortly")

type backgroundTask struct {
	// values is the request's context, cancellation removed: its values (the
	// request id, the client address) go with the task.
	values context.Context
	run    func(context.Context)
}

type backgroundRunner struct {
	mu      sync.Mutex
	queue   chan backgroundTask
	started bool
	closed  bool
	workers sync.WaitGroup
	// stop cancels every task, queued or running (Close, once draining
	// runs out of time).
	stop    context.Context
	cancel  context.CancelFunc
	timeout time.Duration
}

func newBackgroundRunner() *backgroundRunner {
	stop, cancel := context.WithCancel(context.Background()) // #nosec G118 -- cancel is kept on the runner and called by close.
	return &backgroundRunner{
		queue:   make(chan backgroundTask, backgroundQueue),
		stop:    stop,
		cancel:  cancel,
		timeout: backgroundTaskTimeout,
	}
}

// submit admits run, to be called with a context carrying ctx's values, a
// deadline, and the runner's cancellation. It returns errBackgroundBusy,
// and does not run it, when the queue is full or the runner closed.
func (r *backgroundRunner) submit(ctx context.Context, run func(context.Context)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errBackgroundBusy
	}
	if !r.started {
		r.started = true
		for range backgroundWorkers {
			r.workers.Add(1)
			go r.work()
		}
	}
	select {
	case r.queue <- backgroundTask{values: context.WithoutCancel(ctx), run: run}:
		return nil
	default:
		return errBackgroundBusy
	}
}

func (r *backgroundRunner) work() {
	defer r.workers.Done()
	for task := range r.queue {
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
	close(r.queue)
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
