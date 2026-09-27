package faulttest_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/internal/faulttest"
)

var errBoom = errors.New("boom")

// outcomes hits inj n times and reports which calls failed.
func outcomes(inj *faulttest.Injector, n int) []bool {
	failed := make([]bool, n)
	for i := range failed {
		failed[i] = inj.Hit(context.Background()) != nil
	}
	return failed
}

func TestPolicies(t *testing.T) {
	for _, tc := range []struct {
		name string
		inj  *faulttest.Injector
		want []bool // failed, per call
	}{
		{"FailAlways", faulttest.FailAlways(errBoom), []bool{true, true, true, true}},
		{"FailOnce", faulttest.FailOnce(errBoom), []bool{true, false, false, false}},
		{"FailNTimes", faulttest.FailNTimes(2, errBoom), []bool{true, true, false, false}},
		{"FailOnCall", faulttest.FailOnCall(3, errBoom), []bool{false, false, true, false}},
		{"Sequence", faulttest.Sequence(faulttest.Success(), faulttest.Failure(errBoom), faulttest.Success(), faulttest.Failure(errBoom)),
			[]bool{false, true, false, true, false, false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := outcomes(tc.inj, len(tc.want))
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("call %d failed = %v, want %v (all: %v)", i+1, got[i], tc.want[i], got)
				}
			}
			wantFailures := 0
			for _, f := range tc.want {
				if f {
					wantFailures++
				}
			}
			if tc.inj.Calls() != len(tc.want) || tc.inj.Failures() != wantFailures {
				t.Fatalf("Calls, Failures = %d, %d; want %d, %d", tc.inj.Calls(), tc.inj.Failures(), len(tc.want), wantFailures)
			}
		})
	}
}

func TestFailuresReturnTheInjectedError(t *testing.T) {
	if err := faulttest.FailOnce(errBoom).Hit(context.Background()); !errors.Is(err, errBoom) {
		t.Fatalf("Hit = %v, want the injected error", err)
	}
}

func TestDoRunsOnlyCallsThatPass(t *testing.T) {
	inj := faulttest.FailOnCall(1, faulttest.ErrInjected)
	ran := 0
	fn := func(context.Context) error { ran++; return nil }
	if err := inj.Do(context.Background(), fn); !errors.Is(err, faulttest.ErrInjected) || ran != 0 {
		t.Fatalf("failed call: Do = %v, fn ran %d times", err, ran)
	}
	if err := inj.Do(context.Background(), fn); err != nil || ran != 1 {
		t.Fatalf("passing call: Do = %v, fn ran %d times", err, ran)
	}
	fnErr := errors.New("fn failed")
	if err := inj.Do(context.Background(), func(context.Context) error { return fnErr }); !errors.Is(err, fnErr) {
		t.Fatalf("Do = %v, want fn's own error", err)
	}
}

func TestANilInjectorLetsEverythingThrough(t *testing.T) {
	var inj *faulttest.Injector
	if err := inj.Hit(context.Background()); err != nil || inj.Calls() != 0 || inj.Failures() != 0 {
		t.Fatalf("nil injector: Hit = %v, Calls %d, Failures %d", err, inj.Calls(), inj.Failures())
	}
	inj.Reset()
}

func TestBlockHoldsACallUntilReleased(t *testing.T) {
	release := make(chan struct{})
	inj := faulttest.BlockUntil(release)
	done := make(chan error, 1)
	go func() { done <- inj.Hit(context.Background()) }()
	<-inj.Reached(1) // in flight, held
	select {
	case err := <-done:
		t.Fatalf("the call returned (%v) before release", err)
	default:
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("released call = %v, want nil", err)
	}
	// Released for good: later calls pass straight through.
	if err := inj.Hit(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestBlockAndWaitHonorTheCallsContext(t *testing.T) {
	for name, inj := range map[string]*faulttest.Injector{
		"BlockUntil": faulttest.BlockUntil(make(chan struct{})),
		"Delay":      faulttest.Delay(time.Hour),
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- inj.Hit(ctx) }()
			<-inj.Reached(1)
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled call = %v, want context.Canceled", err)
			}
			if inj.Failures() != 1 {
				t.Fatalf("Failures = %d, want the canceled call counted", inj.Failures())
			}
		})
	}
}

func TestDelayLetsTheCallThroughAfterIt(t *testing.T) {
	inj := faulttest.Delay(time.Millisecond)
	if err := inj.Hit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := faulttest.Sequence(faulttest.Wait(0)).Hit(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestReached(t *testing.T) {
	inj := faulttest.Sequence()
	second := inj.Reached(2)
	if second != inj.Reached(2) {
		t.Fatal("Reached(2) twice returned different channels")
	}
	_ = inj.Hit(context.Background())
	select {
	case <-second:
		t.Fatal("Reached(2) closed after one call")
	default:
	}
	_ = inj.Hit(context.Background())
	<-second
	<-inj.Reached(1) // already reached: closed at once
}

func TestResetStartsOver(t *testing.T) {
	inj := faulttest.FailOnCall(2, errBoom)
	outcomes(inj, 3)
	inj.Reset()
	if inj.Calls() != 0 || inj.Failures() != 0 {
		t.Fatalf("after Reset: Calls %d, Failures %d", inj.Calls(), inj.Failures())
	}
	if got := outcomes(inj, 2); got[0] || !got[1] {
		t.Fatalf("after Reset the 2nd call fails again; got %v", got)
	}
}

// TestConcurrentCallsAreNumberedOnce: under concurrency every call gets
// exactly one number, so FailNTimes fails exactly n of them.
func TestConcurrentCallsAreNumberedOnce(t *testing.T) {
	inj := faulttest.FailNTimes(10, errBoom)
	var wg sync.WaitGroup
	var mu sync.Mutex
	failed := 0
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if inj.Hit(context.Background()) != nil {
				mu.Lock()
				failed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if failed != 10 || inj.Calls() != 100 || inj.Failures() != 10 {
		t.Fatalf("failed %d (Calls %d, Failures %d); want 10 of 100", failed, inj.Calls(), inj.Failures())
	}
}

func TestMisuseIsRefused(t *testing.T) {
	for name, fn := range map[string]func(){
		"Failure(nil)":    func() { faulttest.Failure(nil) },
		"FailNTimes(0)":   func() { faulttest.FailNTimes(0, errBoom) },
		"FailOnCall(0)":   func() { faulttest.FailOnCall(0, errBoom) },
		"Wait(-1)":        func() { faulttest.Wait(-time.Second) },
		"Block(nil)":      func() { faulttest.Block(nil) },
		"Reached(0)":      func() { faulttest.Sequence().Reached(0) },
		"FailAlways(nil)": func() { faulttest.FailAlways(nil) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("%s did not panic", name)
				}
			}()
			fn()
		})
	}
}

// TestResetIsolatesCallsInFlight: a call that started before Reset and ends
// after it does not count in the new generation.
func TestResetIsolatesCallsInFlight(t *testing.T) {
	inj := faulttest.BlockUntil(make(chan struct{}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- inj.Hit(ctx) }()
	<-inj.Reached(1)
	inj.Reset()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("the blocked call = %v, want context.Canceled", err)
	}
	if inj.Calls() != 0 || inj.Failures() != 0 {
		t.Fatalf("after Reset: Calls %d, Failures %d; a call from before the reset leaked into the counts", inj.Calls(), inj.Failures())
	}
}

func TestDisarmedCallsPassUncounted(t *testing.T) {
	inj := faulttest.FailOnCall(1, errBoom).Disarm()
	if got := outcomes(inj, 3); got[0] || got[1] || got[2] || inj.Calls() != 0 {
		t.Fatalf("disarmed: outcomes %v, Calls %d; want all passing, none counted", got, inj.Calls())
	}
	inj.Arm()
	if got := outcomes(inj, 2); !got[0] || got[1] {
		t.Fatalf("armed: outcomes %v; want the 1st call after Arm to fail", got)
	}
}
