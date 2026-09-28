package jobs_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/jobs"
)

func TestSyncDispatchRunsTheJobInline(t *testing.T) {
	reg := newRegistry()
	boom := errors.New("smtp down")
	var ran []uint
	var attempt int
	jobs.MustRegister(reg, func(ctx context.Context, job sendWelcome) error {
		ran = append(ran, job.UserID)
		info, _ := jobs.InfoFromContext(ctx)
		attempt = info.Attempt
		if job.UserID == 13 {
			return boom
		}
		return nil
	})
	d := jobs.NewDispatcher(reg, nil)
	if d.Driver() != config.JobsDriverSync || d.Queue() != nil || d.Registry() != reg || d.DefaultQueue() != "default" {
		t.Fatalf("sync dispatcher = driver %q queue %v default %q", d.Driver(), d.Queue(), d.DefaultQueue())
	}

	env, err := d.Dispatch(context.Background(), sendWelcome{UserID: 7})
	if err != nil || env.ID == "" || len(ran) != 1 || ran[0] != 7 || attempt != 1 {
		t.Fatalf("Dispatch(sync) = %+v, %v; ran %v attempt %d", env, err, ran, attempt)
	}
	// The sync driver surfaces the job's failure to the dispatcher.
	if _, err := d.Dispatch(context.Background(), sendWelcome{UserID: 13}); !errors.Is(err, boom) || jobs.Classify(err) != jobs.KindHandler {
		t.Fatalf("Dispatch(sync, failing job) error = %v, want the handler's", err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestQueuedDispatchStoresTheJob(t *testing.T) {
	reg := newRegistry()
	ran := false
	jobs.MustRegister(reg, func(context.Context, sendWelcome) error { ran = true; return nil })
	q := jobs.NewMemoryQueue()
	d := jobs.NewDispatcher(reg, q, jobs.WithDefaultQueue("mail"))

	env, err := d.Dispatch(context.Background(), sendWelcome{UserID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Fatal("a queued dispatch ran the job")
	}
	got, err := q.Reserve(context.Background(), []string{"mail"}, time.Minute)
	if err != nil || got.Envelope.ID != env.ID {
		t.Fatalf("Reserve(mail) = %+v, %v; want the dispatched job on the default queue", got, err)
	}

	if _, err := d.Dispatch(context.Background(), sendWelcome{UserID: 8}, jobs.OnQueue("critical")); err != nil {
		t.Fatal(err)
	}
	if got, err := q.Reserve(context.Background(), []string{"critical"}, time.Minute); err != nil || got.Queue != "critical" {
		t.Fatalf("Reserve(critical) = %+v, %v; want the OnQueue job", got, err)
	}

	if _, err := d.Dispatch(context.Background(), sendWelcome{UserID: 9}, jobs.OnQueue("No Spaces")); !errors.Is(err, jobs.ErrInvalidQueue) {
		t.Fatalf("Dispatch(OnQueue invalid) error = %v, want ErrInvalidQueue", err)
	}
	if _, err := d.Dispatch(context.Background(), other{}); !errors.Is(err, jobs.ErrUnknownJob) {
		t.Fatalf("Dispatch(unregistered) error = %v, want ErrUnknownJob", err)
	}
	if q.Len() != 2 {
		t.Fatalf("queue holds %d jobs, want 2 (failed dispatches store nothing)", q.Len())
	}
}

func TestOpenSelectsTheConfiguredDriver(t *testing.T) {
	redisCfg := config.Default().Cache.Redis
	for _, tc := range []struct {
		driver    config.JobsDriver
		wantQueue string
	}{
		{config.JobsDriverSync, ""},
		{config.JobsDriverMemory, "*jobs.MemoryQueue"},
		{config.JobsDriverRedis, "*jobs.RedisQueue"}, // go-redis connects lazily
	} {
		d, err := jobs.Open(config.JobsConfig{Driver: tc.driver, Queue: "mail", Namespace: "app"}, redisCfg, jobs.NewRegistry())
		if err != nil {
			t.Fatalf("Open(%s) error = %v", tc.driver, err)
		}
		gotQueue := ""
		if q := d.Queue(); q != nil {
			gotQueue = typeName(q)
		}
		if d.Driver() != tc.driver || gotQueue != tc.wantQueue || d.DefaultQueue() != "mail" {
			t.Fatalf("Open(%s) = driver %q queue %s default %q", tc.driver, d.Driver(), gotQueue, d.DefaultQueue())
		}
		if err := d.Close(); err != nil {
			t.Fatalf("Close(%s) error = %v", tc.driver, err)
		}
	}
	if _, err := jobs.Open(config.JobsConfig{Driver: "kafka", Queue: "default", Namespace: "app"}, redisCfg, jobs.NewRegistry()); err == nil {
		t.Fatal("Open accepted an unknown driver")
	}
}

func typeName(v any) string {
	switch v.(type) {
	case *jobs.MemoryQueue:
		return "*jobs.MemoryQueue"
	case *jobs.RedisQueue:
		return "*jobs.RedisQueue"
	default:
		return "other"
	}
}
