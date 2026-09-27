package jobs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// WorkerOptions configures a Worker. Zero values take the defaults noted.
type WorkerOptions struct {
	// Queues are consumed in priority order: a job waiting on an earlier
	// queue goes first. Required.
	Queues []string
	// Concurrency is how many jobs run at once. Default 1.
	Concurrency int
	// Lease is how long a reserved job is held before another worker may
	// take it; the worker renews it every Lease/3 while the handler runs.
	// Default 5m.
	Lease time.Duration
	// PollInterval is how long an idle worker waits before looking again.
	// Default 1s.
	PollInterval time.Duration
	// ShutdownTimeout bounds shutdown: in-flight jobs get this long to
	// finish once the worker stops reserving, then their context is
	// canceled. Default 30s.
	ShutdownTimeout time.Duration
	// Logger receives one structured entry per job outcome. Default: no
	// logging.
	Logger *zap.Logger
}

// Worker runs the jobs of a Registry from a Queue. At most Concurrency jobs
// run at once; each holds a renewed lease, so a worker that dies leaves its
// jobs to be delivered again when their leases expire, not lost.
type Worker struct {
	registry *Registry
	queue    Queue
	opts     WorkerOptions
	log      *zap.Logger
}

// Defaults for WorkerOptions.
const (
	DefaultLease           = 5 * time.Minute
	DefaultPollInterval    = time.Second
	DefaultShutdownTimeout = 30 * time.Second
)

// ErrNoQueue: the sync driver has no queue for a worker to consume.
var ErrNoQueue = errors.New("jobs: no queue to work: the sync driver runs jobs at dispatch; set GOMBIT_JOBS_DRIVER=redis")

// NewWorker validates opts and returns a worker.
func NewWorker(registry *Registry, queue Queue, opts WorkerOptions) (*Worker, error) {
	if registry == nil {
		return nil, errors.New("jobs: worker: nil registry")
	}
	if queue == nil {
		return nil, ErrNoQueue
	}
	if len(opts.Queues) == 0 {
		return nil, errors.New("jobs: worker: no queues to consume")
	}
	for _, q := range opts.Queues {
		if !ValidName(q) {
			return nil, fmt.Errorf("%w: %q", ErrInvalidQueue, q)
		}
	}
	if opts.Concurrency < 0 || opts.Lease < 0 || opts.PollInterval < 0 || opts.ShutdownTimeout < 0 {
		return nil, errors.New("jobs: worker: concurrency and durations must not be negative")
	}
	if opts.Concurrency == 0 {
		opts.Concurrency = 1
	}
	if opts.Lease == 0 {
		opts.Lease = DefaultLease
	}
	if opts.PollInterval == 0 {
		opts.PollInterval = DefaultPollInterval
	}
	if opts.ShutdownTimeout == 0 {
		opts.ShutdownTimeout = DefaultShutdownTimeout
	}
	log := opts.Logger
	if log == nil {
		log = zap.NewNop()
	}
	return &Worker{registry: registry, queue: queue, opts: opts, log: log}, nil
}

// queueOpTimeout is the deadline of every queue call the worker makes
// (reserve, extend, ack, release). It is a real bound only on a client that
// enforces context deadlines: MemoryQueue, and a RedisQueue built with
// RedisClientOptions (as Open and OpenWithRedis build it); go-redis's default
// client ignores the caller's context. Ack and release run on their own
// context, not the worker's, so a job that finishes during shutdown is still
// acknowledged.
const queueOpTimeout = QueueCallTimeout

// QueueCallTimeout is the deadline of each queue call the worker makes. A
// stop can find one reserve on the wire, and the release of what it returns,
// before the shutdown timeout even starts: a supervisor's budget counts two.
const QueueCallTimeout = 4 * time.Second

// ShutdownGrace is how long Run waits, after canceling in-flight jobs at the
// shutdown timeout, for them to settle. It covers the worst case of one lease
// renewal still in flight when a handler returns plus the ack or release
// after it, each bounded by the 4s queue-call deadline. A process supervisor
// should allow ShutdownTimeout + ShutdownGrace and a margin (framework's
// WorkerKillAfter) before it kills a worker.
const ShutdownGrace = 10 * time.Second

// Run works the queues until ctx is canceled, then shuts down gracefully: it
// stops reserving at once, lets in-flight jobs finish for up to
// ShutdownTimeout, then cancels their context and returns. A job still
// running after that keeps no lease renewal, so it is delivered again once
// its lease expires.
func (w *Worker) Run(ctx context.Context) error {
	// Handlers run on a context that outlives ctx: shutdown begins by
	// canceling ctx, but in-flight jobs keep running until the timeout.
	jobCtx, cancelJobs := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelJobs()

	w.log.Info("jobs worker started",
		zap.Strings("queues", w.opts.Queues),
		zap.Int("concurrency", w.opts.Concurrency),
		zap.Duration("lease", w.opts.Lease))

	slots := make(chan struct{}, w.opts.Concurrency)
	var inFlight sync.WaitGroup
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return w.shutdown(&inFlight, cancelJobs)
		case slots <- struct{}{}:
		}
		if ctx.Err() != nil {
			<-slots
			return w.shutdown(&inFlight, cancelJobs)
		}
		// Reserve on the worker's own context: shutdown stops a call that has
		// not started, and a delivery that lands after the stop is handed back,
		// not run. (A call already on the wire runs to its deadline; if its
		// reply is lost after the queue leased a job, the lease brings the job
		// back.)
		d, err := w.reserve(ctx)
		if ctx.Err() != nil {
			<-slots
			if err == nil {
				if relErr := w.release(d, time.Now()); relErr != nil {
					w.log.Warn("jobs worker: returning a job reserved during shutdown; it returns when its lease expires",
						zap.String("job_id", d.Envelope.ID), zap.Error(relErr))
				}
			}
			return w.shutdown(&inFlight, cancelJobs)
		}
		if errors.Is(err, errSetAside) || errors.Is(err, errNotSetAside) {
			// An undecodable job was buried (or could not be, and stays leased
			// until it expires, so it is not reserved again meanwhile): look
			// again at once.
			<-slots
			continue
		}
		if err != nil {
			<-slots
			wait := w.opts.PollInterval
			if errors.Is(err, ErrNoJob) {
				failures = 0
			} else {
				// The queue is unreachable: back off, so a Redis outage is not a
				// reconnect and an error line every poll from every worker.
				failures++
				wait = reserveBackoff(w.opts.PollInterval, failures)
				w.log.Error("jobs worker: reserve failed", zap.Error(err), zap.Duration("retry_in", wait))
			}
			select {
			case <-ctx.Done():
			case <-time.After(wait):
			}
			continue
		}
		failures = 0
		inFlight.Add(1)
		go func() {
			defer inFlight.Done()
			defer func() { <-slots }()
			w.process(jobCtx, d)
		}()
	}
}

// reserve takes the next job. An envelope that no longer decodes cannot be
// run by any worker; it is set aside with the failed jobs and logged.
func (w *Worker) reserve(ctx context.Context) (Delivery, error) {
	opCtx, cancel := context.WithTimeout(ctx, queueOpTimeout)
	defer cancel()
	d, err := w.queue.Reserve(opCtx, w.opts.Queues, w.opts.Lease)
	if err != nil || d.Err == nil {
		return d, err
	}
	failure := Failure{Reason: ReasonUndecodable, Kind: KindDecode, Error: d.Err.Error(), At: time.Now()}
	if buryErr := w.bury(d, failure); buryErr != nil {
		w.log.Error("jobs worker: a job whose envelope does not decode could not be set aside; it returns when its lease expires",
			zap.String("job_id", d.Envelope.ID), zap.String("queue", d.Queue), zap.Error(d.Err), zap.NamedError("bury_error", buryErr))
		return Delivery{}, errNotSetAside
	}
	w.log.Error("job failed for good: its envelope does not decode",
		zap.String("job_id", d.Envelope.ID), zap.String("queue", d.Queue), zap.String("reason", ReasonUndecodable),
		zap.Error(d.Err), zap.String("inspect", "gombit jobs inspect "+d.Envelope.ID+" --queue "+d.Queue))
	return Delivery{}, errSetAside
}

var (
	// errSetAside: reserve buried an undecodable delivery with the failed
	// jobs.
	errSetAside = errors.New("jobs: undecodable job set aside")
	// errNotSetAside: reserve could not bury an undecodable delivery; it
	// stays leased until its lease expires.
	errNotSetAside = errors.New("jobs: undecodable job could not be set aside")
)

// maxReserveBackoff caps the wait between failing Reserve calls.
const maxReserveBackoff = 30 * time.Second

// reserveBackoff doubles the poll interval per consecutive Reserve failure,
// up to maxReserveBackoff.
func reserveBackoff(poll time.Duration, failures int) time.Duration {
	wait := poll
	for i := 1; i < failures && wait < maxReserveBackoff; i++ {
		wait *= 2
	}
	if wait > maxReserveBackoff {
		wait = maxReserveBackoff
	}
	return wait
}

// process runs one delivery: renew its lease while the handler runs, then
// ack it or release it for a retry.
func (w *Worker) process(jobCtx context.Context, d Delivery) {
	runCtx, cancelRun := context.WithCancel(jobCtx)
	if store, ok := w.queue.(OnceStore); ok {
		runCtx = withOnceStore(runCtx, store)
	}
	defer cancelRun()
	stopRenewing := w.renewLease(runCtx, cancelRun, d)

	started := time.Now()
	err := w.registry.Run(runCtx, d.Envelope)
	lost := stopRenewing()
	fields := []zap.Field{
		zap.String("job_id", d.Envelope.ID),
		zap.String("job", d.Envelope.Name),
		zap.String("queue", d.Queue),
		zap.Int("attempt", d.Envelope.Attempt),
		zap.Duration("duration", time.Since(started)),
	}
	if len(d.Envelope.Metadata) > 0 {
		// Under their own key, so a propagated name cannot shadow a field above.
		fields = append(fields, zap.Any("metadata", d.Envelope.Metadata))
	}
	if lost {
		// Another worker holds the job now; its outcome is that worker's to
		// record, and this lease can neither ack nor release it.
		w.log.Warn("job abandoned after its lease was lost", fields...)
		return
	}

	if err == nil {
		if ackErr := w.ack(d); ackErr != nil {
			w.log.Warn("jobs worker: job succeeded but its ack failed; it may run again",
				append(fields, zap.Error(ackErr))...)
			return
		}
		w.log.Info("job succeeded", fields...)
		return
	}

	policy := w.registry.Options(d.Envelope.Name)
	fields = append(fields, zap.String("kind", string(Classify(err))), zap.Error(err))
	if runCtx.Err() != nil {
		// The worker canceled this attempt (the shutdown timeout ran out): it
		// was interrupted, not failed, whatever error the handler made of
		// the cancellation. Back to the queue at once, never given up, so a
		// deploy cannot delete a job on its last attempt. (A lost lease
		// returned above; a job's own Timeout cancels a context inside Run,
		// not runCtx.)
		w.retry(d, 0, fields)
		return
	}
	if errors.Is(err, ErrInProgress) && !IsPermanent(err) {
		// Another run holds the effect's Once lock. Waiting is not failing:
		// retried past MaxAttempts, bounded by the lock's expiry, so a job
		// whose other run crashed is not buried before the lock frees.
		w.retry(d, policy.Backoff(d.Envelope.Attempt), fields)
		return
	}
	if IsPermanent(err) || d.Envelope.Attempt >= policy.MaxAttempts {
		w.giveUp(d, err, policy, fields)
		return
	}
	w.retry(d, policy.Backoff(d.Envelope.Attempt), fields)
}

// retry releases d to run again after delay.
func (w *Worker) retry(d Delivery, delay time.Duration, fields []zap.Field) {
	fields = append(fields, zap.Duration("retry_in", delay))
	if relErr := w.release(d, time.Now().Add(delay)); relErr != nil {
		w.log.Error("jobs worker: job failed and its release failed; it returns when its lease expires",
			append(fields, zap.NamedError("release_error", relErr))...)
		return
	}
	w.log.Warn("job failed", fields...)
}

// giveUp stops retrying d: a permanent failure or the last allowed attempt.
// It moves to the queue's failed jobs, payload and reason kept for `gombit
// jobs inspect` and `retry`, and is logged as an error without its payload,
// which may hold personal data.
func (w *Worker) giveUp(d Delivery, err error, policy Options, fields []zap.Field) {
	reason := ReasonExhausted
	if IsPermanent(err) {
		reason = ReasonPermanent
	}
	fields = append(fields, zap.String("reason", reason), zap.Int("max_attempts", policy.MaxAttempts))
	failure := Failure{Reason: reason, Kind: Classify(err), Error: err.Error(), At: time.Now()}
	if buryErr := w.bury(d, failure); buryErr != nil {
		w.log.Error("jobs worker: giving up on a job, but setting it aside failed; it returns when its lease expires",
			append(fields, zap.NamedError("bury_error", buryErr))...)
		return
	}
	w.log.Error("job failed for good", append(fields,
		zap.String("inspect", "gombit jobs inspect "+d.Envelope.ID+" --queue "+d.Queue))...)
}

func (w *Worker) bury(d Delivery, f Failure) error {
	ctx, cancel := context.WithTimeout(context.Background(), queueOpTimeout)
	defer cancel()
	return w.queue.Bury(ctx, d, f)
}

// renewLease extends d's lease every Lease/3 until the returned stop is
// called. Losing the lease (another worker took the job) cancels the
// handler: its result would be discarded anyway.
func (w *Worker) renewLease(ctx context.Context, cancelRun context.CancelFunc, d Delivery) (stop func() (lost bool)) {
	done := make(chan struct{})
	// Renewal calls run on their own context, canceled by stop, so a handler
	// that returned never waits out an Extend in flight before its ack.
	renewCtx, cancelRenew := context.WithCancel(context.WithoutCancel(ctx))
	var leaseLost atomic.Bool
	var once sync.Once
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		interval := w.opts.Lease / 3
		if interval < time.Millisecond {
			interval = time.Millisecond
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				opCtx, cancel := context.WithTimeout(renewCtx, queueOpTimeout)
				err := w.queue.Extend(opCtx, d, w.opts.Lease)
				cancel()
				if errors.Is(err, ErrLeaseLost) {
					w.log.Warn("jobs worker: lease lost to another worker; canceling this run",
						zap.String("job_id", d.Envelope.ID), zap.String("job", d.Envelope.Name))
					leaseLost.Store(true)
					cancelRun()
					return
				}
				if errors.Is(err, ErrClosed) || renewCtx.Err() != nil {
					return // stopped, or shutting down; the lease runs out on its own
				}
				if err != nil {
					w.log.Error("jobs worker: renew lease", zap.String("job_id", d.Envelope.ID), zap.Error(err))
				}
			}
		}
	}()
	return func() bool {
		once.Do(func() {
			close(done)
			cancelRenew()
		})
		wg.Wait()
		return leaseLost.Load()
	}
}

func (w *Worker) ack(d Delivery) error {
	ctx, cancel := context.WithTimeout(context.Background(), queueOpTimeout)
	defer cancel()
	return w.queue.Ack(ctx, d)
}

func (w *Worker) release(d Delivery, at time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), queueOpTimeout)
	defer cancel()
	return w.queue.Release(ctx, d, at)
}

// shutdown waits for in-flight jobs up to ShutdownTimeout, then cancels them
// and waits a short grace for their handlers to return.
func (w *Worker) shutdown(inFlight *sync.WaitGroup, cancelJobs context.CancelFunc) error {
	w.log.Info("jobs worker stopping; waiting for in-flight jobs", zap.Duration("timeout", w.opts.ShutdownTimeout))
	done := make(chan struct{})
	go func() {
		inFlight.Wait()
		close(done)
	}()
	select {
	case <-done:
		w.log.Info("jobs worker stopped")
		return nil
	case <-time.After(w.opts.ShutdownTimeout):
	}
	cancelJobs()
	select {
	case <-done:
		w.log.Warn("jobs worker stopped; in-flight jobs were canceled at the shutdown timeout and released")
		return nil
	case <-time.After(shutdownGrace):
		return fmt.Errorf("jobs: worker: in-flight jobs ignored cancellation for %s after the %s shutdown timeout; their leases will expire and they will be delivered again",
			shutdownGrace, w.opts.ShutdownTimeout)
	}
}

// shutdownGrace is ShutdownGrace, variable for tests.
var shutdownGrace = ShutdownGrace
