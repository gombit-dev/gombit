// Package faulttest injects deterministic faults into Gombit's own tests.
//
// A fault is expressed as "fail exactly the second call", never as a
// probability: an Injector walks a fixed list of steps, one per call, and
// every call it sees is counted, so a test can reproduce a failure path on
// every run and assert what the code under test did about it.
//
//	inj := faulttest.FailOnCall(2, errConnLost) // the 2nd call fails
//	err := inj.Do(ctx, func(ctx context.Context) error { return dep.Call(ctx) })
//
// Injectors wrap a dependency at a narrow seam: Do around a function, or the
// database/sql wrapper in this package (OpenDB, WrapConnector) for GORM and
// transactions. Nothing here is global and nothing changes production code
// paths: a test opts in by building the wrapped dependency itself.
//
// Delays and blocks wait on timers, channels, and the call's context, never
// on sleeps the test has to guess at: a test that needs a call to be in
// flight waits on Reached(n), and releases it by closing the channel it
// handed to BlockUntil (or Block).
package faulttest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrInjected is a ready-made fault for tests that do not need a specific
// error.
var ErrInjected = errors.New("faulttest: injected fault")

// Step is what one call does: succeed, fail with an error, wait, or block.
type Step struct {
	err     error
	wait    time.Duration
	release <-chan struct{}
}

// Success lets the call through.
func Success() Step { return Step{} }

// Failure fails the call with err.
func Failure(err error) Step {
	if err == nil {
		panic("faulttest: Failure needs an error")
	}
	return Step{err: err}
}

// Wait holds the call for d, then lets it through. A context that ends first
// fails the call with the context's error.
func Wait(d time.Duration) Step {
	if d < 0 {
		panic("faulttest: Wait needs a non-negative duration")
	}
	return Step{wait: d}
}

// Block holds the call until release is closed, then lets it through. A
// context that ends first fails the call with the context's error.
func Block(release <-chan struct{}) Step {
	if release == nil {
		panic("faulttest: Block needs a channel")
	}
	return Step{release: release}
}

// run applies the step to one call.
func (s Step) run(ctx context.Context) error {
	switch {
	case s.err != nil:
		return s.err
	case s.release != nil:
		select {
		case <-s.release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	case s.wait > 0:
		timer := time.NewTimer(s.wait)
		defer timer.Stop()
		select {
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// Injector decides, call by call, whether a call fails. It is safe for
// concurrent use: calls are numbered in the order they reach it. A nil
// *Injector lets every call through, so an optional fault can be left
// unset.
type Injector struct {
	mu       sync.Mutex
	steps    []Step // the first calls, in order
	rest     Step   // every call after them
	calls    int
	failures int
	reached  map[int]chan struct{}
	// gen is bumped by Reset: a call that started before it finishes
	// without touching the new counts.
	gen int
	// disarmed lets calls through uncounted (OpenDB, while GORM opens).
	disarmed bool
}

func newInjector(rest Step, steps ...Step) *Injector {
	return &Injector{steps: steps, rest: rest}
}

// FailAlways fails every call with err.
func FailAlways(err error) *Injector { return newInjector(Failure(err)) }

// FailOnce fails the first call with err and lets the rest through.
func FailOnce(err error) *Injector { return FailNTimes(1, err) }

// FailNTimes fails the first n calls with err and lets the rest through.
func FailNTimes(n int, err error) *Injector {
	if n < 1 {
		panic(fmt.Sprintf("faulttest: FailNTimes(%d): n must be at least 1", n))
	}
	steps := make([]Step, n)
	for i := range steps {
		steps[i] = Failure(err)
	}
	return newInjector(Success(), steps...)
}

// FailOnCall fails only the nth call (1-based) with err.
func FailOnCall(n int, err error) *Injector {
	if n < 1 {
		panic(fmt.Sprintf("faulttest: FailOnCall(%d): calls are numbered from 1", n))
	}
	steps := make([]Step, n)
	for i := range steps[:n-1] {
		steps[i] = Success()
	}
	steps[n-1] = Failure(err)
	return newInjector(Success(), steps...)
}

// Delay holds every call for d (see Wait).
func Delay(d time.Duration) *Injector { return newInjector(Wait(d)) }

// BlockUntil holds every call until release is closed (see Block).
func BlockUntil(release <-chan struct{}) *Injector { return newInjector(Block(release)) }

// Sequence applies steps to the first calls, one each, and lets every call
// after them through.
func Sequence(steps ...Step) *Injector { return newInjector(Success(), steps...) }

// Hit counts a call and applies its step: nil to let it through, or the
// error it fails with.
func (i *Injector) Hit(ctx context.Context) error {
	if i == nil {
		return nil
	}
	i.mu.Lock()
	if i.disarmed {
		i.mu.Unlock()
		return nil
	}
	i.calls++
	n, gen := i.calls, i.gen
	step := i.rest
	if n <= len(i.steps) {
		step = i.steps[n-1]
	}
	if ch, ok := i.reached[n]; ok {
		close(ch)
		delete(i.reached, n)
	}
	i.mu.Unlock()

	err := step.run(ctx)
	if err != nil {
		i.mu.Lock()
		if i.gen == gen {
			i.failures++
		}
		i.mu.Unlock()
	}
	return err
}

// Do runs fn unless the call fails: Hit, then fn.
func (i *Injector) Do(ctx context.Context, fn func(context.Context) error) error {
	if err := i.Hit(ctx); err != nil {
		return err
	}
	return fn(ctx)
}

// Calls reports how many calls reached the injector.
func (i *Injector) Calls() int {
	if i == nil {
		return 0
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.calls
}

// Failures reports how many calls it failed (a context that ended during a
// wait or block included).
func (i *Injector) Failures() int {
	if i == nil {
		return 0
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.failures
}

// Reached returns a channel closed once call n (1-based) has reached the
// injector, before its step runs: a test waits on it to know a blocked call
// is in flight.
func (i *Injector) Reached(n int) <-chan struct{} {
	if n < 1 {
		panic(fmt.Sprintf("faulttest: Reached(%d): calls are numbered from 1", n))
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	ch := make(chan struct{})
	if n <= i.calls {
		close(ch)
		return ch
	}
	if i.reached == nil {
		i.reached = map[int]chan struct{}{}
	}
	if existing, ok := i.reached[n]; ok {
		return existing
	}
	i.reached[n] = ch
	return ch
}

// suspend lets calls through uncounted until the returned restore, which
// puts back the armed state from before and starts the injector over.
func (i *Injector) suspend() (restore func()) {
	if i == nil {
		return func() {}
	}
	i.mu.Lock()
	was := i.disarmed
	i.disarmed = true
	i.mu.Unlock()
	return func() {
		i.mu.Lock()
		defer i.mu.Unlock()
		i.disarmed = was
		i.calls, i.failures = 0, 0
		i.gen++
	}
}

// Reset starts the injector over: counts go to zero and the next call is
// call 1 again. A call still in flight from before (a blocked one, say)
// finishes as it would have, but no longer counts. Channels from Reached
// for calls not yet reached stay pending, numbered from the new start.
func (i *Injector) Reset() {
	if i == nil {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.calls, i.failures = 0, 0
	i.gen++
}
