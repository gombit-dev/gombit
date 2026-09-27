package jobstest_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/jobs"
	"github.com/gombit-dev/gombit/jobs/jobstest"
)

type welcomeEmail struct {
	UserID int `json:"user_id"`
}

func (welcomeEmail) JobName() string { return "send_welcome_email" }

type receipt struct {
	OrderID string `json:"order_id"`
}

func (receipt) JobName() string { return "send_receipt" }

// fakeT records a Fatalf instead of ending the test, so assertions can be
// tested failing.
type fakeT struct {
	testing.TB
	failed string
}

func (f *fakeT) Helper() {}
func (f *fakeT) Fatalf(format string, args ...any) {
	f.failed = fmt.Sprintf(format, args...)
}

func TestAssertDispatchedDoesNotRunTheJob(t *testing.T) {
	q := jobstest.New()
	var ran atomic.Int32
	jobs.MustRegister(q.Registry(), func(context.Context, welcomeEmail) error { ran.Add(1); return nil })
	jobs.MustRegister(q.Registry(), func(context.Context, receipt) error { return nil })
	ctx := context.Background()

	q.AssertNothingDispatched(t)
	for _, id := range []int{7, 8} {
		if _, err := q.Dispatcher().Dispatch(ctx, welcomeEmail{UserID: id}); err != nil {
			t.Fatal(err)
		}
	}
	d := q.AssertDispatched(t, "send_welcome_email")
	if got := jobstest.Payload[welcomeEmail](t, d); got.UserID != 8 {
		t.Fatalf("AssertDispatched returned %+v, want the last dispatch", got)
	}
	if d.Queue != "default" || d.Unique != nil || !d.AvailableAt.Equal(q.Now()) || d.Envelope.ID == "" {
		t.Fatalf("Dispatched = %+v", d)
	}
	q.AssertDispatchedTimes(t, "send_welcome_email", 2)
	q.AssertNotDispatched(t, "send_receipt")
	if got := jobstest.Payloads[welcomeEmail](t, q); len(got) != 2 || got[0].UserID != 7 {
		t.Fatalf("Payloads = %+v", got)
	}
	if ran.Load() != 0 {
		t.Fatal("dispatching ran the job")
	}
	if n := q.Len(); n != 2 {
		t.Fatalf("queue holds %d jobs, want 2", n)
	}

	q.Reset()
	q.AssertNothingDispatched(t)
	if n := q.Len(); n != 2 {
		t.Fatalf("Reset dropped queued jobs: %d left", n)
	}
}

func TestAssertionsFail(t *testing.T) {
	q := jobstest.New()
	jobs.MustRegister(q.Registry(), func(context.Context, welcomeEmail) error { return nil })
	jobs.MustRegister(q.Registry(), func(context.Context, receipt) error { return nil })

	ft := &fakeT{}
	q.AssertDispatched(ft, "send_welcome_email")
	if !strings.Contains(ft.failed, `no "send_welcome_email" job dispatched; dispatched: none`) {
		t.Fatalf("AssertDispatched on nothing: %q", ft.failed)
	}
	if _, err := q.Dispatcher().Dispatch(context.Background(), receipt{OrderID: "o1"}); err != nil {
		t.Fatal(err)
	}
	for name, check := range map[string]func(*fakeT){
		`no "send_welcome_email" job dispatched; dispatched: send_receipt`: func(ft *fakeT) { q.AssertDispatched(ft, "send_welcome_email") },
		`"send_receipt" dispatched 1 times, want 2`:                        func(ft *fakeT) { q.AssertDispatchedTimes(ft, "send_receipt", 2) },
		`"send_receipt" dispatched 1 times, want none`:                     func(ft *fakeT) { q.AssertNotDispatched(ft, "send_receipt") },
		`want no jobs dispatched; dispatched: send_receipt`:                func(ft *fakeT) { q.AssertNothingDispatched(ft) },
		`is "send_receipt", not "send_welcome_email"`: func(ft *fakeT) {
			jobstest.Payload[welcomeEmail](ft, q.Dispatched()[0])
		},
	} {
		ft := &fakeT{}
		check(ft)
		if !strings.Contains(ft.failed, name) {
			t.Errorf("failure %q, want it to say %q", ft.failed, name)
		}
	}
}

func TestUniqueDuplicateIsNotRecorded(t *testing.T) {
	q := jobstest.New()
	jobs.MustRegister(q.Registry(), func(context.Context, receipt) error { return nil })
	ctx := context.Background()
	if _, err := q.Dispatcher().Dispatch(ctx, receipt{OrderID: "o1"}, jobs.Unique("receipt:o1", time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Dispatcher().Dispatch(ctx, receipt{OrderID: "o1"}, jobs.Unique("receipt:o1", time.Hour)); !errors.Is(err, jobs.ErrDuplicateDispatch) {
		t.Fatalf("second Unique dispatch = %v", err)
	}
	d := q.AssertDispatchedTimes(t, "send_receipt", 1)[0]
	if d.Unique == nil || d.Unique.Key != "receipt:o1" {
		t.Fatalf("Unique = %+v", d.Unique)
	}
}

func TestRunAll(t *testing.T) {
	q := jobstest.New(jobstest.WithStart(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)))
	var sent []int
	var receipts atomic.Int32
	jobs.MustRegister(q.Registry(), func(ctx context.Context, job welcomeEmail) error {
		if job.UserID < 0 {
			return jobs.Permanent(errors.New("no such user"))
		}
		sent = append(sent, job.UserID)
		// A job that dispatches another, which RunAll runs too.
		_, err := q.Dispatcher().Dispatch(ctx, receipt{OrderID: fmt.Sprint(job.UserID)}, jobs.OnQueue("mail"))
		return err
	})
	jobs.MustRegister(q.Registry(), func(ctx context.Context, job receipt) error {
		// Once is backed by the queue: a second receipt for the same order
		// does not send twice.
		return jobs.Once(ctx, "receipt:"+job.OrderID, func(context.Context) error { receipts.Add(1); return nil })
	})
	ctx := context.Background()
	d := q.Dispatcher()
	for _, job := range []jobs.Job{welcomeEmail{UserID: 1}, welcomeEmail{UserID: 2}, receipt{OrderID: "1"}} {
		if _, err := d.Dispatch(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Dispatch(ctx, welcomeEmail{UserID: 3}, jobs.Delay(time.Minute)); err != nil {
		t.Fatal(err)
	}

	ran, err := q.RunAll(ctx)
	if err != nil || ran != 5 {
		t.Fatalf("RunAll = %d, %v; want the 3 ready jobs and the 2 receipts they queued", ran, err)
	}
	if fmt.Sprint(sent) != "[1 2]" || receipts.Load() != 2 {
		t.Fatalf("sent %v and %d receipts; want [1 2] and one receipt per order", sent, receipts.Load())
	}
	if ran, err := q.RunAll(ctx); ran != 0 || err != nil {
		t.Fatalf("RunAll before the delay = %d, %v", ran, err)
	}

	q.Advance(time.Minute)
	if ran, err := q.RunAll(ctx); ran != 2 || err != nil {
		t.Fatalf("RunAll after the delay = %d, %v", ran, err)
	}
	if fmt.Sprint(sent) != "[1 2 3]" {
		t.Fatalf("sent %v after Advance", sent)
	}

	// A failing job is kept with the failed jobs and its error returned,
	// after the other jobs ran.
	for _, id := range []int{-1, 4} {
		if _, err := d.Dispatch(ctx, welcomeEmail{UserID: id}); err != nil {
			t.Fatal(err)
		}
	}
	ran, err = q.RunAll(ctx)
	if ran != 3 || !jobs.IsPermanent(err) || !strings.Contains(err.Error(), "no such user") {
		t.Fatalf("RunAll with a failing job = %d, %v", ran, err)
	}
	failed, err := q.Failed(ctx, "default", 0)
	if err != nil || len(failed) != 1 || failed[0].Failure.Reason != jobs.ReasonPermanent || failed[0].Attempts != 1 {
		t.Fatalf("failed jobs = %+v, %v", failed, err)
	}
	if q.Len() != 1 { // only the failed job remains
		t.Fatalf("queue holds %d jobs", q.Len())
	}
}

type loop struct{}

func (loop) JobName() string { return "loop" }

func TestRunAllStopsARunawayJob(t *testing.T) {
	q := jobstest.New()
	jobs.MustRegister(q.Registry(), func(ctx context.Context, _ loop) error {
		_, err := q.Dispatcher().Dispatch(ctx, loop{})
		return err
	})
	if _, err := q.Dispatcher().Dispatch(context.Background(), loop{}); err != nil {
		t.Fatal(err)
	}
	defer jobstest.SetMaxRuns(5)()
	ran, err := q.RunAll(context.Background())
	if ran != 5 || err == nil || !strings.Contains(err.Error(), "keeps dispatching") {
		t.Fatalf("RunAll on a job that re-dispatches itself = %d, %v", ran, err)
	}
	if q.Len() != 1 {
		t.Fatalf("the job RunAll stopped at was not left queued: %d jobs", q.Len())
	}
}

// TestRunAllRunsExactlyTheBound: as many jobs as the bound is not a runaway.
func TestRunAllRunsExactlyTheBound(t *testing.T) {
	q := jobstest.New()
	jobs.MustRegister(q.Registry(), func(context.Context, receipt) error { return nil })
	defer jobstest.SetMaxRuns(3)()
	for i := 0; i < 3; i++ {
		if _, err := q.Dispatcher().Dispatch(context.Background(), receipt{OrderID: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if ran, err := q.RunAll(context.Background()); ran != 3 || err != nil {
		t.Fatalf("RunAll over exactly the bound = %d, %v", ran, err)
	}
}

// TestRunAllStopsWhenCanceled: jobs interrupted by the caller's context go
// back on the queue; they are not failed.
func TestRunAllStopsWhenCanceled(t *testing.T) {
	q := jobstest.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	jobs.MustRegister(q.Registry(), func(ctx context.Context, _ receipt) error {
		cancel()
		return ctx.Err()
	})
	for i := 0; i < 3; i++ {
		if _, err := q.Dispatcher().Dispatch(context.Background(), receipt{OrderID: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	ran, err := q.RunAll(ctx)
	if ran != 1 || !errors.Is(err, context.Canceled) {
		t.Fatalf("RunAll canceled by its job = %d, %v", ran, err)
	}
	failed, _ := q.Failed(context.Background(), "default", 0)
	if q.Len() != 3 || len(failed) != 0 {
		t.Fatalf("after cancel: %d queued, %d failed; want all 3 queued", q.Len(), len(failed))
	}
	if ran, err := q.RunAll(ctx); ran != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("RunAll on a canceled context = %d, %v", ran, err)
	}
}

type tenantKey struct{}

type tenantPropagator struct{}

func (tenantPropagator) Inject(ctx context.Context, md map[string]string) {
	if v, ok := ctx.Value(tenantKey{}).(string); ok {
		md["tenant"] = v
	}
}

func (tenantPropagator) Extract(ctx context.Context, md map[string]string) context.Context {
	return context.WithValue(ctx, tenantKey{}, md["tenant"])
}

// TestRegistryOptions: the queue's registry takes the app's propagators.
func TestRegistryOptions(t *testing.T) {
	q := jobstest.New(jobstest.WithRegistryOptions(jobs.WithPropagator(tenantPropagator{})))
	var seen string
	jobs.MustRegister(q.Registry(), func(ctx context.Context, _ receipt) error {
		seen, _ = ctx.Value(tenantKey{}).(string)
		return nil
	})
	ctx := context.WithValue(context.Background(), tenantKey{}, "acme")
	if _, err := q.Dispatcher().Dispatch(ctx, receipt{OrderID: "o1"}); err != nil {
		t.Fatal(err)
	}
	if d := q.AssertDispatched(t, "send_receipt"); d.Envelope.Metadata["tenant"] != "acme" || !d.Envelope.EnqueuedAt.Equal(q.Now()) {
		t.Fatalf("envelope = %+v", d.Envelope)
	}
	if _, err := q.RunAll(context.Background()); err != nil || seen != "acme" {
		t.Fatalf("RunAll = %v; handler saw tenant %q", err, seen)
	}
}

func TestRunAllOnAnEmptyQueue(t *testing.T) {
	q := jobstest.New()
	if ran, err := q.RunAll(context.Background()); ran != 0 || err != nil {
		t.Fatalf("RunAll = %d, %v", ran, err)
	}
}

// TestWorkerOverTheQueue: the queue is a real one, so a jobs.Worker runs it
// with retries, for tests of the retry policy (on the wall clock, which the
// worker schedules retries by).
func TestWorkerOverTheQueue(t *testing.T) {
	q := jobstest.New(jobstest.WithWallClock())
	var attempts atomic.Int32
	done := make(chan struct{})
	jobs.MustRegister(q.Registry(), func(ctx context.Context, _ receipt) error {
		if attempts.Add(1) == 1 {
			return errors.New("flaky")
		}
		close(done)
		return nil
	}, jobs.WithOptions(jobs.Options{Backoff: jobs.Constant(0)}))
	if _, err := q.Dispatcher().Dispatch(context.Background(), receipt{OrderID: "o1"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w, err := jobs.NewWorker(q.Registry(), q, jobs.WorkerOptions{Queues: []string{"default"}, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = w.Run(ctx) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker did not retry the job")
	}
	q.AssertDispatchedTimes(t, "send_receipt", 1) // a retry is not a dispatch
}

func TestAdvanceOnAWallClock(t *testing.T) {
	q := jobstest.New(jobstest.WithWallClock())
	jobs.MustRegister(q.Registry(), func(context.Context, receipt) error { return nil })
	ctx := context.Background()
	if _, err := q.Dispatcher().Dispatch(ctx, receipt{OrderID: "o1"}, jobs.Delay(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if ran, err := q.RunAll(ctx); ran != 0 || err != nil {
		t.Fatalf("RunAll before the delay = %d, %v", ran, err)
	}
	q.Advance(time.Hour)
	if ran, err := q.RunAll(ctx); ran != 1 || err != nil {
		t.Fatalf("RunAll after Advance = %d, %v", ran, err)
	}
}
