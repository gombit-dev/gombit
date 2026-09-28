package faulttest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// Sleeper is the time seam a retry policy waits through between attempts,
// so its backoff can be tested without sleeping.
type Sleeper interface {
	// Sleep waits for d, or until ctx ends (returning ctx's error).
	Sleep(ctx context.Context, d time.Duration) error
}

// RealSleeper waits on a timer.
type RealSleeper struct{}

// Sleep implements Sleeper.
func (RealSleeper) Sleep(ctx context.Context, d time.Duration) error {
	// A context that has already ended fails the wait, whatever d is: in the
	// select below an already-fired timer would win half the time.
	if err := ctx.Err(); err != nil || d <= 0 {
		return err
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// FakeSleeper records every delay it is asked for and returns at once (a
// context that has already ended still returns its error). With Blocking
// set, it instead waits for the context to end: a retry that is mid-backoff
// when it is canceled.
type FakeSleeper struct {
	// Blocking makes Sleep wait until ctx ends.
	Blocking bool

	mu       sync.Mutex
	delays   []time.Duration
	sleeping chan struct{}
}

// Sleep implements Sleeper.
func (s *FakeSleeper) Sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.delays = append(s.delays, d)
	if s.sleeping == nil {
		s.sleeping = make(chan struct{})
	}
	select {
	case <-s.sleeping:
	default:
		close(s.sleeping)
	}
	blocking := s.Blocking
	s.mu.Unlock()
	if blocking {
		<-ctx.Done()
	}
	return ctx.Err()
}

// Delays returns the delays asked for, in order.
func (s *FakeSleeper) Delays() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.delays...)
}

// Sleeping returns a channel closed once the first Sleep has begun.
func (s *FakeSleeper) Sleeping() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sleeping == nil {
		s.sleeping = make(chan struct{})
	}
	return s.sleeping
}

// RetryContract describes a retry policy for CheckRetryPolicy (INV-4: every
// retry is bounded, honors cancellation, and ends in success or an
// observable terminal error).
type RetryContract struct {
	// Do runs op under the policy, waiting between attempts through
	// sleeper. It is the policy under test.
	Do func(ctx context.Context, sleeper Sleeper, op func(context.Context) error) error
	// Retryable is an error the policy retries; Permanent one it must not.
	Retryable, Permanent error
	// MaxAttempts is the policy's attempt budget, the first attempt
	// included: an op that keeps failing retryably runs exactly MaxAttempts
	// times (never more, which is the bound; never fewer, since a retryable
	// failure is retried while budget remains).
	MaxAttempts int
	// Delay is the policy's backoff: the wait after the attempt-th failure
	// (1-based). Every delay it produces must lie within [0, MaxDelay] (no
	// negative or overflowed durations, jitter included). The checker samples
	// that (see CheckRetryPolicy's backoff bullet, which says what the sample
	// does not catch): cap a delay before the arithmetic can overflow, not
	// after.
	Delay func(attempt int) time.Duration
	// MaxDelay caps Delay.
	MaxDelay time.Duration
	// Deterministic says Delay is exact (no jitter): the waits the policy
	// asks the sleeper for must equal Delay(1), Delay(2), ...
	Deterministic bool
	// Attempts, when set, reads the attempt count from the policy's final
	// error (a diagnostic the policy promises to expose).
	Attempts func(err error) (int, bool)
}

// retryGuard bounds one run of the policy under test: a loop that ignores
// its bound, the sleeper, and the context never returns, and must fail the
// check rather than hang it.
var retryGuard = 5 * time.Second

// CheckRetryPolicy runs c.Do through the retry contract and reports every
// violated property (by name) through t.Errorf:
//
//   - bounded: an op that always fails retryably is tried no more than
//     MaxAttempts times;
//   - retryable: ...and no fewer (a retryable failure is retried while the
//     budget lasts), and an op that fails once and then succeeds succeeds
//     on the retry;
//   - cancellation: a context that has already ended makes Do return the
//     context's error without starting an attempt; a context canceled
//     mid-backoff ends Do promptly with the context's error, and no further
//     attempt runs;
//   - permanent: a non-retryable error is not retried;
//   - backoff: the waits asked for follow Delay (exactly, when
//     Deterministic) and lie within [0, MaxDelay]; and Delay itself,
//     called 20 times per attempt (for jitter) at every attempt
//     1..max(MaxAttempts, 128) and at 1000, is within [0, MaxDelay]. That
//     catches a shift or doubling that overflows: any base of 1ns or more
//     wraps by attempt 64. It does not catch an attempt multiplied by a
//     unit, which wraps at an attempt that depends on the unit, usually far
//     past 1000: compute a delay so it cannot overflow (cap before the
//     arithmetic, not after);
//   - success: a success ends the retries at once;
//   - observable failure: the final error wraps the last attempt's error
//     (and reports the attempt count, when Attempts is set).
//
// Each run of the policy is bounded by a guard: a policy that never returns
// fails the check, and its goroutine is left running for the rest of the
// test binary (Go cannot stop it), so a broken policy should be fixed, not
// kept failing. The contract describes attempt-bounded policies with a
// Delay per attempt; a policy bounded by a total deadline states the
// attempts that deadline allows.
func CheckRetryPolicy(t testing.TB, c RetryContract) {
	t.Helper()
	if c.Do == nil || c.Retryable == nil || c.Permanent == nil || c.MaxAttempts < 1 || c.Delay == nil || c.MaxDelay < 0 {
		t.Errorf("faulttest: CheckRetryPolicy needs Do, Retryable, Permanent, MaxAttempts >= 1, Delay, and MaxDelay >= 0")
		return
	}

	// run drives one Do with results scripted per call (after the script,
	// results repeat its last entry); it stops a runaway loop by making
	// every call past the bound succeed, and gives up after retryGuard.
	type outcome struct {
		err      error
		calls    int
		runaway  bool
		finished bool
	}
	run := func(ctx context.Context, sleeper Sleeper, results ...error) outcome {
		var mu sync.Mutex
		calls, runaway := 0, false
		bound := c.MaxAttempts + 100
		op := func(context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			calls++
			if calls > bound {
				runaway = true
				return nil // let a loop that stops on success stop
			}
			if calls <= len(results) {
				return results[calls-1]
			}
			return results[len(results)-1]
		}
		done := make(chan error, 1)
		go func() { done <- c.Do(ctx, sleeper, op) }()
		select {
		case err := <-done:
			mu.Lock()
			defer mu.Unlock()
			return outcome{err: err, calls: calls, runaway: runaway, finished: true}
		case <-time.After(retryGuard):
			mu.Lock()
			defer mu.Unlock()
			return outcome{calls: calls, runaway: true}
		}
	}

	// bounded, retryable (the budget is used), observable failure, and the
	// waits asked for: each reported on its own, so one violation does not
	// hide the others.
	var waits []time.Duration
	{
		sleeper := &FakeSleeper{}
		o := run(context.Background(), sleeper, c.Retryable)
		waits = sleeper.Delays()
		switch {
		case !o.finished || o.runaway:
			t.Errorf("bounded: an op that always fails retryably ran %d+ times and did not stop (MaxAttempts %d)", o.calls, c.MaxAttempts)
		case o.calls > c.MaxAttempts:
			t.Errorf("bounded: %d attempts, more than MaxAttempts %d", o.calls, c.MaxAttempts)
		case o.calls < c.MaxAttempts:
			t.Errorf("retryable: gave up after %d attempts, before its budget of MaxAttempts %d", o.calls, c.MaxAttempts)
		}
		if o.finished && !o.runaway {
			if !errors.Is(o.err, c.Retryable) {
				t.Errorf("observable failure: final error %v does not wrap the last attempt's error %v", o.err, c.Retryable)
			}
			if c.Attempts != nil {
				if n, ok := c.Attempts(o.err); !ok || n != o.calls {
					t.Errorf("observable failure: final error reports %d attempts (found %v), want %d", n, ok, o.calls)
				}
			}
			if got := len(waits); got != o.calls-1 {
				t.Errorf("backoff: %d waits between %d attempts, want %d", got, o.calls, o.calls-1)
			}
		}
	}

	// permanent
	{
		sleeper := &FakeSleeper{}
		o := run(context.Background(), sleeper, c.Permanent)
		if !o.finished || o.runaway || o.calls != 1 || !errors.Is(o.err, c.Permanent) || len(sleeper.Delays()) != 0 {
			t.Errorf("permanent: a non-retryable error ran %d attempts with %d waits and returned %v; want 1 attempt, no wait, the error itself",
				o.calls, len(sleeper.Delays()), o.err)
		}
	}

	// success, first time and after retries
	{
		sleeper := &FakeSleeper{}
		o := run(context.Background(), sleeper, nil)
		if !o.finished || o.err != nil || o.calls != 1 || len(sleeper.Delays()) != 0 {
			t.Errorf("success: a first-attempt success ran %d attempts, %d waits, and returned %v; want 1, 0, nil", o.calls, len(sleeper.Delays()), o.err)
		}
		if c.MaxAttempts >= 2 {
			sleeper := &FakeSleeper{}
			o := run(context.Background(), sleeper, c.Retryable, nil)
			if !o.finished || o.err != nil || o.calls != 2 {
				t.Errorf("retryable: fail-then-succeed ran %d attempts and returned %v; want 2 and nil", o.calls, o.err)
			}
		}
	}

	// cancellation mid-backoff
	if c.MaxAttempts >= 2 {
		sleeper := &FakeSleeper{Blocking: true}
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan outcome, 1)
		go func() { result <- run(ctx, sleeper, c.Retryable) }()
		select {
		case <-sleeper.Sleeping():
			cancel()
			o := <-result
			if !o.finished || o.runaway {
				t.Errorf("cancellation: canceled mid-backoff, Do did not return (%d attempts)", o.calls)
			} else if !errors.Is(o.err, context.Canceled) || o.calls != 1 {
				t.Errorf("cancellation: canceled mid-backoff, Do returned %v after %d attempts; want context.Canceled after 1", o.err, o.calls)
			}
		case o := <-result:
			cancel()
			t.Errorf("cancellation: the policy never waited through its sleeper (%d attempts, %v): its backoff cannot be canceled or tested", o.calls, o.err)
		}
	}

	// already canceled: no attempt starts work for a caller that has gone
	{
		sleeper := &FakeSleeper{}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		o := run(ctx, sleeper, c.Retryable)
		if !o.finished || o.runaway || !errors.Is(o.err, context.Canceled) || o.calls != 0 {
			t.Errorf("cancellation: on an already-canceled context Do ran %d attempts and returned %v; want no attempt and context.Canceled",
				o.calls, o.err)
		}
	}

	// backoff: the waits asked for, then the Delay sample the godoc lists.
	// A doubling of any base of 1ns or more crosses the int64 sign bit (or
	// shifts off it to 0) by attempt 64, so every attempt up to 128 is
	// probed. Attempts past MaxAttempts are probed on purpose: an overflow a
	// policy would reach with a larger budget is still a bug. It all runs
	// under the guard, so a Delay that never returns fails the check rather
	// than hanging it.
	attempts := make([]int, 0, 129)
	for a := 1; a <= max(c.MaxAttempts, 128); a++ {
		attempts = append(attempts, a)
	}
	attempts = append(attempts, 1000)
	problems := make(chan []string, 1)
	go func() {
		var found []string
		for i, d := range waits {
			if d < 0 || d > c.MaxDelay {
				found = append(found, fmt.Sprintf("backoff: wait %d was %s, outside [0, %s]", i+1, d, c.MaxDelay))
			}
			if c.Deterministic {
				if want := c.Delay(i + 1); d != want {
					found = append(found, fmt.Sprintf("backoff: wait %d was %s, want Delay(%d) = %s", i+1, d, i+1, want))
				}
			}
		}
	sample:
		for _, attempt := range attempts {
			for i := 0; i < 20; i++ {
				if d := c.Delay(attempt); d < 0 || d > c.MaxDelay {
					found = append(found, fmt.Sprintf("backoff: Delay(%d) = %s, outside [0, %s] (overflow or unbounded jitter)", attempt, d, c.MaxDelay))
					break sample
				}
			}
		}
		problems <- found
	}()
	select {
	case found := <-problems:
		for _, msg := range found {
			t.Errorf("%s", msg)
		}
	case <-time.After(retryGuard):
		t.Errorf("backoff: Delay did not return within %s", retryGuard)
	}
}
