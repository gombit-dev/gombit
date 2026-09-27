package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

func newToken() string { return uuid.NewString() }

// UniqueKey is a dispatch's uniqueness claim: while it is held, dispatching
// another job with the same key to the same queue is refused.
type UniqueKey struct {
	Key string
	// TTL bounds the claim. For Unique it is a safety net (a job that
	// vanishes without finishing cannot hold its key forever); for
	// UniqueFor it is the deduplication window.
	TTL time.Duration
	// UntilDone releases the claim when the job is acknowledged or given up
	// on, so the next dispatch after it finishes goes through.
	UntilDone bool
}

// ErrDuplicateDispatch: Dispatch refused a job whose uniqueness key is held.
var ErrDuplicateDispatch = errors.New("jobs: a job with this uniqueness key is already queued")

// DuplicateError reports the job that holds the key.
type DuplicateError struct {
	Queue string
	Key   string
	// HolderID is the ID of the job that holds the key.
	HolderID string
}

func (e *DuplicateError) Error() string {
	return fmt.Sprintf("jobs: job %s already holds uniqueness key %q on %s", e.HolderID, e.Key, e.Queue)
}

// Is matches ErrDuplicateDispatch.
func (e *DuplicateError) Is(target error) bool { return target == ErrDuplicateDispatch }

// Unique makes the dispatch refuse to queue a second job with key on the
// same queue while one is queued, delayed, or running: "only one job with
// this key at a time". The key is released when that job succeeds or is
// given up on, and in any case after ttl, so a job that disappears cannot
// block the key forever. A refused Dispatch returns a *DuplicateError
// (errors.Is ErrDuplicateDispatch) naming the holder.
func Unique(key string, ttl time.Duration) DispatchOption {
	return func(c *dispatchConfig) { c.unique = &UniqueKey{Key: key, TTL: ttl, UntilDone: true} }
}

// UniqueFor makes the dispatch refuse a second job with key on the same
// queue for window after this one, whether or not it has run: a
// deduplication window.
func UniqueFor(key string, window time.Duration) DispatchOption {
	return func(c *dispatchConfig) { c.unique = &UniqueKey{Key: key, TTL: window} }
}

func (u UniqueKey) validate() error {
	if u.Key == "" {
		return errors.New("jobs: empty uniqueness key")
	}
	if u.TTL <= 0 {
		return fmt.Errorf("jobs: uniqueness key %q: TTL must be positive, got %s", u.Key, u.TTL)
	}
	return nil
}

// OnceState is what BeginOnce found for a key.
type OnceState int

const (
	// OnceAcquired: the caller holds the key's lock and runs the effect.
	OnceAcquired OnceState = iota
	// OnceDone: the effect already completed.
	OnceDone
	// OnceBusy: another run holds the lock.
	OnceBusy
)

// OnceStore records side effects that must not repeat (Once). The durable
// queues implement it on the same storage as the jobs.
type OnceStore interface {
	// BeginOnce reports the key's state and, when it is free, takes its lock
	// for lock under token.
	BeginOnce(ctx context.Context, key, token string, lock time.Duration) (OnceState, error)
	// FinishOnce marks the key done for keep, if token still holds it.
	FinishOnce(ctx context.Context, key, token string, keep time.Duration) error
	// AbandonOnce releases the lock, if token still holds it.
	AbandonOnce(ctx context.Context, key, token string) error
}

// ErrInProgress: Once found another run of the same effect in progress. It
// is an ordinary (retryable) failure: the job runs again after its backoff,
// by which time the other run has finished or its lock expired.
var ErrInProgress = errors.New("jobs: this effect is already in progress")

// OnceOption configures Once.
type OnceOption func(*onceConfig)

type onceConfig struct {
	keep, lock time.Duration
}

// Defaults for Once.
const (
	DefaultOnceKeep = 7 * 24 * time.Hour
	DefaultOnceLock = 15 * time.Minute
)

// KeepFor sets how long Once remembers a completed effect (default 7 days):
// longer than any job carrying the key can still be redelivered.
func KeepFor(d time.Duration) OnceOption { return func(c *onceConfig) { c.keep = d } }

// LockFor sets how long a run holds the key before another may take over
// (default 15m): longer than the effect takes, so a live run is not joined,
// and short enough that a crashed run does not block the key for long.
func LockFor(d time.Duration) OnceOption { return func(c *onceConfig) { c.lock = d } }

type onceStoreKey struct{}

// withOnceStore makes Once in ctx use store.
func withOnceStore(ctx context.Context, store OnceStore) context.Context {
	return context.WithValue(ctx, onceStoreKey{}, store)
}

// Once runs fn unless an effect with key already completed, so a job that is
// delivered again (at-least-once) does not repeat its side effect. Key it on
// the job and the effect: "welcome-email:" + info.ID.
//
//   - Done before: Once returns nil without running fn.
//   - In progress in another run: Once returns ErrInProgress (retried).
//   - Otherwise: fn runs under the key's lock. Success marks the key done;
//     an error releases the lock, so a retry runs fn again.
//
// A run that dies between fn's success and the record keeps the lock until
// LockFor, after which a redelivery runs fn again; so does a run whose effect
// outlasts LockFor (size LockFor above the effect's duration; the lock is not
// renewed). Once narrows duplicate effects to those windows, it does not make
// them impossible. A record that fails after fn succeeded still returns nil:
// failing would retry the job and repeat the effect. For effects that
// must be exactly-once, make the effect itself idempotent (a unique
// constraint, an idempotency key the other system honors).
//
// Outside a worker (the sync driver, a test) there is no store, and fn just
// runs.
func Once(ctx context.Context, key string, fn func(context.Context) error, opts ...OnceOption) error {
	if key == "" {
		return errors.New("jobs: Once: empty key")
	}
	cfg := onceConfig{keep: DefaultOnceKeep, lock: DefaultOnceLock}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.keep <= 0 || cfg.lock <= 0 {
		return errors.New("jobs: Once: KeepFor and LockFor must be positive")
	}
	store, _ := ctx.Value(onceStoreKey{}).(OnceStore)
	if store == nil {
		return fn(ctx)
	}
	token := newToken()
	state, err := store.BeginOnce(ctx, key, token, cfg.lock)
	if err != nil {
		return fmt.Errorf("jobs: Once %q: %w", key, err)
	}
	switch state {
	case OnceDone:
		return nil
	case OnceBusy:
		return fmt.Errorf("%w: %q", ErrInProgress, key)
	}
	if err := fn(ctx); err != nil {
		abandonCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), queueOpTimeout)
		defer cancel()
		if abandonErr := store.AbandonOnce(abandonCtx, key, token); abandonErr != nil {
			return errors.Join(err, fmt.Errorf("jobs: Once %q: release: %w", key, abandonErr))
		}
		return err
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), queueOpTimeout)
	defer cancel()
	// The effect ran. If recording it fails, failing the job would only
	// retry it and repeat the effect; succeed instead. The unrecorded key
	// stays locked until LockFor, so a concurrent delivery still waits.
	_ = store.FinishOnce(finishCtx, key, token, cfg.keep)
	return nil
}
