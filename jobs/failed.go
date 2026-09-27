package jobs

import (
	"errors"
	"time"
)

// Failure is why a worker gave up on a job, recorded with it by Queue.Bury.
type Failure struct {
	// Reason is the worker's decision: ReasonExhausted, ReasonPermanent, or
	// ReasonUndecodable.
	Reason string `json:"reason"`
	// Kind is the classified failure of the last attempt.
	Kind Kind `json:"kind"`
	// Error is the last attempt's error message.
	Error string `json:"error"`
	// At is when the worker gave up.
	At time.Time `json:"at"`
}

// Reasons a worker gives up on a job.
const (
	ReasonExhausted   = "attempts exhausted"
	ReasonPermanent   = "permanent failure"
	ReasonUndecodable = "undecodable envelope"
)

// FailedJob is a job a worker gave up on, kept by its queue until it is
// retried, forgotten, or purged.
type FailedJob struct {
	Queue string `json:"queue"`
	// Envelope is the job as it was queued, payload included. For an
	// envelope that no longer decodes it holds only the ID, and RawEnvelope
	// the stored bytes.
	Envelope    Envelope `json:"envelope"`
	RawEnvelope []byte   `json:"raw_envelope,omitempty"`
	// Attempts is how many times it was delivered.
	Attempts int     `json:"attempts"`
	Failure  Failure `json:"failure"`
}

// ErrNotFailed: no failed job with that ID on that queue.
var ErrNotFailed = errors.New("jobs: no such failed job")
