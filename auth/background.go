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
// backgroundWorstCase), so while tasks keep to their deadlines the admitted
// work never outgrows them.
//
// Go cannot stop a task that ignores its context (a Mailer that never
// returns): its worker is gone for good. So the resources stay hard-bounded
// whatever the work does: never more than backgroundWorkers goroutines, and
// never more than backgroundQueue tasks waiting. A task admitted when the
// queue is full, which keeping to the deadlines never causes (the bucket's
// burst plus what the workers hold fits in it), is discarded before any
// lookup and logged, and answered as admitted all the same: the answer
// never depends on the work already admitted, so an admission is a
// best-effort acceptance, not a promise.

const (
	// backgroundWorkers is how many background tasks run at once.
	backgroundWorkers = 16
	// backgroundBurst is how many requests the bucket admits at once.
	backgroundBurst = 32
	// backgroundQueue bounds the tasks waiting for a worker: room for the
	// burst and the workers' share of refills, so only workers stuck past
	// their deadlines can fill it.
	backgroundQueue = 64
	// backgroundTaskTimeout bounds one task: its context is cancelled
	// then, which frees its worker if the task honors it (a Mailer must).
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

// errBackgroundDiscarded is a task the bucket admitted and the full queue
// could not take (workers stuck past their deadlines): dropped before it
// ran, to be logged, and answered as admitted.
var errBackgroundDiscarded = errors.New("auth: background work queue full (workers stuck past their deadlines); a request was discarded")

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
	// closing is made by the first close and closed when its shutdown
	// returned; closeErr is that shutdown's result.
	closing  chan struct{}
	closeErr error
	workers  sync.WaitGroup
	// stop cancels every task, queued or running (Close, once draining
	// runs out of time).
	stop    context.Context
	cancel  context.CancelFunc
	timeout time.Duration
	// queueCap and closeGrace are backgroundQueue and backgroundCloseGrace
	// (tests shorten them).
	queueCap   int
	closeGrace time.Duration

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
		queueCap:    backgroundQueue,
		closeGrace:  backgroundCloseGrace,
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
// already admitted. An admitted task the full queue cannot take is dropped:
// errBackgroundDiscarded, which the caller logs and answers as admitted.
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
	if len(r.pending) >= r.queueCap {
		return errBackgroundDiscarded
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

// close shuts the runner down once, whoever asks and however often. The
// first call runs the shutdown: it stops admitting work, lets the admitted
// tasks finish until its ctx is done, then cancels the rest, drops the ones
// still waiting, and waits a short grace for the running ones. Its result
// is the shutdown's: nil when every admitted task finished, ctx's error
// when it had to cancel, joined with an error naming the tasks that still
// had not returned after the grace (a task that ignores its context keeps
// its goroutine, which Go cannot stop; close returns all the same).
//
// A call while the shutdown runs joins it: it returns the shutdown's
// result once that is known, or its own ctx's error if that ends first,
// never nil before the shutdown succeeded. A call after it returns the same
// result: a shutdown that failed stays failed.
func (r *backgroundRunner) close(ctx context.Context) error {
	r.mu.Lock()
	if r.closing != nil {
		done := r.closing
		r.mu.Unlock()
		// A finished shutdown's result comes first: when both are ready, a
		// select would pick at random, and a caller whose ctx is already
		// done would sometimes get that instead.
		select {
		case <-done:
			return r.closeErr
		default:
		}
		select {
		case <-done:
			return r.closeErr
		case <-ctx.Done():
			select {
			case <-done: // finished as ctx ended: the result still wins
				return r.closeErr
			default:
				return ctx.Err()
			}
		}
	}
	r.closed = true
	r.closing = make(chan struct{})
	r.ready.Broadcast()
	r.mu.Unlock()

	err := r.shutdown(ctx)
	r.closeErr = err // published by closing's close, read only after it
	close(r.closing)
	return err
}

// shutdown drains, then cancels, as close describes.
func (r *backgroundRunner) shutdown(ctx context.Context) error {
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
	// What has not started will not: release it now.
	r.mu.Lock()
	r.pending = nil
	r.mu.Unlock()
	timer := time.NewTimer(r.closeGrace)
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
// messages POST /auth/password/forgot accepted, the email-changed
// notifications): it stops admitting more,
// waits for the admitted work until ctx is done, then cancels what is left.
// framework.App calls it when it stops; an application that mounts the
// routes itself calls it, on the Service Mount returned, on shutdown. The
// shutdown runs once: a Close while another runs waits for it (or for its
// own ctx), and every Close returns its result, so none reports success
// before the work is done. After Close, the routes answer background
// requests with 503.
func (s *Service) Close(ctx context.Context) error {
	return s.background.close(ctx)
}
