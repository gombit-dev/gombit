package cache

import (
	"context"
	"strings"
	"time"
)

type namespaced struct {
	namespace string
	next      Cache
}

// Namespace prefixes all keys before forwarding operations to next.
func Namespace(namespace string, next Cache) Cache {
	namespace = strings.Trim(namespace, ":")
	if namespace == "" {
		return next
	}
	return namespaced{namespace: namespace, next: next}
}

func (n namespaced) Get(ctx context.Context, key string, dst any) (bool, error) {
	return n.next.Get(ctx, n.key(key), dst)
}

func (n namespaced) Set(ctx context.Context, key string, value any, ttl time.Duration) error {
	return n.next.Set(ctx, n.key(key), value, ttl)
}

func (n namespaced) Delete(ctx context.Context, keys ...string) error {
	namespacedKeys := make([]string, len(keys))
	for i, key := range keys {
		namespacedKeys[i] = n.key(key)
	}
	return n.next.Delete(ctx, namespacedKeys...)
}

func (n namespaced) Increment(ctx context.Context, key string, delta int64) (int64, error) {
	return n.next.Increment(ctx, n.key(key), delta)
}

// DecrementExisting implements Counter, through next's when it has one.
func (n namespaced) DecrementExisting(ctx context.Context, key string, delta int64) (int64, bool, error) {
	return DecrementExisting(ctx, n.next, n.key(key), delta)
}

// IncrementTTL implements Counter, through next's when it has one.
func (n namespaced) IncrementTTL(ctx context.Context, key string, delta int64, ttl time.Duration) (int64, error) {
	return IncrementTTL(ctx, n.next, n.key(key), delta, ttl)
}

func (n namespaced) key(key string) string {
	return n.namespace + ":" + key
}
