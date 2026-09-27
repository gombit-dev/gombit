package faulttest_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/internal/faulttest"
)

var (
	errRetryable = errors.New("temporarily unavailable")
	errPermanent = errors.New("invalid request")
)

// retryError is the reference policy's terminal error: the last attempt's
// error and how many attempts ran.
type retryError struct {
	attempts int
	last     error
}

func (e *retryError) Error() string { return fmt.Sprintf("after %d attempts: %v", e.attempts, e.last) }
func (e *retryError) Unwrap() error { return e.last }

// reference is a correct policy: up to max attempts, exponential backoff
// from base capped at maxDelay (overflow-safe), permanent errors and
// cancellation end it at once.
type reference struct {
	max            int
	base, maxDelay time.Duration
}

func (r reference) delay(attempt int) time.Duration {
	d := r.base
	for i := 1; i < attempt; i++ {
		if d >= r.maxDelay/2 {
			return r.maxDelay
		}
		d *= 2
	}
	return min(d, r.maxDelay)
}

func (r reference) do(ctx context.Context, sleeper faulttest.Sleeper, op func(context.Context) error) error {
	var last error
	for attempt := 1; attempt <= r.max; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		last = op(ctx)
		if last == nil {
			return nil
		}
		if errors.Is(last, errPermanent) || attempt == r.max {
			break
		}
		if err := sleeper.Sleep(ctx, r.delay(attempt)); err != nil {
			return err
		}
	}
	return &retryError{attempts: attemptsOf(last, r.max), last: last}
}

// attemptsOf is how many attempts ran before last ended the policy.
func attemptsOf(last error, max int) int {
	if errors.Is(last, errPermanent) {
		return 1
	}
	return max
}

func referenceContract(r reference) faulttest.RetryContract {
	return faulttest.RetryContract{
		Do:            r.do,
		Retryable:     errRetryable,
		Permanent:     errPermanent,
		MaxAttempts:   r.max,
		Delay:         r.delay,
		MaxDelay:      r.maxDelay,
		Deterministic: true,
		Attempts: func(err error) (int, bool) {
			var re *retryError
			if errors.As(err, &re) {
				return re.attempts, true
			}
			return 0, false
		},
	}
}

func TestCheckRetryPolicyAcceptsACorrectPolicy(t *testing.T) {
	faulttest.CheckRetryPolicy(t, referenceContract(reference{max: 5, base: 100 * time.Millisecond, maxDelay: 10 * time.Second}))
	faulttest.CheckRetryPolicy(t, referenceContract(reference{max: 1, base: time.Second, maxDelay: time.Second}))
}

// recorder captures a checker's failures instead of failing the test.
type recorder struct {
	testing.TB
	errs []string
}

func (r *recorder) Helper() {}
func (r *recorder) Errorf(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

// rejects runs the checker on c and fails t unless it reported a violation
// of every property named.
func rejects(t *testing.T, c faulttest.RetryContract, properties ...string) {
	t.Helper()
	rec := &recorder{}
	faulttest.CheckRetryPolicy(rec, c)
	all := strings.Join(rec.errs, "\n")
	for _, p := range properties {
		if !strings.Contains(all, p+":") {
			t.Errorf("the checker did not report %q for this policy; it reported:\n%s", p, all)
		}
	}
}

// TestCheckRetryPolicyAcceptsASlowButCorrectBackoff: a capped backoff that
// loops over the attempt number is correct; the checker must not probe it
// with attempts no policy makes.
func TestCheckRetryPolicyAcceptsASlowButCorrectBackoff(t *testing.T) {
	r := reference{max: 5, base: 10 * time.Millisecond, maxDelay: time.Second}
	c := referenceContract(r)
	c.Delay = func(attempt int) time.Duration {
		d := r.base
		for i := 1; i < attempt; i++ {
			d = min(2*d, r.maxDelay)
		}
		return d
	}
	faulttest.CheckRetryPolicy(t, c)
}

func TestCheckRetryPolicyRejectsBrokenPolicies(t *testing.T) {
	good := reference{max: 4, base: 10 * time.Millisecond, maxDelay: time.Second}

	t.Run("the unbounded loop", func(t *testing.T) {
		// for { if dependency.Call() == nil { return nil } }
		c := referenceContract(good)
		c.Do = func(ctx context.Context, _ faulttest.Sleeper, op func(context.Context) error) error {
			for {
				if op(ctx) == nil {
					return nil
				}
			}
		}
		rejects(t, c, "bounded", "permanent", "cancellation")
	})

	t.Run("one attempt too many", func(t *testing.T) {
		c := referenceContract(good)
		c.Do = reference{max: good.max + 1, base: good.base, maxDelay: good.maxDelay}.do
		rejects(t, c, "bounded")
	})

	t.Run("retrying a permanent error", func(t *testing.T) {
		c := referenceContract(good)
		c.Permanent = errors.New("not the reference's permanent error") // the policy retries it
		rejects(t, c, "permanent")
	})

	t.Run("ignoring cancellation mid-backoff", func(t *testing.T) {
		defer faulttest.SetRetryGuard(200 * time.Millisecond)() // it never returns on its own
		c := referenceContract(good)
		c.Do = func(ctx context.Context, sleeper faulttest.Sleeper, op func(context.Context) error) error {
			return good.do(context.Background(), ignoreCancel{sleeper}, op)
		}
		rejects(t, c, "cancellation")
	})

	t.Run("ignoring an already-canceled context", func(t *testing.T) {
		c := referenceContract(good)
		c.Do = func(ctx context.Context, sleeper faulttest.Sleeper, op func(context.Context) error) error {
			var last error
			for attempt := 1; attempt <= good.max; attempt++ {
				if last = op(ctx); last == nil || errors.Is(last, errPermanent) {
					return last
				}
				_ = sleeper.Sleep(ctx, good.delay(attempt)) // error ignored
			}
			return last
		}
		rejects(t, c, "cancellation")
	})

	t.Run("an overflowing backoff", func(t *testing.T) {
		c := referenceContract(good)
		c.Delay = func(attempt int) time.Duration { return good.base << attempt } // wraps negative
		rejects(t, c, "backoff")
	})

	t.Run("a backoff that differs from its promise", func(t *testing.T) {
		c := referenceContract(good)
		c.Delay = func(int) time.Duration { return 0 }
		rejects(t, c, "backoff")
	})

	t.Run("a terminal error that hides the cause", func(t *testing.T) {
		c := referenceContract(good)
		c.Do = func(ctx context.Context, s faulttest.Sleeper, op func(context.Context) error) error {
			if err := good.do(ctx, s, op); err != nil {
				return errors.New("retries exhausted")
			}
			return nil
		}
		rejects(t, c, "observable failure")
	})

	t.Run("a backoff that never returns", func(t *testing.T) {
		defer faulttest.SetRetryGuard(200 * time.Millisecond)()
		c := referenceContract(good)
		block := make(chan struct{})
		defer close(block)
		c.Delay = func(int) time.Duration { <-block; return 0 }
		c.Deterministic = false
		c.Do = func(ctx context.Context, s faulttest.Sleeper, op func(context.Context) error) error {
			return good.do(ctx, s, op) // waits by the reference schedule, not the stuck Delay
		}
		rejects(t, c, "backoff")
	})

	t.Run("giving up early", func(t *testing.T) {
		c := referenceContract(good)
		c.Do = reference{max: 2, base: good.base, maxDelay: good.maxDelay}.do
		rejects(t, c, "retryable")
	})
}

// ignoreCancel waits through a sleeper on a context that never ends.
type ignoreCancel struct{ faulttest.Sleeper }

func (i ignoreCancel) Sleep(_ context.Context, d time.Duration) error {
	return i.Sleeper.Sleep(context.Background(), d)
}

func TestSleepers(t *testing.T) {
	fake := &faulttest.FakeSleeper{}
	for _, d := range []time.Duration{time.Hour, time.Minute} {
		if err := fake.Sleep(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
	if got := fake.Delays(); len(got) != 2 || got[0] != time.Hour || got[1] != time.Minute {
		t.Fatalf("Delays = %v", got)
	}
	<-fake.Sleeping()

	blocking := &faulttest.FakeSleeper{Blocking: true}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- blocking.Sleep(ctx, time.Hour) }()
	<-blocking.Sleeping()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("blocking sleep canceled = %v", err)
	}

	if err := (faulttest.RealSleeper{}).Sleep(context.Background(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	if err := (faulttest.RealSleeper{}).Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("real sleep on a canceled context = %v", err)
	}
}
