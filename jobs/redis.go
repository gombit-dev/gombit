package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
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
//	…:failed    sorted set of jobs a worker gave up on (IDs), by when
//	…:seq       counter that orders jobs available in the same millisecond
//	…:job:<id>  hash: the envelope, attempt count, lease receipt, and member
//	…:unique:<key>  the ID of the job holding a uniqueness key, with a TTL
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

// RedisClientOptions adapts go-redis options for a job queue's client:
//
//   - ContextTimeoutEnabled, so a call's context deadline bounds it (go-redis
//     otherwise runs every call on context.Background() and only the socket
//     timeouts apply), which is what the worker's shutdown budget relies on;
//   - no retries (MaxRetries -1): every queue operation is one Lua script, and
//     retrying one whose reply was lost could lease a second job or report a
//     pushed job as a duplicate.
//
// Open and OpenWithRedis build their clients with it; a caller of
// NewRedisQueue should too.
func RedisClientOptions(base *redis.Options) *redis.Options {
	opts := *base
	opts.ContextTimeoutEnabled = true
	opts.MaxRetries = -1
	return &opts
}

// NewRedisQueue returns a queue over client, with every key under namespace.
// Build client with RedisClientOptions.
func NewRedisQueue(client redis.UniversalClient, namespace string, opts ...RedisOption) *RedisQueue {
	q := &RedisQueue{client: client, namespace: namespace, now: time.Now}
	for _, opt := range opts {
		opt(q)
	}
	return q
}

type redisKeys struct {
	pending, reserved, failed, seq, jobPrefix, unique string
}

func (q *RedisQueue) keys(queue string) redisKeys {
	base := fmt.Sprintf("{%s:jobs:%s}", q.namespace, queue)
	return redisKeys{
		pending:   base + ":pending",
		reserved:  base + ":reserved",
		failed:    base + ":failed",
		seq:       base + ":seq",
		jobPrefix: base + ":job:",
		unique:    base + ":unique:",
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

// pushUniqueScript: KEYS job, pending, seq, unique; ARGV envelope,
// available-at ms, id, claim ttl ms, until-done ('1' or '0'). Returns
// {1, ”} when pushed, {0, holder} when the uniqueness key is held, and
// {-1, ”} when the ID is already on this queue.
var pushUniqueScript = redis.NewScript(`
local holder = redis.call('GET', KEYS[4])
if holder then return {0, holder} end
if redis.call('EXISTS', KEYS[1]) == 1 then return {-1, ''} end
redis.call('SET', KEYS[4], ARGV[3], 'PX', ARGV[4])
local member = string.format('%016d', redis.call('INCR', KEYS[3])) .. ':' .. ARGV[3]
redis.call('HSET', KEYS[1], 'env', ARGV[1], 'attempts', 0, 'receipt', '', 'member', member)
if ARGV[5] == '1' then redis.call('HSET', KEYS[1], 'unique', KEYS[4], 'unique_ttl', ARGV[4]) end
redis.call('ZADD', KEYS[2], ARGV[2], member)
return {1, ''}
`)

// releaseUniqueLua drops the uniqueness claim a job holds until done, if it
// still holds it. It expects the job key in KEYS[1] and the job ID in `id`.
// The claim key shares the queue's hash tag.
const releaseUniqueLua = `
local unique = redis.call('HGET', KEYS[1], 'unique')
if unique and unique ~= '' and redis.call('GET', unique) == id then redis.call('DEL', unique) end
`

// reserveScript: KEYS pending, reserved; ARGV now ms, lease deadline ms,
// receipt, job key prefix. Leases the earliest-available job: the first
// pending job due by now or the first expired lease, whichever became
// available first. Returns false when none is, else {id, envelope, attempts,
// available-since ms}.
var reserveScript = redis.NewScript(`
local now = tonumber(ARGV[1])
while true do
  local p = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', now, 'WITHSCORES', 'LIMIT', 0, 1)
  local r = redis.call('ZRANGEBYSCORE', KEYS[2], '-inf', now, 'WITHSCORES', 'LIMIT', 0, 1)
  local member, from, since
  if #p > 0 and #r > 0 then
    local ps, rs = tonumber(p[2]), tonumber(r[2])
    if rs < ps or (rs == ps and r[1] < p[1]) then member, from, since = r[1], KEYS[2], r[2] else member, from, since = p[1], KEYS[1], p[2] end
  elseif #p > 0 then member, from, since = p[1], KEYS[1], p[2]
  elseif #r > 0 then member, from, since = r[1], KEYS[2], r[2]
  else return false end
  redis.call('ZREM', from, member)
  local id = string.sub(member, 18)
  local job = ARGV[4] .. id
  if redis.call('EXISTS', job) == 1 then
    local attempts = redis.call('HINCRBY', job, 'attempts', 1)
    redis.call('HSET', job, 'receipt', ARGV[3])
    redis.call('ZADD', KEYS[2], ARGV[2], member)
    return {id, redis.call('HGET', job, 'env'), attempts, since}
  end
end
`)

// ackScript: KEYS job, pending, reserved; ARGV receipt. Returns 0 when the
// receipt is not the job's current lease.
var ackScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'receipt') ~= ARGV[1] then return 0 end
local member = redis.call('HGET', KEYS[1], 'member')
local id = string.sub(member, 18)
` + releaseUniqueLua + `
redis.call('ZREM', KEYS[3], member)
redis.call('ZREM', KEYS[2], member)
redis.call('DEL', KEYS[1])
return 1
`)

// releaseScript: KEYS job, pending, reserved, seq; ARGV receipt,
// available-at ms, id, and '1' to take back the delivery's attempt
// (Postpone). Returns 0 when the receipt is not the job's current lease. The
// job goes behind others available at the same time.
var releaseScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'receipt') ~= ARGV[1] then return 0 end
if ARGV[4] == '1' and tonumber(redis.call('HGET', KEYS[1], 'attempts') or '0') > 0 then
  redis.call('HINCRBY', KEYS[1], 'attempts', -1)
end
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

// buryScript: KEYS job, reserved, failed; ARGV receipt, id, failed-at ms,
// failure JSON. Returns 0 when the receipt is not the job's current lease.
var buryScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'receipt') ~= ARGV[1] then return 0 end
local id = ARGV[2]
` + releaseUniqueLua + `
redis.call('ZREM', KEYS[2], redis.call('HGET', KEYS[1], 'member'))
redis.call('HSET', KEYS[1], 'receipt', '', 'failure', ARGV[4])
redis.call('ZADD', KEYS[3], ARGV[3], ARGV[2])
return 1
`)

// retryFailedScript: KEYS job, failed, pending, seq; ARGV id, now ms.
// Returns {0, ”} when the job is not failed, {2, holder} when its
// uniqueness key is held by another job, {1, ”} when retried.
var retryFailedScript = redis.NewScript(`
if not redis.call('ZSCORE', KEYS[2], ARGV[1]) then return {0, ''} end
-- An ID whose job hash is gone (evicted, deleted by hand) has nothing to
-- retry: drop it rather than queue a job with no envelope.
if redis.call('HEXISTS', KEYS[1], 'env') == 0 then
  redis.call('ZREM', KEYS[2], ARGV[1])
  return {0, ''}
end
-- Reclaim the uniqueness key the job gave up when it failed, unless another
-- job holds it now: running both is what Unique prevents.
local unique = redis.call('HGET', KEYS[1], 'unique')
if unique and unique ~= '' then
  local holder = redis.call('GET', unique)
  if holder and holder ~= ARGV[1] then return {2, holder} end
  redis.call('SET', unique, ARGV[1], 'PX', redis.call('HGET', KEYS[1], 'unique_ttl'))
end
redis.call('ZREM', KEYS[2], ARGV[1])
local member = string.format('%016d', redis.call('INCR', KEYS[4])) .. ':' .. ARGV[1]
redis.call('HSET', KEYS[1], 'attempts', 0, 'receipt', '', 'member', member)
redis.call('HDEL', KEYS[1], 'failure')
redis.call('ZADD', KEYS[3], ARGV[2], member)
return {1, ''}
`)

// forgetFailedScript: KEYS job, failed; ARGV id. Returns 0 when the job is
// not failed.
var forgetFailedScript = redis.NewScript(`
if not redis.call('ZSCORE', KEYS[2], ARGV[1]) then return 0 end
redis.call('ZREM', KEYS[2], ARGV[1])
redis.call('DEL', KEYS[1])
return 1
`)

// purgeFailedScript: KEYS failed; ARGV before ms ('+inf' for all), batch,
// job key prefix. Deletes up to batch failed jobs older than before and
// returns how many.
var purgeFailedScript = redis.NewScript(`
local max = ARGV[1]
if max ~= '+inf' then max = '(' .. max end
local ids = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', max, 'LIMIT', 0, tonumber(ARGV[2]))
for _, id in ipairs(ids) do
  redis.call('ZREM', KEYS[1], id)
  redis.call('DEL', ARGV[3] .. id)
end
return #ids
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

// PushUnique implements Queue.
func (q *RedisQueue) PushUnique(ctx context.Context, queue string, env Envelope, at time.Time, unique UniqueKey) error {
	if q.closed.Load() {
		return ErrClosed
	}
	if !ValidName(queue) {
		return fmt.Errorf("%w: %q", ErrInvalidQueue, queue)
	}
	if env.ID == "" {
		return fmt.Errorf("jobs: push %q: envelope has no ID", env.Name)
	}
	if err := unique.validate(); err != nil {
		return err
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
	untilDone := "0"
	if unique.UntilDone {
		untilDone = "1"
	}
	k := q.keys(queue)
	res, err := pushUniqueScript.Run(ctx, q.client, []string{k.jobPrefix + env.ID, k.pending, k.seq, k.unique + unique.Key},
		data, at.UnixMilli(), env.ID, unique.TTL.Milliseconds(), untilDone).Slice()
	if err != nil {
		return fmt.Errorf("jobs: push %q to %s: %w", env.Name, queue, err)
	}
	if len(res) != 2 {
		return fmt.Errorf("jobs: push %q to %s: unexpected reply %v", env.Name, queue, res)
	}
	switch code, _ := toInt(res[0]); code {
	case 1:
		return nil
	case 0:
		return &DuplicateError{Queue: queue, Key: unique.Key, HolderID: fmt.Sprint(res[1])}
	default:
		return fmt.Errorf("%w: %s on %s", ErrDuplicateJob, env.ID, queue)
	}
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
		if len(res) != 4 {
			return Delivery{}, fmt.Errorf("jobs: reserve from %s: unexpected reply %v", queue, res)
		}
		var availableAt time.Time
		if ms, err := strconv.ParseFloat(fmt.Sprint(res[3]), 64); err == nil {
			availableAt = time.UnixMilli(int64(ms))
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
			// caller buries it (keeping the stored bytes) rather than meet
			// it on every lease expiry.
			return Delivery{Queue: queue, Envelope: Envelope{ID: id, Attempt: attempts}, Receipt: receipt, Err: err, AvailableAt: availableAt}, nil
		}
		env.Attempt = attempts
		return Delivery{Queue: queue, Envelope: env, Receipt: receipt, AvailableAt: availableAt}, nil
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
	return q.release(ctx, d, at, false)
}

// Postpone implements Queue.
func (q *RedisQueue) Postpone(ctx context.Context, d Delivery, at time.Time) error {
	return q.release(ctx, d, at, true)
}

func (q *RedisQueue) release(ctx context.Context, d Delivery, at time.Time, uncount bool) error {
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
		d.Receipt, at.UnixMilli(), d.Envelope.ID, uncount).Int()
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

// Bury implements Queue.
func (q *RedisQueue) Bury(ctx context.Context, d Delivery, f Failure) error {
	if q.closed.Load() {
		return ErrClosed
	}
	if d.Receipt == "" || !ValidName(d.Queue) {
		return fmt.Errorf("%w: %s", ErrLeaseLost, d.Envelope.ID)
	}
	if f.At.IsZero() {
		f.At = q.now()
	}
	data, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("jobs: bury %s: %w", d.Envelope.ID, err)
	}
	k := q.keys(d.Queue)
	ok, err := buryScript.Run(ctx, q.client, []string{k.jobPrefix + d.Envelope.ID, k.reserved, k.failed},
		d.Receipt, d.Envelope.ID, f.At.UnixMilli(), data).Int()
	if err != nil {
		return fmt.Errorf("jobs: bury %s: %w", d.Envelope.ID, err)
	}
	if ok == 0 {
		return fmt.Errorf("%w: %s", ErrLeaseLost, d.Envelope.ID)
	}
	return nil
}

// Failed implements Queue. It reads in pages of 500, each one script that
// resumes after the last job the previous page saw (not at a rank, which
// shifts as jobs fail, retry, and are forgotten), so a page of newer failures
// arriving mid-read is not read twice and a deleted head does not skip the
// jobs behind it. An ID whose job record is gone (evicted, deleted by hand)
// is dropped from the failed set as the read passes it. It is not a
// snapshot: a job that fails while a read in pages is under way sorts ahead
// of where the read resumes and is not in the result, and a job returned
// may have been retried or forgotten since.
func (q *RedisQueue) Failed(ctx context.Context, queue string, limit int) ([]FailedJob, error) {
	if q.closed.Load() {
		return nil, ErrClosed
	}
	if !ValidName(queue) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidQueue, queue)
	}
	k := q.keys(queue)
	var out []FailedJob
	// A cursor whose job left the set resumes at the start of its failure
	// instant, which can meet jobs already read.
	seen := map[string]bool{}
	var cursor failedCursor
	for {
		want := failedPageSize
		if limit > 0 && limit-len(out) < want {
			want = limit - len(out)
		}
		page, next, done, err := q.failedPage(ctx, queue, k, cursor, want)
		if err != nil {
			return out, err
		}
		for _, job := range page {
			if !seen[job.Envelope.ID] {
				seen[job.Envelope.ID] = true
				out = append(out, job)
			}
		}
		if done || (limit > 0 && len(out) >= limit) {
			return out, nil
		}
		cursor = next
		if failedPageHook != nil {
			failedPageHook()
		}
	}
}

// failedPageSize bounds one read of the failed set: the jobs it returns and
// the set members it looks at.
var failedPageSize = 500

// failedPageHook runs between the pages of a Failed read (tests only).
var failedPageHook func()

// failedCursor is the last failed-set member a page looked at: its score
// (failure time in ms) and ID. Zero starts at the most recent.
type failedCursor struct{ score, id string }

// failedScanScript: KEYS failed; ARGV job key prefix, want, most members to
// look at, cursor score (empty for the top), cursor ID. Returns done (1 when
// the set ran out), the new cursor's score and ID, then id, env, attempts,
// failure for each job. It resumes just after the cursor when the cursor's
// job is still there at that score, else at the start of the cursor's
// failure instant. A member without an envelope or failure record is not a
// failed job any more (its hash was evicted or deleted by hand): it is
// dropped from the set.
var failedScanScript = redis.NewScript(`
local start = 0
if ARGV[4] ~= '' then
  local score = redis.call('ZSCORE', KEYS[1], ARGV[5])
  if score and tonumber(score) == tonumber(ARGV[4]) then
    start = redis.call('ZREVRANK', KEYS[1], ARGV[5]) + 1
  else
    start = redis.call('ZCOUNT', KEYS[1], '(' .. ARGV[4], '+inf')
  end
end
local want, look = tonumber(ARGV[2]), tonumber(ARGV[3])
local rows = redis.call('ZREVRANGE', KEYS[1], start, start + look - 1, 'WITHSCORES')
local out = {1, ARGV[4], ARGV[5]}
if #rows / 2 == look then out[1] = 0 end
local found = 0
for i = 1, #rows, 2 do
  local id = rows[i]
  out[2], out[3] = rows[i + 1], id
  local f = redis.call('HMGET', ARGV[1] .. id, 'env', 'attempts', 'failure')
  if not f[1] or not f[3] then
    redis.call('ZREM', KEYS[1], id)
  else
    out[#out + 1] = id
    out[#out + 1] = f[1]
    out[#out + 1] = f[2] or '0'
    out[#out + 1] = f[3]
    found = found + 1
    if found == want then
      if i + 1 < #rows then out[1] = 0 end
      break
    end
  end
end
return out
`)

// failedPage reads up to want failed jobs after cursor, most recent first,
// in one script.
func (q *RedisQueue) failedPage(ctx context.Context, queue string, k redisKeys, cursor failedCursor, want int) (jobs []FailedJob, next failedCursor, done bool, err error) {
	res, err := failedScanScript.Run(ctx, q.client, []string{k.failed},
		k.jobPrefix, want, failedPageSize, cursor.score, cursor.id).Slice()
	if err != nil {
		return nil, cursor, false, fmt.Errorf("jobs: list failed jobs on %s: %w", queue, err)
	}
	if len(res) < 3 || (len(res)-3)%4 != 0 {
		return nil, cursor, false, fmt.Errorf("jobs: list failed jobs on %s: malformed reply", queue)
	}
	flag, _ := toInt(res[0])
	next = failedCursor{score: fmt.Sprint(res[1]), id: fmt.Sprint(res[2])}
	for i := 3; i < len(res); i += 4 {
		id := fmt.Sprint(res[i])
		job, err := decodeFailedJob(queue, id, res[i+1:i+4])
		if err != nil {
			return nil, cursor, false, err
		}
		jobs = append(jobs, job)
	}
	return jobs, next, flag == 1, nil
}

// FailedJob implements Queue.
func (q *RedisQueue) FailedJob(ctx context.Context, queue, id string) (FailedJob, error) {
	if q.closed.Load() {
		return FailedJob{}, ErrClosed
	}
	if !ValidName(queue) {
		return FailedJob{}, fmt.Errorf("%w: %q", ErrInvalidQueue, queue)
	}
	k := q.keys(queue)
	if err := q.client.ZScore(ctx, k.failed, id).Err(); errors.Is(err, redis.Nil) {
		return FailedJob{}, fmt.Errorf("%w: %s on %s", ErrNotFailed, id, queue)
	} else if err != nil {
		return FailedJob{}, fmt.Errorf("jobs: failed job %s: %w", id, err)
	}
	return q.failedJob(ctx, queue, k, id)
}

func (q *RedisQueue) failedJob(ctx context.Context, queue string, k redisKeys, id string) (FailedJob, error) {
	fields, err := q.client.HMGet(ctx, k.jobPrefix+id, "env", "attempts", "failure").Result()
	if err != nil {
		return FailedJob{}, fmt.Errorf("jobs: failed job %s: %w", id, err)
	}
	return decodeFailedJob(queue, id, fields)
}

// decodeFailedJob builds a FailedJob from a job hash's env, attempts, and
// failure fields.
func decodeFailedJob(queue, id string, fields []any) (FailedJob, error) {
	if len(fields) != 3 {
		return FailedJob{}, fmt.Errorf("%w: %s on %s", ErrNotFailed, id, queue)
	}
	raw, _ := fields[0].(string)
	failure, _ := fields[2].(string)
	if raw == "" || failure == "" {
		return FailedJob{}, fmt.Errorf("%w: %s on %s", ErrNotFailed, id, queue)
	}
	job := FailedJob{Queue: queue}
	if n, err := toInt(fields[1]); err == nil {
		job.Attempts = n
	}
	if err := json.Unmarshal([]byte(failure), &job.Failure); err != nil {
		return FailedJob{}, fmt.Errorf("jobs: failed job %s: failure record: %w", id, err)
	}
	env, err := UnmarshalEnvelope([]byte(raw))
	if err != nil {
		job.Envelope = Envelope{ID: id}
		job.RawEnvelope = []byte(raw)
	} else {
		job.Envelope = env
	}
	job.Envelope.Attempt = job.Attempts
	return job, nil
}

// RetryFailed implements Queue.
func (q *RedisQueue) RetryFailed(ctx context.Context, queue, id string) error {
	if q.closed.Load() {
		return ErrClosed
	}
	if !ValidName(queue) {
		return fmt.Errorf("%w: %q", ErrInvalidQueue, queue)
	}
	k := q.keys(queue)
	res, err := retryFailedScript.Run(ctx, q.client, []string{k.jobPrefix + id, k.failed, k.pending, k.seq},
		id, q.now().UnixMilli()).Slice()
	if err != nil {
		return fmt.Errorf("jobs: retry failed job %s: %w", id, err)
	}
	if len(res) != 2 {
		return fmt.Errorf("jobs: retry failed job %s: unexpected reply %v", id, res)
	}
	switch code, _ := toInt(res[0]); code {
	case 1:
		return nil
	case 2:
		unique, _ := q.client.HGet(ctx, k.jobPrefix+id, "unique").Result()
		return &DuplicateError{Queue: queue, Key: strings.TrimPrefix(unique, k.unique), HolderID: fmt.Sprint(res[1])}
	default:
		return fmt.Errorf("%w: %s on %s", ErrNotFailed, id, queue)
	}
}

// ForgetFailed implements Queue.
func (q *RedisQueue) ForgetFailed(ctx context.Context, queue, id string) error {
	if q.closed.Load() {
		return ErrClosed
	}
	if !ValidName(queue) {
		return fmt.Errorf("%w: %q", ErrInvalidQueue, queue)
	}
	k := q.keys(queue)
	ok, err := forgetFailedScript.Run(ctx, q.client, []string{k.jobPrefix + id, k.failed}, id).Int()
	if err != nil {
		return fmt.Errorf("jobs: forget failed job %s: %w", id, err)
	}
	if ok == 0 {
		return fmt.Errorf("%w: %s on %s", ErrNotFailed, id, queue)
	}
	return nil
}

// purgeBatch bounds one purge script, so a large purge does not block Redis.
var purgeBatch = 500

// PurgeFailed implements Queue. It deletes in batches of 500, each one
// script.
func (q *RedisQueue) PurgeFailed(ctx context.Context, queue string, before time.Time) (int, error) {
	if q.closed.Load() {
		return 0, ErrClosed
	}
	if !ValidName(queue) {
		return 0, fmt.Errorf("%w: %q", ErrInvalidQueue, queue)
	}
	bound := "+inf"
	if !before.IsZero() {
		bound = strconv.FormatInt(before.UnixMilli(), 10)
	}
	k := q.keys(queue)
	total := 0
	for {
		n, err := purgeFailedScript.Run(ctx, q.client, []string{k.failed}, bound, purgeBatch, k.jobPrefix).Int()
		if err != nil {
			return total, fmt.Errorf("jobs: purge failed jobs on %s: %w", queue, err)
		}
		total += n
		if n < purgeBatch {
			return total, nil
		}
	}
}

// onceBeginScript: KEYS once; ARGV token, lock ms. Returns 1 when done, 2
// when another run holds the lock, 0 when the caller took it.
var onceBeginScript = redis.NewScript(`
local v = redis.call('GET', KEYS[1])
if v == 'done' then return 1 end
if v then return 2 end
redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
return 0
`)

// onceFinishScript: KEYS once; ARGV token, keep ms. Returns 0 when the
// token no longer holds the lock.
var onceFinishScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) ~= ARGV[1] then return 0 end
redis.call('SET', KEYS[1], 'done', 'PX', ARGV[2])
return 1
`)

// onceAbandonScript: KEYS once; ARGV token.
var onceAbandonScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then redis.call('DEL', KEYS[1]) end
return 1
`)

func (q *RedisQueue) onceKey(key string) string {
	return fmt.Sprintf("%s:jobs:once:%s", q.namespace, key)
}

// BeginOnce implements OnceStore.
func (q *RedisQueue) BeginOnce(ctx context.Context, key, token string, lock time.Duration) (OnceState, error) {
	if q.closed.Load() {
		return 0, ErrClosed
	}
	n, err := onceBeginScript.Run(ctx, q.client, []string{q.onceKey(key)}, token, lock.Milliseconds()).Int()
	if err != nil {
		return 0, err
	}
	switch n {
	case 1:
		return OnceDone, nil
	case 2:
		return OnceBusy, nil
	default:
		return OnceAcquired, nil
	}
}

// FinishOnce implements OnceStore.
func (q *RedisQueue) FinishOnce(ctx context.Context, key, token string, keep time.Duration) error {
	if q.closed.Load() {
		return ErrClosed
	}
	n, err := onceFinishScript.Run(ctx, q.client, []string{q.onceKey(key)}, token, keep.Milliseconds()).Int()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: the lock on %q expired before the effect was recorded", ErrLeaseLost, key)
	}
	return nil
}

// AbandonOnce implements OnceStore.
func (q *RedisQueue) AbandonOnce(ctx context.Context, key, token string) error {
	if q.closed.Load() {
		return ErrClosed
	}
	return onceAbandonScript.Run(ctx, q.client, []string{q.onceKey(key)}, token).Err()
}

// Stats implements Queue.
func (q *RedisQueue) Stats(ctx context.Context, queue string) (QueueStats, error) {
	if q.closed.Load() {
		return QueueStats{}, ErrClosed
	}
	if !ValidName(queue) {
		return QueueStats{}, fmt.Errorf("%w: %q", ErrInvalidQueue, queue)
	}
	k := q.keys(queue)
	now := strconv.FormatInt(q.now().UnixMilli(), 10)
	pipe := q.client.Pipeline()
	ready := pipe.ZCount(ctx, k.pending, "-inf", now)
	scheduled := pipe.ZCount(ctx, k.pending, "("+now, "+inf")
	reserved := pipe.ZCard(ctx, k.reserved)
	failed := pipe.ZCard(ctx, k.failed)
	oldest := pipe.ZRangeWithScores(ctx, k.pending, 0, 0)
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return QueueStats{}, fmt.Errorf("jobs: stats of %s: %w", queue, err)
	}
	st := QueueStats{Ready: int(ready.Val()), Scheduled: int(scheduled.Val()), Reserved: int(reserved.Val()), Failed: int(failed.Val())}
	if z := oldest.Val(); len(z) == 1 && st.Ready > 0 {
		st.OldestReady = time.UnixMilli(int64(z[0].Score))
	}
	return st, nil
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

var (
	_ Queue     = (*RedisQueue)(nil)
	_ Queue     = (*MemoryQueue)(nil)
	_ OnceStore = (*RedisQueue)(nil)
	_ OnceStore = (*MemoryQueue)(nil)
)
