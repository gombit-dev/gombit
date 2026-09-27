package jobs

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// MemoryQueue is a Queue in process memory: no infrastructure, for
// development, tests, and single-process apps that accept losing queued
// jobs when the process exits. It keeps the same lease and attempt semantics
// as the durable drivers.
type MemoryQueue struct {
	mu      sync.Mutex
	ops     int // claim operations since the last sweep of expired claims
	now     func() time.Time
	jobs    map[memoryKey]*memoryJob
	uniques map[memoryKey]memoryClaim // by (queue, uniqueness key)
	once    map[string]memoryClaim    // by Once key
	seq     uint64
	closed  bool
}

// memoryClaim is a held key: a uniqueness claim (holder = job ID) or a Once
// record (holder = the lock token, or onceDoneMarker).
type memoryClaim struct {
	holder  string
	expires time.Time
}

const onceDoneMarker = "done"

// claimSweepEvery is how many claim operations pass between sweeps of
// expired uniqueness claims and Once records, which are otherwise only
// overwritten, so a long-running process does not keep every key it saw.
const claimSweepEvery = 256

// sweepClaims drops expired claims every claimSweepEvery calls. Callers hold
// q.mu.
func (q *MemoryQueue) sweepClaims(now time.Time) {
	q.ops++
	if q.ops < claimSweepEvery {
		return
	}
	q.ops = 0
	for k, c := range q.uniques {
		if !now.Before(c.expires) {
			delete(q.uniques, k)
		}
	}
	for k, c := range q.once {
		if !now.Before(c.expires) {
			delete(q.once, k)
		}
	}
}

// ClaimCount reports the uniqueness claims and Once records held (tests).
func (q *MemoryQueue) ClaimCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.uniques) + len(q.once)
}

// memoryKey identifies a job: an ID is unique per queue, as on Redis.
type memoryKey struct{ queue, id string }

type memoryJob struct {
	queue       string
	env         Envelope
	attempts    int
	availableAt time.Time
	seq         uint64
	// reserved jobs carry their lease.
	reserved      bool
	receipt       string
	leaseDeadline time.Time
	// failed jobs are out of circulation until retried or forgotten.
	failure *Failure
	raw     []byte
	// unique is the uniqueness key the job holds until it is done ("" when
	// none, or when the claim is a window that outlives the job), claimed
	// for uniqueTTL. heldUnique reports whether the claim is currently held
	// (it is released when the job is buried, and reclaimed on a retry).
	unique     string
	uniqueTTL  time.Duration
	heldUnique bool
}

// MemoryOption configures NewMemoryQueue.
type MemoryOption func(*MemoryQueue)

// WithMemoryClock sets the queue's clock (tests of delays and leases).
func WithMemoryClock(now func() time.Time) MemoryOption {
	return func(q *MemoryQueue) { q.now = now }
}

// NewMemoryQueue returns an empty in-memory queue.
func NewMemoryQueue(opts ...MemoryOption) *MemoryQueue {
	q := &MemoryQueue{now: time.Now, jobs: map[memoryKey]*memoryJob{}, uniques: map[memoryKey]memoryClaim{}, once: map[string]memoryClaim{}}
	for _, opt := range opts {
		opt(q)
	}
	return q
}

// Push implements Queue.
func (q *MemoryQueue) Push(ctx context.Context, queue string, env Envelope, at time.Time) error {
	return q.push(queue, env, at, nil)
}

// PushUnique implements Queue.
func (q *MemoryQueue) PushUnique(_ context.Context, queue string, env Envelope, at time.Time, unique UniqueKey) error {
	if err := unique.validate(); err != nil {
		return err
	}
	return q.push(queue, env, at, &unique)
}

func (q *MemoryQueue) push(queue string, env Envelope, at time.Time, unique *UniqueKey) error {
	if !ValidName(queue) {
		return fmt.Errorf("%w: %q", ErrInvalidQueue, queue)
	}
	if env.ID == "" {
		return fmt.Errorf("jobs: push %q: envelope has no ID", env.Name)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return ErrClosed
	}
	if unique != nil {
		q.sweepClaims(q.now())
		ukey := memoryKey{queue, unique.Key}
		if claim, ok := q.uniques[ukey]; ok && q.now().Before(claim.expires) {
			return &DuplicateError{Queue: queue, Key: unique.Key, HolderID: claim.holder}
		}
	}
	key := memoryKey{queue, env.ID}
	if _, ok := q.jobs[key]; ok {
		return fmt.Errorf("%w: %s on %s", ErrDuplicateJob, env.ID, queue)
	}
	now := q.now()
	if at.IsZero() || at.Before(now) {
		at = now
	}
	q.seq++
	env.Attempt = 0
	job := &memoryJob{queue: queue, env: env, availableAt: at, seq: q.seq}
	if unique != nil {
		q.uniques[memoryKey{queue, unique.Key}] = memoryClaim{holder: env.ID, expires: now.Add(unique.TTL)}
		if unique.UntilDone {
			job.unique, job.uniqueTTL, job.heldUnique = unique.Key, unique.TTL, true
		}
	}
	q.jobs[key] = job
	return nil
}

// releaseUnique drops the uniqueness claim job holds until done, if it still
// holds it.
func (q *MemoryQueue) releaseUnique(job *memoryJob) {
	if !job.heldUnique {
		return
	}
	ukey := memoryKey{job.queue, job.unique}
	if claim, ok := q.uniques[ukey]; ok && claim.holder == job.env.ID {
		delete(q.uniques, ukey)
	}
	job.heldUnique = false
}

// Reserve implements Queue.
func (q *MemoryQueue) Reserve(_ context.Context, queues []string, lease time.Duration) (Delivery, error) {
	if lease <= 0 {
		return Delivery{}, fmt.Errorf("jobs: reserve: lease must be positive, got %s", lease)
	}
	for _, queue := range queues {
		if !ValidName(queue) {
			return Delivery{}, fmt.Errorf("%w: %q", ErrInvalidQueue, queue)
		}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return Delivery{}, ErrClosed
	}
	now := q.now()
	for _, queue := range queues {
		var next *memoryJob
		for _, job := range q.jobs {
			if job.queue != queue || !job.available(now) {
				continue
			}
			if next == nil || job.before(next) {
				next = job
			}
		}
		if next == nil {
			continue
		}
		next.attempts++
		next.reserved = true
		next.receipt = uuid.NewString()
		next.leaseDeadline = now.Add(lease)
		env := next.env
		env.Attempt = next.attempts
		return Delivery{Queue: queue, Envelope: env, Receipt: next.receipt}, nil
	}
	return Delivery{}, ErrNoJob
}

// available: ready and due, or reserved with its lease expired (a crashed
// or stalled worker's job).
func (j *memoryJob) available(now time.Time) bool {
	if j.failure != nil {
		return false
	}
	if j.reserved {
		return !now.Before(j.leaseDeadline)
	}
	return !now.Before(j.availableAt)
}

// before orders available jobs oldest first: by when they became available,
// then by push order.
func (j *memoryJob) before(other *memoryJob) bool {
	a, b := j.readyAt(), other.readyAt()
	if !a.Equal(b) {
		return a.Before(b)
	}
	return j.seq < other.seq
}

func (j *memoryJob) readyAt() time.Time {
	if j.reserved {
		return j.leaseDeadline
	}
	return j.availableAt
}

// Ack implements Queue.
func (q *MemoryQueue) Ack(_ context.Context, d Delivery) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, err := q.leased(d)
	if err != nil {
		return err
	}
	q.releaseUnique(job)
	delete(q.jobs, memoryKey{job.queue, job.env.ID})
	return nil
}

// Release implements Queue.
func (q *MemoryQueue) Release(_ context.Context, d Delivery, at time.Time) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, err := q.leased(d)
	if err != nil {
		return err
	}
	now := q.now()
	if at.IsZero() || at.Before(now) {
		at = now
	}
	job.reserved = false
	job.receipt = ""
	job.availableAt = at
	q.seq++
	job.seq = q.seq
	return nil
}

// Extend implements Queue.
func (q *MemoryQueue) Extend(_ context.Context, d Delivery, lease time.Duration) error {
	if lease <= 0 {
		return fmt.Errorf("jobs: extend: lease must be positive, got %s", lease)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	job, err := q.leased(d)
	if err != nil {
		return err
	}
	job.leaseDeadline = q.now().Add(lease)
	return nil
}

// Bury implements Queue.
func (q *MemoryQueue) Bury(_ context.Context, d Delivery, f Failure) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, err := q.leased(d)
	if err != nil {
		return err
	}
	if f.At.IsZero() {
		f.At = q.now()
	}
	job.reserved, job.receipt = false, ""
	job.failure = &f
	q.releaseUnique(job)
	return nil
}

// BeginOnce implements OnceStore.
func (q *MemoryQueue) BeginOnce(_ context.Context, key, token string, lock time.Duration) (OnceState, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.now()
	q.sweepClaims(now)
	if claim, ok := q.once[key]; ok && now.Before(claim.expires) {
		if claim.holder == onceDoneMarker {
			return OnceDone, nil
		}
		return OnceBusy, nil
	}
	q.once[key] = memoryClaim{holder: token, expires: now.Add(lock)}
	return OnceAcquired, nil
}

// FinishOnce implements OnceStore.
func (q *MemoryQueue) FinishOnce(_ context.Context, key, token string, keep time.Duration) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	// The token holds the lock only while it is unexpired, as BeginOnce
	// sees it (and as the Redis key's TTL enforces).
	if claim, ok := q.once[key]; ok && claim.holder == token && q.now().Before(claim.expires) {
		q.once[key] = memoryClaim{holder: onceDoneMarker, expires: q.now().Add(keep)}
		return nil
	}
	return fmt.Errorf("%w: the lock on %q expired before the effect was recorded", ErrLeaseLost, key)
}

// AbandonOnce implements OnceStore.
func (q *MemoryQueue) AbandonOnce(_ context.Context, key, token string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if claim, ok := q.once[key]; ok && claim.holder == token {
		delete(q.once, key)
	}
	return nil
}

// Failed implements Queue.
func (q *MemoryQueue) Failed(_ context.Context, queue string, limit int) ([]FailedJob, error) {
	if !ValidName(queue) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidQueue, queue)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return nil, ErrClosed
	}
	var out []FailedJob
	for _, job := range q.jobs {
		if job.queue == queue && job.failure != nil {
			out = append(out, job.failedJob())
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Failure.At.Equal(out[j].Failure.At) {
			return out[i].Failure.At.After(out[j].Failure.At)
		}
		// Ties in the same instant: ID descending, as Redis orders them.
		return out[i].Envelope.ID > out[j].Envelope.ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// FailedJob implements Queue.
func (q *MemoryQueue) FailedJob(_ context.Context, queue, id string) (FailedJob, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, err := q.failed(queue, id)
	if err != nil {
		return FailedJob{}, err
	}
	return job.failedJob(), nil
}

// RetryFailed implements Queue.
func (q *MemoryQueue) RetryFailed(_ context.Context, queue, id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, err := q.failed(queue, id)
	if err != nil {
		return err
	}
	if job.unique != "" {
		// Reclaim the key the job gave up when it failed, unless another job
		// has taken it since: running both is what Unique prevents.
		ukey := memoryKey{queue, job.unique}
		now := q.now()
		if claim, ok := q.uniques[ukey]; ok && now.Before(claim.expires) && claim.holder != id {
			return &DuplicateError{Queue: queue, Key: job.unique, HolderID: claim.holder}
		}
		q.uniques[ukey] = memoryClaim{holder: id, expires: now.Add(job.uniqueTTL)}
		job.heldUnique = true
	}
	job.failure = nil
	job.attempts = 0
	job.availableAt = q.now()
	q.seq++
	job.seq = q.seq
	return nil
}

// ForgetFailed implements Queue.
func (q *MemoryQueue) ForgetFailed(_ context.Context, queue, id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, err := q.failed(queue, id); err != nil {
		return err
	}
	delete(q.jobs, memoryKey{queue, id})
	return nil
}

// PurgeFailed implements Queue.
func (q *MemoryQueue) PurgeFailed(_ context.Context, queue string, before time.Time) (int, error) {
	if !ValidName(queue) {
		return 0, fmt.Errorf("%w: %q", ErrInvalidQueue, queue)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return 0, ErrClosed
	}
	n := 0
	for key, job := range q.jobs {
		if job.queue == queue && job.failure != nil && (before.IsZero() || job.failure.At.Before(before)) {
			delete(q.jobs, key)
			n++
		}
	}
	return n, nil
}

func (q *MemoryQueue) failed(queue, id string) (*memoryJob, error) {
	if q.closed {
		return nil, ErrClosed
	}
	job, ok := q.jobs[memoryKey{queue, id}]
	if !ok || job.failure == nil {
		return nil, fmt.Errorf("%w: %s on %s", ErrNotFailed, id, queue)
	}
	return job, nil
}

func (j *memoryJob) failedJob() FailedJob {
	env := j.env
	env.Attempt = j.attempts
	return FailedJob{Queue: j.queue, Envelope: env, RawEnvelope: j.raw, Attempts: j.attempts, Failure: *j.failure}
}

// leased returns the job d holds the current lease of. A lease that expired
// but that no other Reserve has taken yet still counts: the job was not
// delivered twice.
func (q *MemoryQueue) leased(d Delivery) (*memoryJob, error) {
	if q.closed {
		return nil, ErrClosed
	}
	job, ok := q.jobs[memoryKey{d.Queue, d.Envelope.ID}]
	if !ok || !job.reserved || d.Receipt == "" || job.receipt != d.Receipt {
		return nil, fmt.Errorf("%w: %s", ErrLeaseLost, d.Envelope.ID)
	}
	return job, nil
}

// Len returns how many jobs the queue holds, available or not (tests).
func (q *MemoryQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.jobs)
}

// Close implements Queue. Queued jobs are dropped.
func (q *MemoryQueue) Close() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	q.jobs = map[memoryKey]*memoryJob{}
	return nil
}
