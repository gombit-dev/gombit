package jobs

import (
	"context"
	"errors"
	"time"
)

// Queue is the driver contract: durable (or in-memory) storage of envelopes
// on named queues, with leased delivery. Application code does not call it;
// it dispatches through a Dispatcher, and a worker consumes through Reserve,
// Ack, and Release.
//
// Delivery is at least once. A reserved job is leased; if the lease expires
// before Ack or Release (the worker crashed or stalled), the job becomes
// available again and its next Reserve counts another attempt.
type Queue interface {
	// Push stores env on queue. It becomes available at at, or now when at
	// is zero or in the past. An ID is unique per queue: pushing one that is
	// already on the queue fails with ErrDuplicateJob.
	Push(ctx context.Context, queue string, env Envelope, at time.Time) error
	// PushUnique is Push under a uniqueness claim: when unique.Key is held
	// on queue it stores nothing and returns a *DuplicateError naming the
	// holder; otherwise it claims the key for unique.TTL and pushes. A claim
	// made UntilDone is released when the job is acked or buried.
	PushUnique(ctx context.Context, queue string, env Envelope, at time.Time, unique UniqueKey) error
	// Reserve leases the next available job from the first of queues that
	// has one, in order. Within a queue the job that became available first
	// goes first: a waiting job since its available-at time, a job whose
	// lease expired since that deadline, push order breaking ties. The
	// returned Delivery's Envelope.Attempt counts this delivery. It does not
	// block: with nothing available it returns ErrNoJob.
	//
	// A stored envelope that no longer decodes is still a delivery: leased,
	// with a nil error and the failure in Delivery.Err. The caller must end
	// that lease, or the job returns on every lease expiry: Bury keeps the
	// stored bytes with the failed jobs (what the worker does); Ack deletes
	// them.
	Reserve(ctx context.Context, queues []string, lease time.Duration) (Delivery, error)
	// Ack removes a delivered job: it is done.
	Ack(ctx context.Context, d Delivery) error
	// Release returns a delivered job to its queue, available at at (a
	// retry). Its attempt count is kept.
	Release(ctx context.Context, d Delivery, at time.Time) error
	// Extend renews a delivery's lease to lease from now, so a job that runs
	// longer than one lease is not delivered to a second worker. A worker
	// calls it periodically while the handler runs.
	Extend(ctx context.Context, d Delivery, lease time.Duration) error

	// Bury moves a delivered job the worker gave up on to the queue's failed
	// jobs, with the original envelope and why, instead of acking it away.
	Bury(ctx context.Context, d Delivery, f Failure) error
	// Failed lists a queue's failed jobs, most recent first, at most limit
	// (all when limit <= 0, read in pages; prefer a limit on a large set).
	// A read in pages is not a snapshot: a job that fails meanwhile can be
	// missing from it (read again from the top to see it).
	Failed(ctx context.Context, queue string, limit int) ([]FailedJob, error)
	// FailedJob returns one failed job, or ErrNotFailed.
	FailedJob(ctx context.Context, queue, id string) (FailedJob, error)
	// RetryFailed puts a failed job back on its queue, available now, with a
	// fresh set of attempts. ErrNotFailed when there is no such failed job.
	// A job dispatched Unique reclaims its key, and is refused with a
	// *DuplicateError while another job holds it.
	RetryFailed(ctx context.Context, queue, id string) error
	// ForgetFailed deletes a failed job. ErrNotFailed when there is none.
	ForgetFailed(ctx context.Context, queue, id string) error
	// PurgeFailed deletes a queue's failed jobs that failed before before (all
	// of them when before is zero) and returns how many.
	PurgeFailed(ctx context.Context, queue string, before time.Time) (int, error)
	// Close releases the driver's resources.
	Close() error
}

// Delivery is a job leased by Reserve.
type Delivery struct {
	Queue    string
	Envelope Envelope
	// Receipt identifies this lease. Ack and Release with a receipt whose
	// job was since reserved again fail with ErrLeaseLost.
	Receipt string
	// Err is non-nil when the stored envelope does not decode (KindDecode).
	// Envelope then holds only the ID and attempt: the job cannot run, but
	// it is leased. Bury it to keep the stored bytes for inspection (Ack
	// would delete them).
	Err error
}

var (
	// ErrNoJob: Reserve found nothing available.
	ErrNoJob = errors.New("jobs: no job available")
	// ErrLeaseLost: the delivery's lease expired and another Reserve took
	// the job, or the job is gone.
	ErrLeaseLost = errors.New("jobs: job lease lost")
	// ErrDuplicateJob: Push of an ID that is already on that queue.
	ErrDuplicateJob = errors.New("jobs: job already queued")
	// ErrInvalidQueue: a queue name outside the job-name alphabet.
	ErrInvalidQueue = errors.New("jobs: invalid queue name")
	// ErrClosed: the queue was closed.
	ErrClosed = errors.New("jobs: queue closed")
)
