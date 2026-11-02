package cache

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis stores cache values in Redis.
type Redis struct {
	client *redis.Client
}

// NewRedis wraps a go-redis client with Gombit's cache contract.
func NewRedis(client *redis.Client) *Redis {
	return &Redis{client: client}
}

// Client returns the underlying go-redis client.
func (r *Redis) Client() *redis.Client {
	if r == nil {
		return nil
	}
	return r.client
}

// Get implements Cache.
func (r *Redis) Get(ctx context.Context, key string, dst any) (bool, error) {
	payload, err := r.client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("cache: redis get %q: %w", key, err)
	}
	return true, decode(payload, dst)
}

// Set implements Cache.
func (r *Redis) Set(ctx context.Context, key string, value any, ttl time.Duration) error {
	if err := validateTTL(ttl); err != nil {
		return err
	}
	payload, err := encode(value)
	if err != nil {
		return err
	}
	if err := r.client.Set(ctx, key, payload, ttl).Err(); err != nil {
		return fmt.Errorf("cache: redis set %q: %w", key, err)
	}
	return nil
}

// Delete implements Cache.
func (r *Redis) Delete(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	if err := r.client.Del(ctx, keys...).Err(); err != nil {
		return fmt.Errorf("cache: redis delete: %w", err)
	}
	return nil
}

// Increment implements Cache.
func (r *Redis) Increment(ctx context.Context, key string, delta int64) (int64, error) {
	value, err := r.client.IncrBy(ctx, key, delta).Result()
	if err != nil {
		return 0, fmt.Errorf("cache: redis increment %q: %w", key, err)
	}
	return value, nil
}

// incrementTTLScript increments KEYS[1] by ARGV[1] and, when the key has no
// expiry (PTTL -1: just created, or written without one), gives it ARGV[2]
// milliseconds: one atomic step on the server.
var incrementTTLScript = redis.NewScript(`
local value = redis.call('INCRBY', KEYS[1], ARGV[1])
if redis.call('PTTL', KEYS[1]) == -1 then
	redis.call('PEXPIRE', KEYS[1], ARGV[2])
end
return value
`)

// decrementExistingScript takes ARGV[1] off KEYS[1] only if it exists: one
// atomic step, so a key that expired or was deleted is never recreated
// (DECRBY alone would make it, at a negative count, with no expiry).
var decrementExistingScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then
	return {0, 0}
end
return {redis.call('DECRBY', KEYS[1], ARGV[1]), 1}
`)

// DecrementExisting implements Counter.
func (r *Redis) DecrementExisting(ctx context.Context, key string, delta int64) (int64, bool, error) {
	result, err := decrementExistingScript.Run(ctx, r.client, []string{key}, delta).Int64Slice()
	if err != nil {
		return 0, false, fmt.Errorf("cache: redis decrement %q: %w", key, err)
	}
	return result[0], result[1] == 1, nil
}

// IncrementTTL implements Counter.
func (r *Redis) IncrementTTL(ctx context.Context, key string, delta int64, ttl time.Duration) (int64, error) {
	if err := validateCounterTTL(ttl); err != nil {
		return 0, err
	}
	// PEXPIRE takes whole milliseconds; less than one would expire at once.
	ms := max(ttl.Milliseconds(), 1)
	value, err := incrementTTLScript.Run(ctx, r.client, []string{key}, delta, ms).Int64()
	if err != nil {
		return 0, fmt.Errorf("cache: redis increment %q: %w", key, err)
	}
	return value, nil
}
