// Package jobstest tests code that dispatches jobs, without Redis or a
// worker.
//
// A Queue records every job dispatched to it, so a test asserts what was
// queued without running anything:
//
//	q := jobstest.New()
//	jobs.MustRegister(q.Registry(), sendWelcomeEmail)
//	app, _ := framework.New(cfg, framework.WithJobs(q.Dispatcher()))
//	// ... exercise a handler that dispatches ...
//	q.AssertDispatched(t, "send_welcome_email")
//
// and, when the test wants the effects too, runs the queued jobs in the
// test's goroutine with RunAll. Its clock stands still until Advance, so
// delayed jobs stay queued until the test moves time past them. It is a
// real queue: a test of retry policies runs a jobs.Worker over it
// (WithWallClock).
package jobstest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/jobs"
)

// Queue is an in-memory jobs.Queue that records what is dispatched to it.
// It is a *jobs.MemoryQueue underneath, so everything a queue does works
// (reserve, ack, failed jobs, Unique, Once, a real jobs.Worker).
type Queue struct {
	*jobs.MemoryQueue

	registry   *jobs.Registry
	dispatcher *jobs.Dispatcher

	mu         sync.Mutex
	now        time.Time
	wall       bool          // now is time.Now() plus offset
	offset     time.Duration // Advance on a wall clock
	dispatched []Dispatched
	queues     map[string]bool
}

// Dispatched is one job pushed to the queue.
type Dispatched struct {
	Queue    string
	Envelope jobs.Envelope
	// AvailableAt is when the job may run: the dispatch time, or later for
	// Delay and At.
	AvailableAt time.Time
	// Unique is the job's uniqueness key, when it was dispatched Unique or
	// UniqueFor.
	Unique *jobs.UniqueKey
}

// Option configures New.
type Option func(*config)

type config struct {
	registry *jobs.Registry
	regOpts  []jobs.RegistryOption
	now      time.Time
	wall     bool
	opts     []jobs.DispatcherOption
}

// WithRegistry dispatches through registry instead of a new one (to test
// with the application's registry, propagators and all). Its clock is its
// own: build it with jobs.WithClock(q.Now) to stamp envelopes with the
// queue's time.
func WithRegistry(registry *jobs.Registry) Option {
	return func(c *config) { c.registry = registry }
}

// WithRegistryOptions configures the queue's own registry (its clock is the
// queue's), e.g. with the propagators the app's registry has:
//
//	jobstest.New(jobstest.WithRegistryOptions(jobs.WithPropagator(framework.JobPropagator())))
func WithRegistryOptions(opts ...jobs.RegistryOption) Option {
	return func(c *config) { c.regOpts = append(c.regOpts, opts...) }
}

// WithStart sets the clock's starting time (default: the time New was
// called). The clock stands still from there until Advance.
func WithStart(t time.Time) Option {
	return func(c *config) { c.now, c.wall = t, false }
}

// WithWallClock makes the queue's clock the real one (plus whatever Advance
// adds), for a test that runs a jobs.Worker over the queue: a worker
// schedules retries by the wall clock, which a clock standing still never
// reaches.
func WithWallClock() Option {
	return func(c *config) { c.wall = true }
}

// WithDispatcherOptions passes options to the dispatcher (a default queue,
// say). The dispatcher's clock is always the queue's.
func WithDispatcherOptions(opts ...jobs.DispatcherOption) Option {
	return func(c *config) { c.opts = append(c.opts, opts...) }
}

// New returns an empty queue, with a dispatcher over it and a registry.
func New(opts ...Option) *Queue {
	cfg := config{now: time.Now()}
	for _, opt := range opts {
		opt(&cfg)
	}
	q := &Queue{now: cfg.now, wall: cfg.wall, queues: map[string]bool{}}
	q.registry = cfg.registry
	if q.registry == nil {
		q.registry = jobs.NewRegistry(append([]jobs.RegistryOption{jobs.WithClock(q.Now)}, cfg.regOpts...)...)
	}
	q.MemoryQueue = jobs.NewMemoryQueue(jobs.WithMemoryClock(q.Now))
	q.dispatcher = jobs.NewDispatcher(q.registry, q, append(cfg.opts, jobs.WithDispatcherClock(q.Now))...)
	return q
}

// Dispatcher dispatches to this queue: attach it to an app with
// framework.WithJobs, or hand it to the code under test.
func (q *Queue) Dispatcher() *jobs.Dispatcher { return q.dispatcher }

// Registry is the registry the dispatcher encodes with and RunAll runs
// with. Register the jobs under test on it.
func (q *Queue) Registry() *jobs.Registry { return q.registry }

// Now is the queue's time.
func (q *Queue) Now() time.Time {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.nowLocked()
}

func (q *Queue) nowLocked() time.Time {
	if q.wall {
		return time.Now().Add(q.offset)
	}
	return q.now
}

// Advance moves the queue's clock forward by d, making jobs delayed until
// then available to RunAll.
func (q *Queue) Advance(d time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.wall {
		q.offset += d
		return
	}
	q.now = q.now.Add(d)
}

// Push implements jobs.Queue, recording the job once it is queued. Put
// jobs on the queue through q (its dispatcher, or q.Push), not the embedded
// MemoryQueue: RunAll only looks at queues it saw a job pushed to.
func (q *Queue) Push(ctx context.Context, queue string, env jobs.Envelope, at time.Time) error {
	if err := q.MemoryQueue.Push(ctx, queue, env, at); err != nil {
		return err
	}
	q.record(queue, env, at, nil)
	return nil
}

// PushUnique implements jobs.Queue, recording the job once it is queued (a
// job refused as a duplicate is not dispatched).
func (q *Queue) PushUnique(ctx context.Context, queue string, env jobs.Envelope, at time.Time, unique jobs.UniqueKey) error {
	if err := q.MemoryQueue.PushUnique(ctx, queue, env, at, unique); err != nil {
		return err
	}
	q.record(queue, env, at, &unique)
	return nil
}

func (q *Queue) record(queue string, env jobs.Envelope, at time.Time, unique *jobs.UniqueKey) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if now := q.nowLocked(); at.IsZero() || at.Before(now) {
		at = now
	}
	q.dispatched = append(q.dispatched, Dispatched{Queue: queue, Envelope: env, AvailableAt: at, Unique: unique})
	q.queues[queue] = true
}

// Dispatched returns the jobs dispatched so far, in order; only those named
// name when a name is given.
func (q *Queue) Dispatched(name ...string) []Dispatched {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []Dispatched
	for _, d := range q.dispatched {
		if len(name) == 0 || d.Envelope.Name == name[0] {
			out = append(out, d)
		}
	}
	return out
}

// Reset forgets what was dispatched so far. Queued jobs stay queued.
func (q *Queue) Reset() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.dispatched = nil
}

// AssertDispatched fails t unless a job named name was dispatched, and
// returns the last one.
func (q *Queue) AssertDispatched(t testing.TB, name string) Dispatched {
	t.Helper()
	got := q.Dispatched(name)
	if len(got) == 0 {
		t.Fatalf("jobstest: no %q job dispatched; dispatched: %s", name, q.names())
		return Dispatched{}
	}
	return got[len(got)-1]
}

// AssertDispatchedTimes fails t unless exactly n jobs named name were
// dispatched, and returns them.
func (q *Queue) AssertDispatchedTimes(t testing.TB, name string, n int) []Dispatched {
	t.Helper()
	got := q.Dispatched(name)
	if len(got) != n {
		t.Fatalf("jobstest: %q dispatched %d times, want %d; dispatched: %s", name, len(got), n, q.names())
	}
	return got
}

// AssertNotDispatched fails t if a job named name was dispatched.
func (q *Queue) AssertNotDispatched(t testing.TB, name string) {
	t.Helper()
	if got := q.Dispatched(name); len(got) > 0 {
		t.Fatalf("jobstest: %q dispatched %d times, want none", name, len(got))
	}
}

// AssertNothingDispatched fails t if any job was dispatched.
func (q *Queue) AssertNothingDispatched(t testing.TB) {
	t.Helper()
	if got := q.Dispatched(); len(got) > 0 {
		t.Fatalf("jobstest: want no jobs dispatched; dispatched: %s", q.names())
	}
}

// names lists what was dispatched, for failure messages.
func (q *Queue) names() string {
	got := q.Dispatched()
	if len(got) == 0 {
		return "none"
	}
	names := make([]string, len(got))
	for i, d := range got {
		names[i] = d.Envelope.Name
	}
	return strings.Join(names, ", ")
}

// Payload decodes a dispatched job's payload as T, failing t when it is
// another job or does not decode.
func Payload[T jobs.Job](t testing.TB, d Dispatched) T {
	t.Helper()
	var job T
	if name := job.JobName(); d.Envelope.Name != name {
		t.Fatalf("jobstest: job %s is %q, not %q", d.Envelope.ID, d.Envelope.Name, name)
		return job
	}
	if err := json.Unmarshal(d.Envelope.Payload, &job); err != nil {
		t.Fatalf("jobstest: decode %q payload: %v", d.Envelope.Name, err)
	}
	return job
}

// Payloads decodes every dispatched T job, in dispatch order.
func Payloads[T jobs.Job](t testing.TB, q *Queue) []T {
	t.Helper()
	var zero T
	var out []T
	for _, d := range q.Dispatched(zero.JobName()) {
		out = append(out, Payload[T](t, d))
	}
	return out
}

// maxRuns bounds RunAll, so a job that keeps dispatching more fails the
// test instead of hanging it.
var maxRuns = 10000

// RunAll runs every job available now with the registry, until none is
// left: queue by queue in name order, oldest first within a queue. Jobs
// those jobs dispatch run too; jobs delayed past Now stay queued (Advance,
// then RunAll again). It returns how many jobs ran.
//
// Each job gets one attempt, in the caller's goroutine, with a context
// derived from ctx that carries the queue as the Once store. A job that
// succeeds is acknowledged; one that fails is kept with the failed jobs
// (q.Failed) and its error is returned, joined with the others, after the
// rest have run. When ctx ends, RunAll stops: the job it interrupted goes
// back on the queue, as under a worker, and ctx's error is returned. Retry policies and timeouts between attempts are a
// worker's: to test them, run a jobs.Worker over the queue.
func (q *Queue) RunAll(ctx context.Context) (int, error) {
	var errs []error
	ran := 0
	for {
		if err := ctx.Err(); err != nil {
			return ran, errors.Join(append(errs, err)...)
		}
		d, err := q.Reserve(ctx, q.queueNames(), time.Hour)
		if errors.Is(err, jobs.ErrNoJob) {
			return ran, errors.Join(errs...)
		}
		if err != nil {
			return ran, errors.Join(append(errs, err)...)
		}
		if ran == maxRuns {
			_ = q.Release(context.WithoutCancel(ctx), d, time.Time{})
			return ran, errors.Join(append(errs, fmt.Errorf("jobstest: RunAll stopped after %d jobs: a job keeps dispatching more", maxRuns))...)
		}
		ran++
		if d.Err != nil {
			errs = append(errs, fmt.Errorf("jobstest: job %s: %w", d.Envelope.ID, d.Err))
			if err := q.Bury(ctx, d, jobs.Failure{Reason: jobs.ReasonUndecodable, Kind: jobs.Classify(d.Err), Error: d.Err.Error()}); err != nil {
				return ran, errors.Join(append(errs, err)...)
			}
			continue
		}
		runErr := q.registry.Run(jobs.ContextWithOnceStore(ctx, q), d.Envelope)
		if runErr != nil && ctx.Err() != nil {
			// Interrupted, not failed: back on the queue for a later run.
			if err := q.Release(context.WithoutCancel(ctx), d, time.Time{}); err != nil {
				return ran, errors.Join(append(errs, err)...)
			}
			return ran, errors.Join(append(errs, ctx.Err())...)
		}
		if runErr == nil {
			if err := q.Ack(ctx, d); err != nil {
				return ran, errors.Join(append(errs, err)...)
			}
			continue
		}
		errs = append(errs, fmt.Errorf("jobstest: job %s (%s): %w", d.Envelope.ID, d.Envelope.Name, runErr))
		reason := jobs.ReasonExhausted
		if jobs.IsPermanent(runErr) {
			reason = jobs.ReasonPermanent
		}
		if err := q.Bury(ctx, d, jobs.Failure{Reason: reason, Kind: jobs.Classify(runErr), Error: runErr.Error()}); err != nil {
			return ran, errors.Join(append(errs, err)...)
		}
	}
}

// queueNames are the queues jobs were dispatched to, sorted.
func (q *Queue) queueNames() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	names := make([]string, 0, len(q.queues))
	for name := range q.queues {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		names = append(names, q.dispatcher.DefaultQueue())
	}
	return names
}
