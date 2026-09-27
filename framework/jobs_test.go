package framework

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/gombit-dev/gombit/jobs"
)

type correlatedJob struct{}

func (correlatedJob) JobName() string { return "correlated" }

// TestJobPropagatorCarriesRequestCorrelation dispatches a job from inside a
// real request and runs it on a bare context, as a worker would: the handler
// sees the dispatching request's IDs.
func TestJobPropagatorCarriesRequestCorrelation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reg := jobs.NewRegistry(jobs.WithPropagator(JobPropagator()))
	var gotRequestID, gotTraceID string
	jobs.MustRegister(reg, func(ctx context.Context, _ correlatedJob) error {
		gotRequestID, gotTraceID = GetRequestIDFromContext(ctx), GetTraceIDFromContext(ctx)
		return nil
	})

	var env jobs.Envelope
	router := gin.New()
	router.Use(requestContextMiddleware(0))
	router.POST("/signup", func(c *gin.Context) {
		var err error
		env, err = reg.Encode(c.Request.Context(), correlatedJob{})
		if err != nil {
			t.Error(err)
		}
		c.Status(http.StatusAccepted)
	})
	req := httptest.NewRequest(http.MethodPost, "/signup", nil)
	req.Header.Set(RequestIDHeader, "req-123")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	traceID := rec.Header().Get(TraceIDHeader)

	if env.Metadata[JobMetadataRequestID] != "req-123" || env.Metadata[JobMetadataTraceID] != traceID || traceID == "" {
		t.Fatalf("metadata = %v, want request req-123 and trace %q", env.Metadata, traceID)
	}
	if err := reg.Run(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if gotRequestID != "req-123" || gotTraceID != traceID {
		t.Fatalf("handler saw request %q trace %q, want req-123 and %q", gotRequestID, gotTraceID, traceID)
	}

	// Dispatched outside a request: nothing to carry, nothing invented.
	env, err := reg.Encode(context.Background(), correlatedJob{})
	if err != nil || env.Metadata != nil {
		t.Fatalf("Encode(bare) metadata = %v, err = %v", env.Metadata, err)
	}
	gotRequestID, gotTraceID = "x", "x"
	if err := reg.Run(context.Background(), env); err != nil || gotRequestID != "" || gotTraceID != "" {
		t.Fatalf("Run(bare) saw request %q trace %q, err %v", gotRequestID, gotTraceID, err)
	}
}
