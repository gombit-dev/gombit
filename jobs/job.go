package jobs

import (
	"encoding/json"
	"fmt"
	"regexp"
	"time"
)

// Job is a unit of background work: a JSON-serializable struct whose JobName
// returns the stable name it is queued under. JobName must be a constant (it
// must not depend on field values) and must use a value receiver.
type Job interface {
	JobName() string
}

// Versioned is implemented by a job whose payload has changed shape. It
// reports the payload version this binary produces and handles; a job that
// does not implement it is version 1.
//
// Adding an optional field needs no new version: older workers ignore fields
// they do not know, and newer ones see the zero value for fields an older
// producer did not send. Renaming, removing, or retyping a field does: bump
// JobVersion and register an UpgradeFrom step for each older version still in
// a queue.
type Versioned interface {
	JobVersion() int
}

// Envelope is a job as a queue driver stores it: the name and version that
// select the handler, the payload, and the correlation metadata the
// registry's propagators carried from the dispatching context.
type Envelope struct {
	// ID identifies this job across retries. Handlers use it as the
	// idempotency key for their side effects.
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Version int             `json:"version"`
	Payload json.RawMessage `json:"payload"`
	// Metadata carries context across the queue (request and trace IDs).
	// It is not part of the payload and never reaches the handler's job value.
	Metadata   map[string]string `json:"metadata,omitempty"`
	EnqueuedAt time.Time         `json:"enqueued_at"`
	// Attempt is the 1-based delivery attempt, maintained by the driver.
	// Zero means the job has not been delivered yet.
	Attempt int `json:"attempt"`
}

// Marshal encodes the envelope for a driver to store.
func (e Envelope) Marshal() ([]byte, error) {
	data, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("jobs: marshal envelope %s: %w", e.Name, err)
	}
	return data, nil
}

// UnmarshalEnvelope decodes an envelope a driver stored. A malformed
// envelope is a decode failure (KindDecode).
func UnmarshalEnvelope(data []byte) (Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(data, &e); err != nil {
		return Envelope{}, &Error{Kind: KindDecode, Err: fmt.Errorf("envelope: %w", err)}
	}
	if e.Name == "" {
		return Envelope{}, &Error{Kind: KindDecode, Err: fmt.Errorf("envelope has no job name")}
	}
	if e.ID == "" {
		// Handlers use the ID as their idempotency key; an envelope without one
		// would share "" with every other.
		return Envelope{}, &Error{Kind: KindDecode, Name: e.Name, Err: fmt.Errorf("envelope has no job ID")}
	}
	return e, nil
}

// maxNameLen bounds a job name so drivers can use it as a key or label.
const maxNameLen = 128

// namePattern is the job-name alphabet: lower case, digits, and _ . : -
// separators, starting with a letter or digit. Names are stable identifiers
// in queues and metrics, so they stay ASCII and case-insensitive-safe.
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.:-]*$`)

// ValidName reports whether name can identify a job.
func ValidName(name string) bool {
	return len(name) <= maxNameLen && namePattern.MatchString(name)
}
