package main

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/framework"
	"github.com/gombit-dev/gombit/jobs/jobstest"
)

// TestExampleRuns keeps the example honest: main exits the test binary with
// log.Fatal when a step does not classify as it says.
func TestExampleRuns(t *testing.T) {
	main()
}

// TestSignUpQueuesTheWelcomeEmail tests the dispatching code the way an app
// does: the app's jobs go to a jobstest queue, the test asserts what was
// queued, then runs it, with no worker and no Redis.
func TestSignUpQueuesTheWelcomeEmail(t *testing.T) {
	q := jobstest.New()
	app, err := framework.New(framework.WithConfig(config.Default()), framework.WithJobs(q.Dispatcher()))
	if err != nil {
		t.Fatal(err)
	}
	var done atomic.Int32
	registerJobs(app.Jobs().Registry(), &done)
	ctx := context.Background()

	if err := signUp(ctx, app.Jobs(), 42); err != nil {
		t.Fatal(err)
	}
	d := q.AssertDispatched(t, "send_welcome_email")
	if job := jobstest.Payload[SendWelcomeEmail](t, d); job.UserID != 42 {
		t.Fatalf("queued %+v, want user 42", job)
	}
	if d.Unique == nil || d.Unique.Key != "welcome:42" {
		t.Fatalf("queued without its uniqueness key: %+v", d.Unique)
	}
	if done.Load() != 0 {
		t.Fatal("dispatching ran the job")
	}

	if ran, err := q.RunAll(ctx); ran != 1 || err != nil {
		t.Fatalf("RunAll = %d, %v", ran, err)
	}
	if done.Load() != 1 {
		t.Fatalf("the welcome email ran %d times", done.Load())
	}
}
