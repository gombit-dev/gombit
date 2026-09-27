package jobs

import (
	"context"
	"time"
)

// Info describes the job a handler is running. Handlers read it with
// InfoFromContext.
type Info struct {
	ID   string
	Name string
	// Version is the payload version the handler receives (the job's
	// JobVersion); QueuedVersion is the one it was queued at, older when
	// UpgradeFrom steps brought the payload forward.
	Version       int
	QueuedVersion int
	// Attempt is this delivery (1-based) and MaxAttempts the job's limit: a
	// worker gives up on a failure once Attempt >= MaxAttempts. It does not
	// count an attempt it interrupted itself (a shutdown) as a failure, so
	// the job can run again past MaxAttempts; don't treat Attempt ==
	// MaxAttempts as certainly the last run. (The sync driver never retries:
	// its one run is attempt 1.)
	Attempt     int
	MaxAttempts int
	EnqueuedAt  time.Time
}

type infoKey struct{}

// InfoFromContext returns the Info of the job running in ctx.
func InfoFromContext(ctx context.Context) (Info, bool) {
	info, ok := ctx.Value(infoKey{}).(Info)
	return info, ok
}

// Propagator carries values from the dispatching context into the handler's
// context, through Envelope.Metadata: request and trace IDs, so a job's logs
// correlate with the request that queued it. Keys should be namespaced
// ("gombit.request_id") because every propagator shares one map.
type Propagator interface {
	// Inject copies what ctx carries into metadata.
	Inject(ctx context.Context, metadata map[string]string)
	// Extract returns ctx with what metadata carries.
	Extract(ctx context.Context, metadata map[string]string) context.Context
}
