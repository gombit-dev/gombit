package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/jobs"
)

// jobsFixture stubs the config and queue behind `gombit jobs` with a memory
// queue holding two failed jobs and one queued job on "mail".
func jobsFixture(t *testing.T) *jobs.MemoryQueue {
	t.Helper()
	q := jobs.NewMemoryQueue()
	ctx := context.Background()
	at := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	for i, id := range []string{"job-a", "job-b", "job-c"} {
		env := jobs.Envelope{ID: id, Name: "send_welcome_email", Version: 1, Payload: json.RawMessage(`{"user_id":7}`), EnqueuedAt: at}
		if err := q.Push(ctx, "mail", env, time.Time{}); err != nil {
			t.Fatal(err)
		}
		if i == 2 {
			continue
		}
		d, err := q.Reserve(ctx, []string{"mail"}, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err := q.Bury(ctx, d, jobs.Failure{Reason: jobs.ReasonExhausted, Kind: jobs.KindHandler, Error: "smtp: 550 mailbox unavailable", At: at.Add(time.Duration(i) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Default()
	cfg.Jobs.Driver = config.JobsDriverRedis
	cfg.Jobs.Queue = "mail"
	prevCfg, prevOpen := LoadConfig, openJobsQueue
	t.Cleanup(func() { LoadConfig, openJobsQueue = prevCfg, prevOpen })
	LoadConfig = func() (config.Config, error) { return cfg, nil }
	openJobsQueue = func(config.Config) (jobs.Queue, func() error, error) { return q, func() error { return nil }, nil }
	return q
}

func runJobs(t *testing.T, args ...string) (string, error) {
	t.Helper()
	stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
	err := ExecuteRoot(context.Background(), NewRoot(stdout, stderr), append([]string{"jobs"}, args...))
	return stdout.String() + stderr.String(), err
}

func TestJobsFailedAndInspect(t *testing.T) {
	jobsFixture(t)
	out, err := runJobs(t, "failed")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[1], "job-b") || !strings.HasPrefix(lines[2], "job-a") ||
		!strings.Contains(out, "attempts exhausted") || !strings.Contains(out, "smtp: 550") {
		t.Fatalf("jobs failed:\n%s", out)
	}
	out, err = runJobs(t, "failed", "--json", "--limit", "1")
	var listed []jobs.FailedJob
	if err != nil || json.Unmarshal([]byte(out), &listed) != nil || len(listed) != 1 || listed[0].Envelope.ID != "job-b" {
		t.Fatalf("jobs failed --json --limit 1 = %v:\n%s", err, out)
	}

	out, err = runJobs(t, "inspect", "job-a")
	if err != nil || !strings.Contains(out, `"user_id": 7`) || !strings.Contains(out, "Reason:      attempts exhausted (handler)") || !strings.Contains(out, "Attempts:    1") {
		t.Fatalf("jobs inspect = %v:\n%s", err, out)
	}
	if _, err := runJobs(t, "inspect", "job-c"); err == nil || !strings.Contains(err.Error(), "no such failed job") {
		t.Fatalf("jobs inspect (a queued job) = %v, want not failed", err)
	}
	if out, err := runJobs(t, "failed", "--queue", "other"); err != nil || !strings.Contains(out, "No failed jobs on other.") {
		t.Fatalf("jobs failed --queue other = %v:\n%s", err, out)
	}
}

func TestJobsRetryForgetPurge(t *testing.T) {
	q := jobsFixture(t)
	ctx := context.Background()
	if _, err := runJobs(t, "retry"); err == nil {
		t.Fatal("jobs retry with neither IDs nor --all accepted")
	}
	if out, err := runJobs(t, "retry", "job-a"); err != nil || !strings.Contains(out, "Retrying job-a on mail.") {
		t.Fatalf("jobs retry = %v:\n%s", err, out)
	}
	if left, _ := q.Failed(ctx, "mail", 0); len(left) != 1 || left[0].Envelope.ID != "job-b" {
		t.Fatalf("failed after retry: %+v", left)
	}
	if out, err := runJobs(t, "forget", "job-b"); err != nil || !strings.Contains(out, "Forgot job-b") {
		t.Fatalf("jobs forget = %v:\n%s", err, out)
	}
	if _, err := runJobs(t, "forget", "job-b"); err == nil {
		t.Fatal("forgetting twice succeeded")
	}
	if _, err := runJobs(t, "purge"); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("jobs purge without --force = %v", err)
	}
	if out, err := runJobs(t, "purge", "--force"); err != nil || !strings.Contains(out, "Purged 0 failed jobs from mail.") {
		t.Fatalf("jobs purge = %v:\n%s", err, out)
	}
	// Two jobs are queued now: the retried job-a and job-c.
	if q.Len() != 2 {
		t.Fatalf("queue holds %d jobs, want the retried one and the untouched one", q.Len())
	}
}

func TestJobsRefusesDriversWithoutKeptJobs(t *testing.T) {
	for driver, want := range map[config.JobsDriver]string{config.JobsDriverSync: "none are kept", config.JobsDriverMemory: "out of reach"} {
		cfg := config.Default()
		cfg.Jobs.Driver = driver
		prev := LoadConfig
		LoadConfig = func() (config.Config, error) { return cfg, nil }
		_, err := runJobs(t, "failed")
		LoadConfig = prev
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("jobs failed with %s = %v, want a refusal (%s)", driver, err, want)
		}
	}
}

func TestTruncateKeepsRunesWhole(t *testing.T) {
	if got := truncate("falha: usuário não encontrado", 12); got != "falha: usuá…" {
		t.Fatalf("truncate = %q", got)
	}
	if got := truncate("short\nline", 60); got != "short line" {
		t.Fatalf("truncate = %q", got)
	}
}

func TestJobsRetryAllPages(t *testing.T) {
	q := jobsFixture(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ { // five failed jobs in all
		env := jobs.Envelope{ID: fmt.Sprintf("more-%d", i), Name: "send_welcome_email", Version: 1, Payload: json.RawMessage(`{}`)}
		if err := q.Push(ctx, "mail", env, time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	for {
		d, err := q.Reserve(ctx, []string{"mail"}, time.Minute)
		if err != nil {
			break
		}
		if err := q.Bury(ctx, d, jobs.Failure{Reason: jobs.ReasonExhausted}); err != nil {
			t.Fatal(err)
		}
	}
	prev := retryAllPage
	t.Cleanup(func() { retryAllPage = prev })
	retryAllPage = 2
	out, err := runJobs(t, "retry", "--all")
	if err != nil || strings.Count(out, "Retrying ") != 6 {
		t.Fatalf("retry --all in pages of 2 = %v:\n%s", err, out)
	}
	if left, _ := q.Failed(ctx, "mail", 0); len(left) != 0 {
		t.Fatalf("%d failed jobs left", len(left))
	}
}

// shortPageQueue returns one short page first, as a Redis read in pages
// does when a job fails while it is under way.
type shortPageQueue struct {
	*jobs.MemoryQueue
	short bool
}

func (q *shortPageQueue) Failed(ctx context.Context, queue string, limit int) ([]jobs.FailedJob, error) {
	page, err := q.MemoryQueue.Failed(ctx, queue, limit)
	if !q.short && len(page) > 1 {
		q.short = true
		page = page[:1]
	}
	return page, err
}

// TestJobsRetryAllReadsUntilEmpty: a short page is not the end of the
// failed set; retry --all stops only when a read comes back empty.
func TestJobsRetryAllReadsUntilEmpty(t *testing.T) {
	q := &shortPageQueue{MemoryQueue: jobsFixture(t)}
	openJobsQueue = func(config.Config) (jobs.Queue, func() error, error) { return q, func() error { return nil }, nil }
	out, err := runJobs(t, "retry", "--all")
	if err != nil || strings.Count(out, "Retrying ") != 2 {
		t.Fatalf("retry --all after a short page = %v:\n%s", err, out)
	}
	if left, _ := q.MemoryQueue.Failed(context.Background(), "mail", 0); len(left) != 0 {
		t.Fatalf("%d failed jobs left behind a short page", len(left))
	}
}

func TestJobsRetryAllSkipsJobsWhoseKeyIsHeld(t *testing.T) {
	q := jobs.NewMemoryQueue()
	ctx := context.Background()
	key := jobs.UniqueKey{Key: "rebuild:1", TTL: time.Hour, UntilDone: true}
	for _, id := range []string{"held", "free-1", "free-2"} {
		env := jobs.Envelope{ID: id, Name: "rebuild", Version: 1, Payload: json.RawMessage(`{}`)}
		var err error
		if id == "held" {
			err = q.PushUnique(ctx, "mail", env, time.Time{}, key)
		} else {
			err = q.Push(ctx, "mail", env, time.Time{})
		}
		if err != nil {
			t.Fatal(err)
		}
		d, err := q.Reserve(ctx, []string{"mail"}, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err := q.Bury(ctx, d, jobs.Failure{Reason: jobs.ReasonExhausted}); err != nil {
			t.Fatal(err)
		}
	}
	// Another job has taken the key since "held" failed.
	if err := q.PushUnique(ctx, "mail", jobs.Envelope{ID: "holder", Name: "rebuild", Version: 1, Payload: json.RawMessage(`{}`)}, time.Time{}, key); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Jobs.Driver, cfg.Jobs.Queue = config.JobsDriverRedis, "mail"
	prevCfg, prevOpen, prevPage := LoadConfig, openJobsQueue, retryAllPage
	t.Cleanup(func() { LoadConfig, openJobsQueue, retryAllPage = prevCfg, prevOpen, prevPage })
	LoadConfig = func() (config.Config, error) { return cfg, nil }
	openJobsQueue = func(config.Config) (jobs.Queue, func() error, error) { return q, func() error { return nil }, nil }
	retryAllPage = 1
	out, err := runJobs(t, "retry", "--all")
	if err != nil || strings.Count(out, "Retrying ") != 2 || !strings.Contains(out, "Skipped held: job holder holds its uniqueness key") {
		t.Fatalf("retry --all = %v:\n%s", err, out)
	}
	if left, _ := q.Failed(ctx, "mail", 0); len(left) != 1 || left[0].Envelope.ID != "held" {
		t.Fatalf("failed after retry --all: %+v, want only the skipped job", left)
	}
}
