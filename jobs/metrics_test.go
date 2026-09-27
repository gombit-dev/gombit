package jobs_test

import (
	"context"
	"errors"
	"strings"
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
		`gombit_jobs_processed_total{job="failing_job",queue="default",result="retried"} 1`,
		`gombit_jobs_processed_total{job="failing_job",queue="default",result="succeeded"} 1`,
		`gombit_jobs_processed_total{job="failing_job",queue="default",result="failed"} 1`,
		// A name no handler knows cannot become a label of its own.
		`gombit_jobs_processed_total{job="unknown",queue="default",result="retried"} 1`,
		`gombit_jobs_processed_total{job="unknown",queue="default",result="failed"} 1`,
		`gombit_jobs_run_seconds_count{job="failing_job",queue="default"} 3`,
		`gombit_jobs_wait_seconds_count{job="failing_job",queue="default"} 3`,
		`gombit_jobs_in_flight{queue="default"} 0`,
		`gombit_jobs_queued{queue="default",state="failed"} 2`,
		`gombit_jobs_oldest_ready_seconds{queue="default"} 0`,
		"# TYPE gombit_jobs_processed_total counter",
		"# TYPE gombit_jobs_run_seconds histogram",
		"# TYPE gombit_jobs_wait_seconds histogram",
		`gombit_jobs_run_seconds_bucket{job="failing_job",queue="default",le="+Inf"} 3`,
		`gombit_jobs_run_seconds_bucket{job="failing_job",queue="default",le="1800"} 3`,
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
