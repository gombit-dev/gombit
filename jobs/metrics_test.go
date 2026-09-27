package jobs_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/jobs"
)

func TestWorkerRecordsMetrics(t *testing.T) {
	reg := jobs.NewRegistry(jobs.WithDefaultOptions(jobs.Options{MaxAttempts: 2, Backoff: jobs.Constant(5 * time.Millisecond)}))
	jobs.MustRegister(reg, func(ctx context.Context, job failingJob) error {
		info, _ := jobs.InfoFromContext(ctx)
		if job.Permanent {
			return jobs.Permanent(errors.New("gone"))
		}
		if info.Attempt == 1 {
			return errors.New("flaky")
		}
		return nil
	}, jobs.WithOptions(jobs.Options{Backoff: jobs.Constant(5 * time.Millisecond)}))
	q := jobs.NewMemoryQueue()
	d := jobs.NewDispatcher(reg, q)
	ctx := context.Background()
	for _, j := range []failingJob{{}, {Permanent: true}} {
		if _, err := d.Dispatch(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.Push(ctx, "default", jobs.Envelope{ID: "u", Name: "not_registered_anywhere", Version: 1, Payload: []byte(`{}`)}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	metrics := jobs.NewMetrics()
	w, err := jobs.NewWorker(reg, q, jobs.WorkerOptions{Queues: []string{"default"}, PollInterval: 5 * time.Millisecond, Metrics: metrics})
	if err != nil {
		t.Fatal(err)
	}
	if w.Metrics() != metrics {
		t.Fatal("Worker.Metrics is not the one given")
	}
	startWorker(t, w)
	eventually(t, "the jobs to settle", func() bool {
		st, _ := q.Stats(ctx, "default")
		return st.Ready == 0 && st.Reserved == 0 && st.Scheduled == 0 && st.Failed == 2
	})
	var out strings.Builder
	st, _ := q.Stats(ctx, "default")
	if err := metrics.WritePrometheus(&out, map[string]jobs.QueueStats{"default": st}, time.Now()); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{
		`gombit_jobs_processed_total{job_name="failing_job",queue="default",result="retried"} 1`,
		`gombit_jobs_processed_total{job_name="failing_job",queue="default",result="succeeded"} 1`,
		`gombit_jobs_processed_total{job_name="failing_job",queue="default",result="failed"} 1`,
		// A name no handler knows cannot become a label of its own.
		`gombit_jobs_processed_total{job_name="unknown",queue="default",result="retried"} 1`,
		`gombit_jobs_processed_total{job_name="unknown",queue="default",result="failed"} 1`,
		`gombit_jobs_run_seconds_count{job_name="failing_job",queue="default"} 3`,
		`gombit_jobs_wait_seconds_count{job_name="failing_job",queue="default"} 3`,
		`gombit_jobs_in_flight{queue="default"} 0`,
		`gombit_jobs_queued{queue="default",state="failed"} 2`,
		`gombit_jobs_oldest_ready_seconds{queue="default"} 0`,
		"# TYPE gombit_jobs_processed_total counter",
		"# TYPE gombit_jobs_run_seconds histogram",
		"# TYPE gombit_jobs_wait_seconds histogram",
		`gombit_jobs_run_seconds_bucket{job_name="failing_job",queue="default",le="+Inf"} 3`,
		`gombit_jobs_run_seconds_bucket{job_name="failing_job",queue="default",le="1800"} 3`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics missing %s:\n%s", want, text)
		}
	}
	if strings.Contains(text, "not_registered_anywhere") {
		t.Fatalf("an unregistered job name became a label:\n%s", text)
	}
}

func TestEmptyMetrics(t *testing.T) {
	m := jobs.NewMetrics()
	if !m.Empty() {
		t.Fatal("new metrics are not empty")
	}
	var out strings.Builder
	if err := m.WritePrometheus(&out, nil, time.Now()); err != nil || out.Len() != 0 {
		t.Fatalf("empty metrics wrote %q, %v", out.String(), err)
	}
}

// refusingQueue fails every ack, release, and bury, as Redis does in an
// outage after a job was reserved.
type refusingQueue struct {
	*jobs.MemoryQueue
	calls atomic.Int32
}

var errRefused = errors.New("redis: connection reset")

func (q *refusingQueue) Ack(context.Context, jobs.Delivery) error { q.calls.Add(1); return errRefused }
func (q *refusingQueue) Release(context.Context, jobs.Delivery, time.Time) error {
	q.calls.Add(1)
	return errRefused
}
func (q *refusingQueue) Bury(context.Context, jobs.Delivery, jobs.Failure) error {
	q.calls.Add(1)
	return errRefused
}

// TestMetricsCountOnlyWhatTheQueueCommitted: an outcome is recorded once
// the queue took it. A job whose ack, release, or bury failed is still
// leased: unsettled, not succeeded, retried, or failed.
func TestMetricsCountOnlyWhatTheQueueCommitted(t *testing.T) {
	reg := jobs.NewRegistry(jobs.WithDefaultOptions(jobs.Options{MaxAttempts: 2}))
	jobs.MustRegister(reg, func(ctx context.Context, job failingJob) error {
		if job.Permanent {
			return jobs.Permanent(errors.New("gone"))
		}
		return errors.New("flaky")
	})
	jobs.MustRegister(reg, func(context.Context, sendWelcome) error { return nil })
	q := &refusingQueue{MemoryQueue: jobs.NewMemoryQueue()}
	d := jobs.NewDispatcher(reg, q.MemoryQueue)
	ctx := context.Background()
	for _, j := range []jobs.Job{failingJob{}, failingJob{Permanent: true}, sendWelcome{UserID: 1}} {
		if _, err := d.Dispatch(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	metrics := jobs.NewMetrics()
	w, err := jobs.NewWorker(reg, q, jobs.WorkerOptions{Queues: []string{"default"}, PollInterval: 5 * time.Millisecond, Lease: time.Hour, Metrics: metrics})
	if err != nil {
		t.Fatal(err)
	}
	startWorker(t, w)
	eventually(t, "every outcome refused", func() bool { return q.calls.Load() == 3 })
	var out strings.Builder
	eventually(t, "the outcomes recorded", func() bool {
		out.Reset()
		_ = metrics.WritePrometheus(&out, nil, time.Now())
		return strings.Contains(out.String(), `job_name="send_welcome_email",queue="default",result="unsettled"} 1`)
	})
	for _, want := range []string{
		`gombit_jobs_processed_total{job_name="failing_job",queue="default",result="unsettled"} 2`,
		`gombit_jobs_processed_total{job_name="send_welcome_email",queue="default",result="unsettled"} 1`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("metrics missing %s:\n%s", want, out.String())
		}
	}
	for _, never := range []string{`result="succeeded"`, `result="retried"`, `result="failed"`} {
		if strings.Contains(out.String(), never) {
			t.Errorf("metrics count %s for an outcome the queue refused:\n%s", never, out.String())
		}
	}
}

// TestAnUndecodableJobIsCountedOnceSetAside: a poison envelope that could
// not be buried is unsettled, not undecodable (set aside).
func TestAnUndecodableJobIsCountedOnceSetAside(t *testing.T) {
	for _, buryFails := range []bool{false, true} {
		q := &poisonQueue{MemoryQueue: jobs.NewMemoryQueue(), buryFails: buryFails}
		metrics := jobs.NewMetrics()
		w, err := jobs.NewWorker(jobs.NewRegistry(), q, jobs.WorkerOptions{Queues: []string{"default"}, PollInterval: 5 * time.Millisecond, Metrics: metrics})
		if err != nil {
			t.Fatal(err)
		}
		stop := startWorker(t, w)
		want := `gombit_jobs_processed_total{job_name="unknown",queue="default",result="undecodable"} 1`
		if buryFails {
			want = `gombit_jobs_processed_total{job_name="unknown",queue="default",result="unsettled"} 1`
		}
		var out strings.Builder
		eventually(t, "the poison envelope counted", func() bool {
			out.Reset()
			_ = metrics.WritePrometheus(&out, nil, time.Now())
			return strings.Contains(out.String(), want)
		})
		_ = stop()
	}
}
