package cli

import (
	"bytes"
	"context"
	"encoding/json"
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
