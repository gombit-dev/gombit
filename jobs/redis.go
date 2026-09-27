package jobs

import (
	"context"
	"errors"
	"fmt"
	"strconv"
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
//	…:ready     list of job IDs available now (LPUSH in, RPOP out: FIFO)
//	…:delayed   sorted set of job IDs by the time they become available
//	…:reserved  sorted set of leased job IDs by lease deadline
//	…:job:<id>  hash: the envelope, attempt count, and current lease receipt
//
// Each operation is one Lua script, so a crash between steps cannot lose or
// duplicate a job.
type RedisQueue struct {
	client      redis.UniversalClient
	namespace   string
	now         func() time.Time
	ownedClient bool
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
	ready, delayed, reserved, jobPrefix string
}

func (q *RedisQueue) keys(queue string) redisKeys {
	base := fmt.Sprintf("{%s:jobs:%s}", q.namespace, queue)
	return redisKeys{
		ready:     base + ":ready",
		delayed:   base + ":delayed",
		reserved:  base + ":reserved",
		jobPrefix: base + ":job:",
	}
}

// pushScript: KEYS job, ready, delayed; ARGV envelope, available-at ms, now
// ms, id. Returns 0 when the ID is already queued.
var pushScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 1 then return 0 end
redis.call('HSET', KEYS[1], 'env', ARGV[1], 'attempts', 0, 'receipt', '')
if tonumber(ARGV[2]) <= tonumber(ARGV[3]) then
  redis.call('LPUSH', KEYS[2], ARGV[4])
else
  redis.call('ZADD', KEYS[3], ARGV[2], ARGV[4])
end
return 1
`)

// reserveScript: KEYS ready, delayed, reserved; ARGV now ms, lease deadline
// ms, receipt, job key prefix. Promotes due delayed jobs and reclaims expired
// leases, then leases the oldest ready job. Returns false when none is
// available, else {id, envelope, attempts}.
var reserveScript = redis.NewScript(`
local now = ARGV[1]
-- Due delayed jobs became available before anything pushed since, so they
-- go to the RPOP end, earliest last so it pops first.
local due = redis.call('ZRANGEBYSCORE', KEYS[2], '-inf', now, 'LIMIT', 0, 100)
for i = #due, 1, -1 do
  redis.call('ZREM', KEYS[2], due[i])
  redis.call('RPUSH', KEYS[1], due[i])
end
local expired = redis.call('ZRANGEBYSCORE', KEYS[3], '-inf', now, 'LIMIT', 0, 100)
for _, id in ipairs(expired) do
  redis.call('ZREM', KEYS[3], id)
  redis.call('RPUSH', KEYS[1], id)
end
while true do
  local id = redis.call('RPOP', KEYS[1])
  if not id then return false end
  local job = ARGV[4] .. id
  if redis.call('EXISTS', job) == 1 then
    local attempts = redis.call('HINCRBY', job, 'attempts', 1)
    redis.call('HSET', job, 'receipt', ARGV[3])
    redis.call('ZADD', KEYS[3], ARGV[2], id)
    return {id, redis.call('HGET', job, 'env'), attempts}
  end
end
`)

// ackScript: KEYS job, ready, reserved; ARGV id, receipt. Returns 0 when
// the receipt is not the job's current lease.
var ackScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'receipt') ~= ARGV[2] then return 0 end
redis.call('ZREM', KEYS[3], ARGV[1])
redis.call('LREM', KEYS[2], 0, ARGV[1])
redis.call('DEL', KEYS[1])
return 1
`)

// releaseScript: KEYS job, ready, delayed, reserved; ARGV id, receipt,
// available-at ms, now ms. Returns 0 when the receipt is not the job's
// current lease.
var releaseScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'receipt') ~= ARGV[2] then return 0 end
redis.call('ZREM', KEYS[4], ARGV[1])
redis.call('LREM', KEYS[2], 0, ARGV[1])
redis.call('HSET', KEYS[1], 'receipt', '')
if tonumber(ARGV[3]) <= tonumber(ARGV[4]) then
  redis.call('LPUSH', KEYS[2], ARGV[1])
else
  redis.call('ZADD', KEYS[3], ARGV[3], ARGV[1])
end
return 1
`)

// Push implements Queue.
func (q *RedisQueue) Push(ctx context.Context, queue string, env Envelope, at time.Time) error {
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
	added, err := pushScript.Run(ctx, q.client, []string{k.jobPrefix + env.ID, k.ready, k.delayed},
		data, at.UnixMilli(), now.UnixMilli(), env.ID).Int()
	if err != nil {
		return fmt.Errorf("jobs: push %q to %s: %w", env.Name, queue, err)
	}
	if added == 0 {
		return fmt.Errorf("%w: %s", ErrDuplicateJob, env.ID)
	}
	return nil
}

// Reserve implements Queue.
func (q *RedisQueue) Reserve(ctx context.Context, queues []string, lease time.Duration) (Delivery, error) {
	if lease <= 0 {
		return Delivery{}, fmt.Errorf("jobs: reserve: lease must be positive, got %s", lease)
	}
	for _, queue := range queues {
		if !ValidName(queue) {
			return Delivery{}, fmt.Errorf("%w: %q", ErrInvalidQueue, queue)
		}
		now := q.now()
		receipt := uuid.NewString()
		k := q.keys(queue)
		res, err := reserveScript.Run(ctx, q.client, []string{k.ready, k.delayed, k.reserved},
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
		raw, _ := res[1].(string)
		env, err := UnmarshalEnvelope([]byte(raw))
		if err != nil {
			// A stored envelope that no longer decodes. The job is leased, not
			// lost; the Delivery (ID and receipt) comes back with the decode
			// error so the caller can Ack it away or set it aside rather than
			// let it return on every lease expiry.
			return Delivery{Queue: queue, Envelope: Envelope{ID: fmt.Sprint(res[0])}, Receipt: receipt}, err
		}
		attempts, err := toInt(res[2])
		if err != nil {
			return Delivery{}, fmt.Errorf("jobs: reserve from %s: attempts: %w", queue, err)
		}
		env.Attempt = attempts
		return Delivery{Queue: queue, Envelope: env, Receipt: receipt}, nil
	}
	return Delivery{}, ErrNoJob
}

// Ack implements Queue.
func (q *RedisQueue) Ack(ctx context.Context, d Delivery) error {
	if d.Receipt == "" || !ValidName(d.Queue) {
		return fmt.Errorf("%w: %s", ErrLeaseLost, d.Envelope.ID)
	}
	k := q.keys(d.Queue)
	ok, err := ackScript.Run(ctx, q.client, []string{k.jobPrefix + d.Envelope.ID, k.ready, k.reserved},
		d.Envelope.ID, d.Receipt).Int()
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
	if d.Receipt == "" || !ValidName(d.Queue) {
		return fmt.Errorf("%w: %s", ErrLeaseLost, d.Envelope.ID)
	}
	now := q.now()
	if at.IsZero() || at.Before(now) {
		at = now
	}
	k := q.keys(d.Queue)
	ok, err := releaseScript.Run(ctx, q.client, []string{k.jobPrefix + d.Envelope.ID, k.ready, k.delayed, k.reserved},
		d.Envelope.ID, d.Receipt, at.UnixMilli(), now.UnixMilli()).Int()
	if err != nil {
		return fmt.Errorf("jobs: release %s: %w", d.Envelope.ID, err)
	}
	if ok == 0 {
		return fmt.Errorf("%w: %s", ErrLeaseLost, d.Envelope.ID)
	}
	return nil
}

// Close implements Queue. It closes the client only when the queue owns it.
func (q *RedisQueue) Close() error {
	if !q.ownedClient {
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
