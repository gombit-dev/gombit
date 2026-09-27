package jobs_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/jobs"
)

func TestBackoffs(t *testing.T) {
	exp := jobs.Exponential(time.Second, 10*time.Second)
	for attempt, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 4: 8 * time.Second, 5: 10 * time.Second, 40: 10 * time.Second} {
		if got := exp(attempt); got != want {
			t.Errorf("Exponential(1s,10s)(%d) = %s, want %s", attempt, got, want)
		}
	}
	if jobs.DefaultBackoff(1) != 10*time.Second || jobs.DefaultBackoff(20) != 10*time.Minute {
		t.Errorf("DefaultBackoff = %s .. %s, want 10s .. 10m", jobs.DefaultBackoff(1), jobs.DefaultBackoff(20))
	}
	if jobs.Constant(3*time.Second)(9) != 3*time.Second {
		t.Error("Constant does not hold")
	}
	jit := jobs.Jittered(jobs.Constant(time.Second))
	for i := 0; i < 200; i++ {
		if d := jit(1); d < 500*time.Millisecond || d > time.Second {
			t.Fatalf("Jittered(1s) = %s, want within [500ms, 1s]", d)
		}
	}
}

func TestOptionsResolveAndValidate(t *testing.T) {
	reg := jobs.NewRegistry(jobs.WithDefaultOptions(jobs.Options{MaxAttempts: 8, Timeout: time.Minute}))
	jobs.MustRegister(reg, func(context.Context, sendWelcome) error { return nil }, jobs.WithOptions(jobs.Options{MaxAttempts: 2}))
	jobs.MustRegister(reg, func(context.Context, other) error { return nil })

	own := reg.Options("send_welcome_email")
	if own.MaxAttempts != 2 || own.Timeout != time.Minute || own.Backoff == nil {
		t.Fatalf("Options(own) = %+v, want its MaxAttempts and the default Timeout", own)
	}
	if def := reg.Options("other"); def.MaxAttempts != 8 || def.Timeout != time.Minute {
		t.Fatalf("Options(defaulted) = %+v", def)
	}
	if unknown := reg.Options("from_a_newer_deploy"); unknown.MaxAttempts != 8 {
		t.Fatalf("Options(unknown) = %+v, want the registry defaults so it stays bounded", unknown)
	}
	if bare := jobs.NewRegistry().Options("x"); bare.MaxAttempts != jobs.DefaultMaxAttempts || bare.Backoff == nil || bare.Timeout != 0 {
		t.Fatalf("package defaults = %+v", bare)
	}
	for _, bad := range []jobs.Options{{MaxAttempts: -1}, {Timeout: -time.Second}} {
		if err := jobs.Register(jobs.NewRegistry(), func(context.Context, sendWelcome) error { return nil }, jobs.WithOptions(bad)); !errors.Is(err, jobs.ErrInvalidJobType) {
			t.Errorf("Register(%+v) error = %v", bad, err)
		}
	}
}

// TestTimeoutReachesTheHandler: the job's Timeout is the handler context's
// deadline, and running out of it is KindTimeout, for every driver (Run
// applies it, not the worker).
func TestTimeoutReachesTheHandler(t *testing.T) {
	reg := newRegistry()
	var sawDeadline bool
	jobs.MustRegister(reg, func(ctx context.Context, _ sendWelcome) error {
		_, sawDeadline = ctx.Deadline()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
			return errors.New("the timeout never canceled the handler")
		}
	}, jobs.WithOptions(jobs.Options{Timeout: 20 * time.Millisecond}))
	env, err := reg.Encode(context.Background(), sendWelcome{UserID: 1})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = reg.Run(context.Background(), env)
	if !sawDeadline || !errors.Is(err, jobs.ErrTimeout) || jobs.Classify(err) != jobs.KindTimeout || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run = %v (deadline seen %v), want KindTimeout wrapping DeadlineExceeded", err, sawDeadline)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("the timeout took %s", took)
	}
	if jobs.IsPermanent(err) {
		t.Fatal("a timeout is retryable")
	}

	// Finishing in time with no error is success, and a caller's own
	// cancellation is not a timeout.
	quick := newRegistry()
	jobs.MustRegister(quick, func(ctx context.Context, _ sendWelcome) error { return ctx.Err() }, jobs.WithOptions(jobs.Options{Timeout: time.Minute}))
	env, _ = quick.Encode(context.Background(), sendWelcome{})
	if err := quick.Run(context.Background(), env); err != nil {
		t.Fatalf("Run(in time) = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := quick.Run(canceled, env); jobs.Classify(err) != jobs.KindHandler {
		t.Fatalf("Run(caller canceled) kind = %s, want handler", jobs.Classify(err))
	}
}

func TestPermanent(t *testing.T) {
	base := errors.New("user 7 was deleted")
	if jobs.Permanent(nil) != nil {
		t.Fatal("Permanent(nil) is not nil")
	}
	p := jobs.Permanent(base)
	if !errors.Is(p, base) || !jobs.IsPermanent(p) || p.Error() != base.Error() {
		t.Fatalf("Permanent(%v) = %v", base, p)
	}
	wrapped := &jobs.Error{Kind: jobs.KindHandler, Err: p}
	if !jobs.IsPermanent(wrapped) || jobs.IsPermanent(&jobs.Error{Kind: jobs.KindHandler, Err: base}) {
		t.Fatal("IsPermanent must see through the handler error and only there")
	}
	if !jobs.IsPermanent(&jobs.Error{Kind: jobs.KindDecode, Err: base}) {
		t.Fatal("a decode failure is permanent")
	}
	for _, kind := range []jobs.Kind{jobs.KindUnknownJob, jobs.KindUnsupportedVersion, jobs.KindPanic, jobs.KindTimeout} {
		if jobs.IsPermanent(&jobs.Error{Kind: kind, Err: base}) {
			t.Errorf("%s is permanent; it may succeed on a retry (a later deploy, a transient state)", kind)
		}
	}
}

func TestTimeoutIsOnlyTheJobsOwn(t *testing.T) {
	reg := newRegistry()
	jobs.MustRegister(reg, func(ctx context.Context, _ sendWelcome) error {
		<-ctx.Done()
		return ctx.Err()
	}, jobs.WithOptions(jobs.Options{Timeout: time.Minute}))
	env, _ := reg.Encode(context.Background(), sendWelcome{})
	// The caller's deadline (a request around a sync dispatch) runs out first.
	callerCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := reg.Run(callerCtx, env); jobs.Classify(err) != jobs.KindHandler || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run under the caller's deadline = %v (kind %s), want handler, not the job's timeout", err, jobs.Classify(err))
	}
}

func TestDefaultOptionsAreValidated(t *testing.T) {
	reg := jobs.NewRegistry(jobs.WithDefaultOptions(jobs.Options{MaxAttempts: -1}))
	if err := jobs.Register(reg, func(context.Context, sendWelcome) error { return nil }); !errors.Is(err, jobs.ErrInvalidJobType) {
		t.Fatalf("Register under invalid defaults = %v", err)
	}
}

func TestExponentialDoesNotOverflow(t *testing.T) {
	huge := jobs.Exponential(time.Second, time.Duration(1<<63-1))
	for attempt := 1; attempt < 200; attempt++ {
		if d := huge(attempt); d <= 0 {
			t.Fatalf("Exponential(1s, max)(%d) = %s", attempt, d)
		}
	}
}
