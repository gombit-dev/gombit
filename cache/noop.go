package cache

import (
	"context"
	"time"
)

// Noop disables persistence while preserving cache call sites.
type Noop struct{}

// Get implements Cache.
func (Noop) Get(ctx context.Context, key string, dst any) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return false, nil
}

// Set implements Cache.
func (Noop) Set(ctx context.Context, key string, value any, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return validateTTL(ttl)
}

// Delete implements Cache.
func (Noop) Delete(ctx context.Context, keys ...string) error {
	return ctx.Err()
}

// Increment implements Cache.
func (Noop) Increment(ctx context.Context, key string, delta int64) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return 0, nil
}

// DecrementExisting implements Counter: nothing exists.
func (Noop) DecrementExisting(ctx context.Context, key string, delta int64) (int64, bool, error) {
	return 0, false, ctx.Err()
}

// IncrementTTL implements Counter: it keeps nothing, as Increment.
func (Noop) IncrementTTL(ctx context.Context, key string, delta int64, ttl time.Duration) (int64, error) {
	if err := validateCounterTTL(ttl); err != nil {
		return 0, err
	}
	return Noop{}.Increment(ctx, key, delta)
}
