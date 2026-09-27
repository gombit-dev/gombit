package framework

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/gombit-dev/gombit/config"
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

func TestAppOpensTheConfiguredJobsDriver(t *testing.T) {
	cfg := config.Default()
	cfg.Jobs.Driver = config.JobsDriverMemory
	app, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	d := app.Jobs()
	if d == nil || d.Driver() != config.JobsDriverMemory {
		t.Fatalf("Jobs() = %+v, want the memory driver", d)
	}
	var gotRequestID string
	jobs.MustRegister(d.Registry(), func(ctx context.Context, _ correlatedJob) error {
		gotRequestID = GetRequestIDFromContext(ctx)
		return nil
	})
	ctx := context.WithValue(context.Background(), requestMetaKey{}, &requestMeta{requestID: "req-9"})
	if _, err := d.Dispatch(ctx, correlatedJob{}); err != nil {
		t.Fatal(err)
	}
	delivery, err := d.Queue().Reserve(context.Background(), []string{d.DefaultQueue()}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Registry().Run(context.Background(), delivery.Envelope); err != nil || gotRequestID != "req-9" {
		t.Fatalf("Run = %v, request %q; want the app registry to carry the request ID", err, gotRequestID)
	}

	if err := app.runStopHooks(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Dispatch(ctx, correlatedJob{}); !errors.Is(err, jobs.ErrClosed) {
		t.Fatalf("Dispatch after shutdown = %v, want the owned queue closed", err)
	}
}

func TestWithJobsIsNotClosedByTheApp(t *testing.T) {
	q := jobs.NewMemoryQueue()
	d := jobs.NewDispatcher(jobs.NewRegistry(), q)
	app, err := New(WithConfig(config.Default()), WithJobs(d))
	if err != nil {
		t.Fatal(err)
	}
	if app.Jobs() != d {
		t.Fatal("WithJobs dispatcher not used")
	}
	if err := app.runStopHooks(); err != nil {
		t.Fatal(err)
	}
	if err := q.Push(context.Background(), "default", jobs.Envelope{ID: "x", Name: "correlated"}, time.Time{}); err != nil {
		t.Fatalf("the app closed a dispatcher it does not own: %v", err)
	}
	if _, err := New(WithConfig(config.Default()), WithJobs(nil)); err == nil {
		t.Fatal("WithJobs(nil) accepted")
	}
}

// TestJobsQueueOnTheAppsRedisClient: with the redis driver, jobs use the
// Redis client the app already has instead of dialing GOMBIT_REDIS_* again.
func TestJobsQueueOnTheAppsRedisClient(t *testing.T) {
	cfg := config.Default()
	cfg.Jobs.Driver = config.JobsDriverRedis
	// Nothing listens here; the error names the address actually dialed.
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond, MaxRetries: -1})
	t.Cleanup(func() { _ = client.Close() })
	app, err := New(WithConfig(cfg), WithRedis(client))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := app.Jobs().Queue().(*jobs.RedisQueue); !ok {
		t.Fatalf("queue = %T, want the Redis driver", app.Jobs().Queue())
	}
	jobs.MustRegister(app.Jobs().Registry(), func(context.Context, correlatedJob) error { return nil })
	_, err = app.Jobs().Dispatch(context.Background(), correlatedJob{})
	if err == nil || !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Fatalf("Dispatch error = %v, want a dial of the attached client's address", err)
	}
	if err := app.runStopHooks(); err != nil {
		t.Fatal(err)
	}
	// Closed with the app, though the client is borrowed.
	if _, err := app.Jobs().Dispatch(context.Background(), correlatedJob{}); !errors.Is(err, jobs.ErrClosed) {
		t.Fatalf("Dispatch after shutdown = %v, want ErrClosed", err)
	}
}
