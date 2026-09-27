package jobs

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"time"
)

// Options is a job's execution policy, set with WithOptions at Register (or
// for every job with WithDefaultOptions). Zero fields take the registry's
// defaults.
type Options struct {
	// MaxAttempts is how many times the job runs before the worker gives up
	// on it, the first run included. Default 5.
	MaxAttempts int
	// Timeout bounds one attempt: the handler's context is canceled when it
	// runs out, and the attempt fails as KindTimeout. Zero means no timeout.
	Timeout time.Duration
	// Backoff is how long a failed job waits before its next attempt.
	// Default: Exponential(10s, 10m).
	Backoff Backoff
}

// Backoff returns how long to wait after the attempt-th failure (1-based)
// before the next attempt.
type Backoff func(attempt int) time.Duration

// Defaults for Options.
const (
	DefaultMaxAttempts = 5
	defaultBackoffBase = 10 * time.Second
	defaultBackoffMax  = 10 * time.Minute
)

// DefaultBackoff is Exponential(10s, 10m).
var DefaultBackoff = Exponential(defaultBackoffBase, defaultBackoffMax)

// Exponential waits base after the first failure and doubles per failure
// after that, up to max. A zero base retries at once, burning attempts
// quickly; that is rarely what an outage needs.
func Exponential(base, max time.Duration) Backoff {
	return func(attempt int) time.Duration {
		wait := base
		for i := 1; i < attempt && wait < max; i++ {
			if wait > max/2 {
				// Doubling past max (or past the int64 range, for a huge max)
				// would overflow; max is the answer from here on.
				wait = max
				break
			}
			wait *= 2
		}
		if wait > max {
			wait = max
		}
		return wait
	}
}

// Constant waits d after every failure.
func Constant(d time.Duration) Backoff {
	return func(int) time.Duration { return d }
}

// Jittered spreads b's delays over [50%, 100%] of their value, so jobs that
// failed together (an outage) do not all retry in the same instant.
func Jittered(b Backoff) Backoff {
	return func(attempt int) time.Duration {
		d := b(attempt)
		if d <= 1 {
			return d
		}
		half := d / 2
		return half + time.Duration(rand.Int64N(int64(d-half)+1)) // #nosec G404 -- jitter, not security
	}
}

// WithOptions sets the job's execution policy.
func WithOptions(opts Options) RegisterOption {
	return func(c *registerConfig) { c.options = &opts }
}

// WithDefaultOptions sets the policy of every job registered without its
// own, and fills the zero fields of those registered with one.
func WithDefaultOptions(opts Options) RegistryOption {
	return func(r *Registry) { r.defaults = opts }
}

func (o Options) validate() error {
	if o.MaxAttempts < 0 {
		return fmt.Errorf("options: MaxAttempts %d is negative", o.MaxAttempts)
	}
	if o.Timeout < 0 {
		return fmt.Errorf("options: Timeout %s is negative", o.Timeout)
	}
	return nil
}

// resolve fills opts' zero fields from defaults, then from the package
// defaults.
func (o Options) resolve(defaults Options) Options {
	if o.MaxAttempts == 0 {
		o.MaxAttempts = defaults.MaxAttempts
	}
	if o.MaxAttempts == 0 {
		o.MaxAttempts = DefaultMaxAttempts
	}
	if o.Timeout == 0 {
		o.Timeout = defaults.Timeout
	}
	if o.Backoff == nil {
		o.Backoff = defaults.Backoff
	}
	if o.Backoff == nil {
		o.Backoff = DefaultBackoff
	}
	return o
}

// permanentError marks a failure no retry can fix.
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// Permanent marks err as a failure no retry can fix (the record is gone, the
// input is invalid): the worker gives up on the job at once instead of
// retrying it. Permanent(nil) is nil.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}

// IsPermanent reports whether a Run failure should not be retried: the
// handler marked it Permanent, or the job cannot be decoded (retrying the
// same bytes cannot help).
func IsPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p) || Classify(err) == KindDecode
}
