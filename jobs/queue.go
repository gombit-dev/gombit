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
	// Reserve leases the next available job from the first of queues that
	// has one, in order. Within a queue the job that became available first
	// goes first: a waiting job since its available-at time, a job whose
	// lease expired since that deadline, push order breaking ties. The
	// returned Delivery's Envelope.Attempt counts this delivery. It does not
	// block: with nothing available it returns ErrNoJob.
	//
	// A stored envelope that no longer decodes is still a delivery: leased,
	// with a nil error and the failure in Delivery.Err. The caller must Ack
	// it, or it returns on every lease expiry.
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
	// it is leased, so the caller must Ack it.
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
