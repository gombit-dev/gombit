package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// Memory stores cache values in process memory.
type Memory struct {
	mu             sync.RWMutex
	items          map[string]memoryItem
	now            func() time.Time
	sweepBatchSize int

	closeOnce sync.Once
	stop      chan struct{}
	done      chan struct{}
}

type memoryItem struct {
	payload   []byte
	expiresAt time.Time
}

// memoryJanitorInterval is the sweep period cache.Open uses for the memory
// driver's default janitor. Fixed rather than configurable: this closes a
// correctness gap (#199), not a new tunable.
const memoryJanitorInterval = time.Minute

// memorySweepBatchSize bounds how many entries sweepExpired inspects while
// holding the write lock in one acquisition. A full-map scan under a single
// held lock would block every Get/Set/Delete/Increment for the whole scan —
// exactly the high-cardinality workload #199 is about (rate limiters,
// one-time tokens) — so the sweep releases and reacquires the lock between
// bounded batches instead, the same reason Redis's own active-expire cycle
// samples in small batches rather than scanning its keyspace in one shot.
const memorySweepBatchSize = 1000

// MemoryOption configures a Memory cache created by NewMemory.
type MemoryOption func(*Memory)

// WithJanitor starts a background goroutine that sweeps expired entries
// every interval, stopped by Close. Without this option, Memory never runs
// a goroutine and only reclaims an expired key when that same key is read
// again via Get — the caller must opt in for proactive reclamation of
// write-heavy, rarely-re-read keys (rate limits, one-time tokens, nonces).
//
// Close must be called to stop the goroutine. cache.Open wires this
// automatically for the memory driver, and framework.App closes it on
// shutdown for a cache the App opened itself. If you construct a Memory
// with WithJanitor yourself and hand it to framework.WithCache (or keep it
// outside framework.App entirely), framework.App does not close a cache it
// did not open — you are responsible for calling Close, or the goroutine
// (and the Memory it holds) leaks for the life of the process.
func WithJanitor(interval time.Duration) MemoryOption {
	return func(m *Memory) {
		m.stop = make(chan struct{})
		m.done = make(chan struct{})
		go m.runJanitor(interval)
	}
}

// NewMemory creates an empty in-process cache.
func NewMemory(opts ...MemoryOption) *Memory {
	m := &Memory{
		items:          make(map[string]memoryItem),
		now:            time.Now,
		sweepBatchSize: memorySweepBatchSize,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Close stops the janitor goroutine started by WithJanitor, if any. Safe to
// call on a Memory with no janitor, and safe to call more than once.
func (m *Memory) Close() error {
	if m.stop == nil {
		return nil
	}
	m.closeOnce.Do(func() {
		close(m.stop)
		<-m.done
	})
	return nil
}

func (m *Memory) runJanitor(interval time.Duration) {
	defer close(m.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			m.sweepExpired()
		}
	}
}

// sweepExpired removes every currently-expired entry. It is the same check
// Get already applies to a single key, just run across the whole map; Set,
// Increment, and Delete never call it on their own, only the janitor (or a
// test) does.
//
// The keys are snapshotted once under a read lock (which still lets
// concurrent Get calls through, only Set/Delete/Increment wait for it), then
// deleted in bounded batches, each under its own short write-lock
// acquisition. A single Lock held for a full scan-and-delete pass would
// block every Get/Set/Delete/Increment call for as long as the pass takes —
// exactly the high-cardinality workload #199 is about (rate limiters,
// one-time tokens) — so no individual lock acquisition here is proportional
// to the whole map's size. A key already deleted or refreshed by another
// goroutine between the snapshot and its batch is re-checked against the
// live map, not the snapshot, so a fresh Set of a since-reused key is never
// wrongly evicted.
func (m *Memory) sweepExpired() int {
	now := m.now()

	m.mu.RLock()
	keys := make([]string, 0, len(m.items))
	for key := range m.items {
		keys = append(keys, key)
	}
	m.mu.RUnlock()

	batches := 0
	for start := 0; start < len(keys); start += m.sweepBatchSize {
		end := min(start+m.sweepBatchSize, len(keys))
		m.sweepBatch(now, keys[start:end])
		batches++
	}
	return batches
}

// sweepBatch deletes the entries in keys that are expired as of now, under a
// single lock acquisition. It re-reads each key's current value rather than
// trusting a caller-supplied snapshot, so a key set again after the snapshot
// that fed keys is left alone.
func (m *Memory) sweepBatch(now time.Time, keys []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, key := range keys {
		if item, ok := m.items[key]; ok && item.expired(now) {
			delete(m.items, key)
		}
	}
}

// Get implements Cache.
func (m *Memory) Get(ctx context.Context, key string, dst any) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	m.mu.RLock()
	item, ok := m.items[key]
	m.mu.RUnlock()
	if !ok {
		return false, nil
	}
	if item.expired(m.now()) {
		m.mu.Lock()
		if current, ok := m.items[key]; ok && current.expired(m.now()) {
			delete(m.items, key)
		}
		m.mu.Unlock()
		return false, nil
	}

	return true, decode(item.payload, dst)
}

// Set implements Cache.
func (m *Memory) Set(ctx context.Context, key string, value any, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateTTL(ttl); err != nil {
		return err
	}
	payload, err := encode(value)
	if err != nil {
		return err
	}

	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = m.now().Add(ttl)
	}

	m.mu.Lock()
	m.items[key] = memoryItem{payload: payload, expiresAt: expiresAt}
	m.mu.Unlock()
	return nil
}

// Delete implements Cache.
func (m *Memory) Delete(ctx context.Context, keys ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	m.mu.Lock()
	for _, key := range keys {
		delete(m.items, key)
	}
	m.mu.Unlock()
	return nil
}

// Increment implements Cache.
func (m *Memory) Increment(ctx context.Context, key string, delta int64) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	var current int64
	if item, ok := m.items[key]; ok && !item.expired(m.now()) {
		if err := json.Unmarshal(item.payload, &current); err != nil {
			return 0, fmt.Errorf("cache: increment %q: stored value is not an integer: %w", key, err)
		}
	}

	current += delta
	payload, err := encode(current)
	if err != nil {
		return 0, err
	}
	expiresAt := time.Time{}
	if item, ok := m.items[key]; ok && !item.expired(m.now()) {
		expiresAt = item.expiresAt
	}
	m.items[key] = memoryItem{payload: payload, expiresAt: expiresAt}
	return current, nil
}

func (i memoryItem) expired(now time.Time) bool {
	return !i.expiresAt.IsZero() && !now.Before(i.expiresAt)
}
