package framework

import (
	"context"

	"github.com/gombit-dev/gombit/jobs"
)

// Envelope metadata keys JobPropagator writes.
const (
	JobMetadataRequestID = "gombit.request_id"
	JobMetadataTraceID   = "gombit.trace_id"
)

// JobPropagator carries the request and trace IDs of the request that
// dispatched a job into the job handler's context, so
// GetRequestIDFromContext / GetTraceIDFromContext (and the logs that use
// them) correlate a job with the request that queued it.
func JobPropagator() jobs.Propagator { return jobPropagator{} }

type jobPropagator struct{}

func (jobPropagator) Inject(ctx context.Context, metadata map[string]string) {
	if id := GetRequestIDFromContext(ctx); id != "" {
		metadata[JobMetadataRequestID] = id
	}
	if id := GetTraceIDFromContext(ctx); id != "" {
		metadata[JobMetadataTraceID] = id
	}
}

func (jobPropagator) Extract(ctx context.Context, metadata map[string]string) context.Context {
	requestID, traceID := metadata[JobMetadataRequestID], metadata[JobMetadataTraceID]
	if requestID == "" && traceID == "" {
		return ctx
	}
	meta := &requestMeta{requestID: requestID, traceID: traceID}
	meta.hdr[0], meta.hdr[1] = requestID, traceID
	return context.WithValue(ctx, requestMetaKey{}, meta)
}
