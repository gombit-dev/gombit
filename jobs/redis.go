package jobs

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// RedisQueue is a durable Queue in Redis. Queued jobs, their attempt counts,
// delays, and leases live in Redis, so they survive application and worker
// restarts, and a job whose worker died mid-run comes back when its lease
// expires.
//
// Each queue is a set of keys sharing one hash tag, {<namespace>:jobs:<queue>},
// so every operation touches a single Redis Cluster slot:
//
//	…:pending   sorted set of waiting jobs, scored by when they become available
//	…:reserved  sorted set of leased jobs, scored by lease deadline
//	…:seq       counter that orders jobs available in the same millisecond
//	…:job:<id>  hash: the envelope, attempt count, lease receipt, and member
//
// A job's member in both sets is "<16-digit sequence>:<id>", so equal scores
// order by push (or release) order. Reserve takes whichever is earlier: the
// first pending job due now, or the first lease that has expired (ready since
// its deadline), the same availability order as MemoryQueue. Each operation
// is one Lua script, so a crash between steps cannot lose or duplicate a job.
type RedisQueue struct {
	client      redis.UniversalClient
	namespace   string
	now         func() time.Time
	ownedClient bool
	closed      atomic.Bool
}

// RedisOption configures NewRedisQueue.
type RedisOption func(*RedisQueue)

// WithRedisClock sets the clock that times delays and leases (tests). Workers
// sharing a queue should have roughly synchronized clocks, as with any lease.
func WithRedisClock(now func() time.Time) RedisOption {
	return func(q *RedisQueue) { q.now = now }
}

// WithRedisClientOwned makes Close close the client too.
func WithRedisClientOwned() RedisOption {
	return func(q *RedisQueue) { q.ownedClient = true }
}

// NewRedisQueue returns a queue over client, with every key under namespace.
func NewRedisQueue(client redis.UniversalClient, namespace string, opts ...RedisOption) *RedisQueue {
	q := &RedisQueue{client: client, namespace: namespace, now: time.Now}
	for _, opt := range opts {
		opt(q)
	}
	return q
}

type redisKeys struct {
	pending, reserved, seq, jobPrefix string
}

func (q *RedisQueue) keys(queue string) redisKeys {
	base := fmt.Sprintf("{%s:jobs:%s}", q.namespace, queue)
	return redisKeys{
		pending:   base + ":pending",
		reserved:  base + ":reserved",
		seq:       base + ":seq",
		jobPrefix: base + ":job:",
	}
}

// pushScript: KEYS job, pending, seq; ARGV envelope, available-at ms, id.
// Returns 0 when the ID is already on this queue.
var pushScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 1 then return 0 end
local member = string.format('%016d', redis.call('INCR', KEYS[3])) .. ':' .. ARGV[3]
redis.call('HSET', KEYS[1], 'env', ARGV[1], 'attempts', 0, 'receipt', '', 'member', member)
redis.call('ZADD', KEYS[2], ARGV[2], member)
return 1
`)

// reserveScript: KEYS pending, reserved; ARGV now ms, lease deadline ms,
// receipt, job key prefix. Leases the earliest-available job: the first
// pending job due by now or the first expired lease, whichever became
// available first. Returns false when none is, else {id, envelope, attempts}.
var reserveScript = redis.NewScript(`
local now = tonumber(ARGV[1])
while true do
  local p = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', now, 'WITHSCORES', 'LIMIT', 0, 1)
  local r = redis.call('ZRANGEBYSCORE', KEYS[2], '-inf', now, 'WITHSCORES', 'LIMIT', 0, 1)
  local member, from
  if #p > 0 and #r > 0 then
    local ps, rs = tonumber(p[2]), tonumber(r[2])
    if rs < ps or (rs == ps and r[1] < p[1]) then member, from = r[1], KEYS[2] else member, from = p[1], KEYS[1] end
  elseif #p > 0 then member, from = p[1], KEYS[1]
  elseif #r > 0 then member, from = r[1], KEYS[2]
  else return false end
  redis.call('ZREM', from, member)
  local id = string.sub(member, 18)
  local job = ARGV[4] .. id
  if redis.call('EXISTS', job) == 1 then
    local attempts = redis.call('HINCRBY', job, 'attempts', 1)
    redis.call('HSET', job, 'receipt', ARGV[3])
    redis.call('ZADD', KEYS[2], ARGV[2], member)
    return {id, redis.call('HGET', job, 'env'), attempts}
  end
end
`)

// ackScript: KEYS job, pending, reserved; ARGV receipt. Returns 0 when the
// receipt is not the job's current lease.
var ackScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'receipt') ~= ARGV[1] then return 0 end
local member = redis.call('HGET', KEYS[1], 'member')
redis.call('ZREM', KEYS[3], member)
redis.call('ZREM', KEYS[2], member)
redis.call('DEL', KEYS[1])
return 1
`)

// releaseScript: KEYS job, pending, reserved, seq; ARGV receipt,
// available-at ms, id. Returns 0 when the receipt is not the job's current
// lease. The job goes behind others available at the same time.
var releaseScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'receipt') ~= ARGV[1] then return 0 end
redis.call('ZREM', KEYS[3], redis.call('HGET', KEYS[1], 'member'))
local member = string.format('%016d', redis.call('INCR', KEYS[4])) .. ':' .. ARGV[3]
redis.call('HSET', KEYS[1], 'receipt', '', 'member', member)
redis.call('ZADD', KEYS[2], ARGV[2], member)
return 1
`)

// extendScript: KEYS job, reserved; ARGV receipt, lease deadline ms. A lease
// that lapsed but that no Reserve took is still this receipt's, and still in
// the reserved set: it is renewed in place. Returns 0 when the receipt is
// not the job's current lease.
var extendScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'receipt') ~= ARGV[1] then return 0 end
redis.call('ZADD', KEYS[2], ARGV[2], redis.call('HGET', KEYS[1], 'member'))
return 1
`)

// Push implements Queue.
func (q *RedisQueue) Push(ctx context.Context, queue string, env Envelope, at time.Time) error {
	if q.closed.Load() {
		return ErrClosed
	}
	if !ValidName(queue) {
		return fmt.Errorf("%w: %q", ErrInvalidQueue, queue)
	}
	if env.ID == "" {
		return fmt.Errorf("jobs: push %q: envelope has no ID", env.Name)
	}
	env.Attempt = 0
	data, err := env.Marshal()
	if err != nil {
		return err
	}
	now := q.now()
	if at.IsZero() || at.Before(now) {
		at = now
	}
	k := q.keys(queue)
	added, err := pushScript.Run(ctx, q.client, []string{k.jobPrefix + env.ID, k.pending, k.seq},
		data, at.UnixMilli(), env.ID).Int()
	if err != nil {
		return fmt.Errorf("jobs: push %q to %s: %w", env.Name, queue, err)
	}
	if added == 0 {
		return fmt.Errorf("%w: %s on %s", ErrDuplicateJob, env.ID, queue)
	}
	return nil
}

// Reserve implements Queue.
func (q *RedisQueue) Reserve(ctx context.Context, queues []string, lease time.Duration) (Delivery, error) {
	if q.closed.Load() {
		return Delivery{}, ErrClosed
	}
	if lease <= 0 {
		return Delivery{}, fmt.Errorf("jobs: reserve: lease must be positive, got %s", lease)
	}
	for _, queue := range queues {
		if !ValidName(queue) {
			return Delivery{}, fmt.Errorf("%w: %q", ErrInvalidQueue, queue)
		}
	}
	for _, queue := range queues {
		now := q.now()
		receipt := uuid.NewString()
		k := q.keys(queue)
		res, err := reserveScript.Run(ctx, q.client, []string{k.pending, k.reserved},
			now.UnixMilli(), now.Add(lease).UnixMilli(), receipt, k.jobPrefix).Slice()
		if errors.Is(err, redis.Nil) {
			continue
		}
		if err != nil {
			return Delivery{}, fmt.Errorf("jobs: reserve from %s: %w", queue, err)
		}
		if len(res) != 3 {
			return Delivery{}, fmt.Errorf("jobs: reserve from %s: unexpected reply %v", queue, res)
		}
		id := fmt.Sprint(res[0])
		attempts, err := toInt(res[2])
		if err != nil {
			return Delivery{}, fmt.Errorf("jobs: reserve from %s: attempts: %w", queue, err)
		}
		raw, _ := res[1].(string)
		env, err := UnmarshalEnvelope([]byte(raw))
		if err != nil {
			// Leased, not lost: the delivery carries the failure so the
			// caller acks it rather than meet it on every lease expiry.
			return Delivery{Queue: queue, Envelope: Envelope{ID: id, Attempt: attempts}, Receipt: receipt, Err: err}, nil
		}
		env.Attempt = attempts
		return Delivery{Queue: queue, Envelope: env, Receipt: receipt}, nil
	}
	return Delivery{}, ErrNoJob
}

// Ack implements Queue.
func (q *RedisQueue) Ack(ctx context.Context, d Delivery) error {
	if q.closed.Load() {
		return ErrClosed
	}
	if d.Receipt == "" || !ValidName(d.Queue) {
		return fmt.Errorf("%w: %s", ErrLeaseLost, d.Envelope.ID)
	}
	k := q.keys(d.Queue)
	ok, err := ackScript.Run(ctx, q.client, []string{k.jobPrefix + d.Envelope.ID, k.pending, k.reserved},
		d.Receipt).Int()
	if err != nil {
		return fmt.Errorf("jobs: ack %s: %w", d.Envelope.ID, err)
	}
	if ok == 0 {
		return fmt.Errorf("%w: %s", ErrLeaseLost, d.Envelope.ID)
	}
	return nil
}

// Release implements Queue.
func (q *RedisQueue) Release(ctx context.Context, d Delivery, at time.Time) error {
	if q.closed.Load() {
		return ErrClosed
	}
	if d.Receipt == "" || !ValidName(d.Queue) {
		return fmt.Errorf("%w: %s", ErrLeaseLost, d.Envelope.ID)
	}
	now := q.now()
	if at.IsZero() || at.Before(now) {
		at = now
	}
	k := q.keys(d.Queue)
	ok, err := releaseScript.Run(ctx, q.client, []string{k.jobPrefix + d.Envelope.ID, k.pending, k.reserved, k.seq},
		d.Receipt, at.UnixMilli(), d.Envelope.ID).Int()
	if err != nil {
		return fmt.Errorf("jobs: release %s: %w", d.Envelope.ID, err)
	}
	if ok == 0 {
		return fmt.Errorf("%w: %s", ErrLeaseLost, d.Envelope.ID)
	}
	return nil
}

// Extend implements Queue.
func (q *RedisQueue) Extend(ctx context.Context, d Delivery, lease time.Duration) error {
	if q.closed.Load() {
		return ErrClosed
	}
	if lease <= 0 {
		return fmt.Errorf("jobs: extend: lease must be positive, got %s", lease)
	}
	if d.Receipt == "" || !ValidName(d.Queue) {
		return fmt.Errorf("%w: %s", ErrLeaseLost, d.Envelope.ID)
	}
	k := q.keys(d.Queue)
	ok, err := extendScript.Run(ctx, q.client, []string{k.jobPrefix + d.Envelope.ID, k.reserved},
		d.Receipt, q.now().Add(lease).UnixMilli()).Int()
	if err != nil {
		return fmt.Errorf("jobs: extend %s: %w", d.Envelope.ID, err)
	}
	if ok == 0 {
		return fmt.Errorf("%w: %s", ErrLeaseLost, d.Envelope.ID)
	}
	return nil
}

// Close implements Queue: later calls return ErrClosed. It closes the client
// only when the queue owns it.
func (q *RedisQueue) Close() error {
	if q.closed.Swap(true) || !q.ownedClient {
		return nil
	}
	return q.client.Close()
}

func toInt(v any) (int, error) {
	switch n := v.(type) {
	case int64:
		return int(n), nil
	case string:
		return strconv.Atoi(n)
	default:
		return 0, fmt.Errorf("not a number: %v", v)
	}
}

var _ Queue = (*RedisQueue)(nil)
var _ Queue = (*MemoryQueue)(nil)
