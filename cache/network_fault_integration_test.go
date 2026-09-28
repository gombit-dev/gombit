//go:build integration

package cache

import (
	"context"
	"errors"
	"flag"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/internal/faulttest"
)

var redisAddr = flag.String("cache.redis-addr", "", "Redis address (host:port) for cache integration tests")

// proxiedRedis is a Redis cache reached through a fault proxy.
func proxiedRedis(t *testing.T) (*Redis, *faulttest.TCPProxy) {
	t.Helper()
	if *redisAddr == "" {
		t.Skip("set -cache.redis-addr to run Redis network fault tests")
	}
	proxy := faulttest.NewTCPProxy(t, *redisAddr)
	client, err := NewRedisClient(config.RedisConfig{Addr: proxy.Addr(), DialTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return NewRedis(client), proxy
}

func withinDeadline(t *testing.T, d time.Duration, fn func(ctx context.Context) error) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	start := time.Now()
	err := fn(ctx)
	if took := time.Since(start); took > d+time.Second {
		t.Fatalf("the call took %s against a %s deadline", took, d)
	}
	return err
}

// assertRecovers: with the fault lifted, the next cache operation succeeds
// on the same client (INV-6).
func assertRecovers(t *testing.T, c *Redis, proxy *faulttest.TCPProxy) {
	t.Helper()
	if err := proxy.Heal(); err != nil {
		t.Fatal(err)
	}
	if err := withinDeadline(t, 5*time.Second, func(ctx context.Context) error {
		return c.Set(ctx, "fault:recovered", "yes", time.Minute)
	}); err != nil {
		t.Fatalf("the first cache write after recovery = %v, want success", err)
	}
}

// TestFault_Network_RedisUnavailable: Redis refusing connections (its open
// ones dropped) fails a cache call within go-redis's bounded retries: a few
// refused dials with backoff (about 2s), never a hang.
func TestFault_Network_RedisUnavailable(t *testing.T) {
	c, proxy := proxiedRedis(t)
	if err := c.Set(context.Background(), "fault:k", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	proxy.Refuse()
	proxy.Cut()
	var v int
	start := time.Now()
	_, err := c.Get(context.Background(), "fault:k", &v) // no deadline: the retry budget alone must end it
	if err == nil {
		t.Fatal("a cache read against a refusing Redis succeeded")
	}
	if took := time.Since(start); took > 4*time.Second {
		t.Fatalf("a refused Redis took %s to report; the client's retries are not bounded as expected", took)
	}
	assertRecovers(t, c, proxy)
}

// TestFault_Network_RedisLatency: a Redis that stops answering ends a cache
// call at the caller's deadline, not at the client's own read timeout.
func TestFault_Network_RedisLatency(t *testing.T) {
	c, proxy := proxiedRedis(t)
	if err := c.Set(context.Background(), "fault:k", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	proxy.Hold()
	var v int
	err := withinDeadline(t, 200*time.Millisecond, func(ctx context.Context) error {
		_, err := c.Get(ctx, "fault:k", &v)
		return err
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a stalled cache read = %v, want context.DeadlineExceeded", err)
	}
	assertRecovers(t, c, proxy)
}

// TestFault_Network_RedisConnectionLost: a connection lost mid-command, with
// Redis still unreachable, fails the command promptly.
func TestFault_Network_RedisConnectionLost(t *testing.T) {
	c, proxy := proxiedRedis(t)
	if err := c.Set(context.Background(), "fault:k", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	proxy.Hold()
	done := make(chan error, 1)
	go func() {
		var v int
		_, err := c.Get(context.Background(), "fault:k", &v)
		done <- err
	}()
	select {
	case <-proxy.Held():
	case err := <-done:
		t.Fatalf("the command returned (%v) before reaching the held Redis", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the command never reached the held Redis")
	}
	proxy.Refuse()
	proxy.Reset()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a command whose connection was reset succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a command whose connection was reset did not return")
	}
	assertRecovers(t, c, proxy)
}
