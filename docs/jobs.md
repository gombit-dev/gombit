# Background jobs

> **Status:** the [JOBS-0 epic](https://github.com/gombit-dev/gombit/issues/278):
> the job contract (JOBS-1), queue drivers (JOBS-2), the worker (JOBS-3),
> retries, backoff, and timeouts (JOBS-4), delayed jobs (JOBS-5), failed jobs
> (JOBS-6), duplicate handling (JOBS-7), observability (JOBS-8), and the test
> queue (JOBS-9).

A job is work that should not run inside an HTTP request: sending an email,
resizing an image, delivering a webhook. Application code defines a job as a
struct, registers a handler for it, and dispatches it. It never imports a
queue driver.

## Defining a job

```go
type SendWelcomeEmail struct {
	UserID uint `json:"user_id"`
}

func (SendWelcomeEmail) JobName() string { return "send_welcome_email" }
```

- **The name is the identity.** A queued job is looked up by `JobName()`,
  not by its Go type, so renaming the type or moving its package does not
  strand jobs already in a queue. Names use `a-z`, `0-9`, and `_ . : -`
  (1–128 characters, starting with a letter or digit), so they are safe as
  queue keys and metric labels.
- **The name is a constant.** It must not depend on field values, and
  `JobName` uses a value receiver. `Encode` refuses a value whose name differs
  from the one it was registered under.
- **The payload is the struct's JSON fields.** The job type may not have its
  own `MarshalJSON`/`UnmarshalJSON` (or `MarshalText`/`UnmarshalText`), so
  both directions use the same fields; field types such as `time.Time` keep
  theirs. Carry IDs, not secrets or large bodies: a queue is stored,
  replicated, and inspected by operators. The handler loads what it needs by
  ID.

## Registering and dispatching

`framework.App` opens a dispatcher for the configured driver. Register every
job on its registry at startup, and dispatch through it:

```go
jobs.MustRegister(app.Jobs().Registry(), func(ctx context.Context, job SendWelcomeEmail) error {
	return mailer.Welcome(ctx, job.UserID)
})

// in a handler or service:
env, err := app.Jobs().Dispatch(ctx, SendWelcomeEmail{UserID: user.ID})
env, err = app.Jobs().Dispatch(ctx, job, jobs.OnQueue("mail")) // another queue
```

### Later

```go
app.Jobs().Dispatch(ctx, SendReminder{UserID: id}, jobs.Delay(24*time.Hour))
app.Jobs().DispatchAt(ctx, PublishPost{PostID: id}, post.PublishAt) // or jobs.At(t)
```

A delayed job waits in the queue, scored by when it becomes available; the
worker picks it up at its time (within its poll interval), and nothing in your
application polls. With the `redis` driver the schedule survives restarts of
the app and the workers. A time in the past means now. A delay is measured on
the dispatching host's clock and compared with the worker's, so keep hosts'
clocks synchronized (NTP), as leases already require. The `sync` driver has
nowhere to hold a job, so it runs a delayed one at once, as it does every job.
In tests, give the dispatcher and the queue the same clock
(`jobs.WithDispatcherClock`, `jobs.WithMemoryClock`/`jobs.WithRedisClock`) and
advance it instead of sleeping.

Application code depends on `*jobs.Dispatcher`, never on a driver. The app's
registry already carries request and trace IDs into handlers
(`framework.JobPropagator`). Outside an App, build one yourself:
`jobs.NewRegistry(...)` plus `jobs.Open(cfg.Jobs, cfg.Cache.Redis, registry)`
or `jobs.NewDispatcher(registry, queue)`.

Register every job at startup. `Register` returns an error (and
`MustRegister` panics) for a programming mistake: an invalid name, a name
another type already uses, a pointer type, a payload that cannot be encoded as
JSON, or a bad version. The handler receives the decoded job, typed.

## Drivers

`GOMBIT_JOBS_DRIVER` selects the backend; application code does not change.

| Driver | Where jobs live | Use |
|--------|-----------------|-----|
| `sync` (default) | nowhere: `Dispatch` runs the job before it returns | development and tests; no infrastructure, no worker. `Dispatch` returns the job's own failure. |
| `memory` | process memory | tests and single-process apps that accept losing queued jobs on exit |
| `redis` | Redis (the shared `GOMBIT_REDIS_*` connection) | production: jobs survive app and worker restarts |

Jobs go to `GOMBIT_JOBS_QUEUE` (`default`) unless dispatched `OnQueue`.
Queue names use the job-name alphabet. `GOMBIT_JOBS_NAMESPACE` prefixes the
Redis keys (it defaults like the cache namespace, from app name and
environment), so apps and environments sharing a server do not share queues.

The `redis` driver keeps each queue under one hash tag,
`{<namespace>:jobs:<queue>}` (a pending sorted set scored by when each job
becomes available, a reserved sorted set scored by lease deadline, and a hash
per job), so it works on Redis Cluster, and each operation is one Lua script:
a crash between steps cannot lose or duplicate a job. Both drivers deliver in
availability order: a waiting job since its available-at time, a job whose
lease expired since that deadline, push order breaking ties. A job ID is
unique per queue.

## Running the worker

The worker runs your app's job handlers, so it is part of your app binary:
`framework.Run` starts it instead of the HTTP server when the first argument
is `worker`. One build deploys as both processes:

```sh
./server                                   # the web process
./server worker --queue critical,default --concurrency 8
```

In development, `gombit worker` builds `./cmd/server` and runs it as
`server worker` with the same flags (it supervises the server itself, so
Ctrl+C gets a graceful shutdown and the worker's exit status):

```sh
GOMBIT_JOBS_DRIVER=redis gombit worker --concurrency 4
```

| Flag | Default | |
|------|---------|--|
| `--queue` | `GOMBIT_JOBS_QUEUE` | queues to consume, highest priority first; repeat or comma-separate |
| `--concurrency` | `1` | jobs running at once |
| `--lease` | `5m` | how long a reserved job is held; renewed every third of it while the handler runs |
| `--shutdown-timeout` | `30s` | how long in-flight jobs get to finish on SIGINT/SIGTERM |
| `--metrics-addr` | off | serve `/metrics` and `/livez` on this address, e.g. `:9091` (see [Observability](#observability)) |

A worker needs a queue another process can reach, so it refuses to start with
the `sync` driver (jobs already ran at dispatch) and the `memory` driver (its
queue lives in the dispatching process). Use `redis`.

The worker runs the app's `OnStart` hooks, then:

- reserves jobs from its queues in priority order, up to `--concurrency` at a
  time, and waits a second when they are empty;
- runs each through the registry, renewing its lease while the handler runs
  (a job that runs past one lease is not handed to a second worker; if the
  lease is lost anyway, the handler's context is canceled);
- acknowledges a job that succeeded, and releases one that failed for a retry
  after its backoff, until it succeeds, fails permanently, or uses its last
  attempt (see [Retries and timeouts](#retries-and-timeouts));
- logs one structured entry per outcome (`job_id`, `job`, `queue`, `attempt`,
  `duration`, the failure `kind`, and the propagated request and trace IDs),
  through the app's logger.

On SIGINT/SIGTERM it stops reserving at once and gives in-flight jobs
`--shutdown-timeout` to finish. A job still running then sees its context
canceled and gets up to 10 more seconds (`jobs.ShutdownGrace`) to return and
be released, available immediately. Then the app's `OnStop` hooks run.

**Give your process manager a stop timeout of at least `--shutdown-timeout`
plus 28 seconds** (58s at the defaults; `terminationGracePeriodSeconds` on
Kubernetes, `TimeoutStopSec` on systemd, `stop_grace_period` in Compose). That
is everything that follows SIGTERM: a reserve already on the wire and the
release of what it returns (4s each), the shutdown timeout, the 10s grace, and
the app's stop hooks (10s). A manager that kills the worker sooner can cut off
a release or an ack, and that job waits out its lease instead.
`framework.WorkerKillAfter` computes it, and `gombit worker` waits that long
itself.

Every queue call the worker makes has a 4s deadline. The Redis driver's client
(built by `jobs.Open`, or `jobs.OpenWithRedis` from the app's own client)
enforces it: it is configured with go-redis's `ContextTimeoutEnabled`, and
without retries, since retrying a Lua script whose reply was lost could lease
a second job. If you build a `RedisQueue` yourself, use
`jobs.RedisClientOptions`.

**Nothing is lost when a worker crashes.** A job a dead worker was running
keeps its lease in Redis; when the lease expires it is delivered to another
worker, as its next attempt. That is why delivery is at least once and
handlers must be idempotent.

To run a worker inside another process (tests, a single-process app with the
memory driver), call `framework.RunWorker(ctx, app, jobs.WorkerOptions{...})`,
or build one with `jobs.NewWorker(registry, queue, opts)`.

A stored envelope that no longer decodes cannot run anywhere. `Reserve` still
hands it out, leased, with the failure on `Delivery.Err` (and a nil error, so
an `if err != nil` caller cannot drop the lease); the worker sets it aside
with the failed jobs (`Queue.Bury`), stored bytes kept. A consumer of your own
must do the same (or ack it), or the job returns on every lease expiry.

## The envelope

A queue driver sits between two registry calls:

```go
env, err := registry.Encode(ctx, SendWelcomeEmail{UserID: 42}) // dispatch
data, err := env.Marshal()                                       // the driver stores data

env, err = jobs.UnmarshalEnvelope(data)                          // a worker loads it
err = registry.Run(ctx, env)                                     // decode + handler
```

`Encode` gives the job a fresh ID and records its name, payload version,
enqueue time, and propagated metadata. Dispatching a job that is not
registered fails right there, not later in a worker.

Inside the handler, `jobs.InfoFromContext(ctx)` returns the job's ID, name,
version (and `QueuedVersion`, the one it was queued at before any upgrade),
attempt, and enqueue time. Delivery is **at least once**: a handler
can run more than once for the same job, so it should use the job ID as the
idempotency key for its side effects.

## Failures

Every failure `Run` returns is a `*jobs.Error` with a `Kind`, a small fixed set
that is safe as a log field or metric label. `jobs.Classify(err)` returns it,
and each kind has a sentinel for `errors.Is`:

| Kind | Sentinel | Meaning |
|------|----------|---------|
| `unknown_job` | `ErrUnknownJob` | no handler is registered for the name |
| `decode` | `ErrDecode` | the envelope (no name or ID, a version below 1, checked by `Run` itself) or payload (empty, `null`, wrong shape) does not decode into the job type |
| `unsupported_version` | `ErrUnsupportedVersion` | the payload version is newer than this binary, or older with no upgrade step |
| `panic` | `ErrPanic` | the handler, an upgrade step, a payload's `UnmarshalJSON`, or a propagator panicked (recovered, so one job cannot take a worker down) |
| `timeout` | `ErrTimeout` | the attempt ran past the job's `Timeout` |
| `upgrade` | `ErrUpgrade` | an `UpgradeFrom` step returned an error (the payload is intact; a deploy can fix the step) |
| `handler` | `ErrHandler` | the handler returned an error, which `errors.Is/As` still reach |

Use `Classify`, not `errors.Is`, to decide a failure's kind: a handler that
returns another job's failure is `handler`, though `errors.Is` still reaches the
inner sentinel through the wrap chain.

## Retries and timeouts

Each job has an execution policy, set when it is registered:

```go
jobs.MustRegister(registry, sendWelcome, jobs.WithOptions(jobs.Options{
	MaxAttempts: 8,                                                    // default 5
	Timeout:     30 * time.Second,                                     // per attempt; default none
	Backoff:     jobs.Jittered(jobs.Exponential(5*time.Second, time.Hour)), // default Exponential(10s, 10m)
}))
```

`jobs.NewRegistry(jobs.WithDefaultOptions(...))` sets the policy of every job
that does not set its own, and fills the fields a job leaves zero.

- **Timeout** is the deadline of the handler's context for one attempt; when it
  runs out, the attempt fails as `timeout` (and is retried). Zero takes the
  registry default; `jobs.NoTimeout` opts a job out of a default timeout. It applies on every
  driver, `sync` included, because `Run` applies it. Go cannot stop a goroutine,
  so a handler must return when its context is done.
- **Backoff** is the wait before the next attempt, given the attempt that just
  failed: `jobs.Exponential(base, max)` doubles from `base` up to `max`,
  `jobs.Constant(d)` waits `d`, and `jobs.Jittered(b)` spreads `b`'s waits over
  50–100% so jobs that failed together (an outage) do not all retry at once.
- **Permanent failures** are not retried: return `jobs.Permanent(err)` when no
  retry can help (the record is gone, the input is invalid). A `decode` failure
  (bytes that cannot be the job: empty, `null`, the wrong shape) is permanent
  too. Every other kind (`handler`, `timeout`, `panic`, and `unknown_job`,
  `unsupported_version`, or `upgrade`, which a deploy can fix) is retried.
- **Giving up.** After a permanent failure, or when the attempt that failed was
  the last (`Info.MaxAttempts`), the worker moves the job to its queue's
  [failed jobs](#failed-jobs) and logs `job failed for good` at error level,
  with its ID, name, attempt, the reason, and the `gombit jobs inspect` command
  for it, but not its payload.

The attempt count and the retry time live in the queue. With Redis both
survive worker restarts: a job that failed under one worker runs its next
attempt under another, not before its backoff, and `MaxAttempts` counts
attempts across both. A job interrupted by a worker's shutdown goes back at
once rather than waiting out its backoff, but its attempt was already
counted: every deploy that interrupts a job uses one of its attempts, so leave
headroom in `MaxAttempts` for long jobs. An interrupted attempt is never given
up on, even the last one, so a job can run again with `Attempt` past
`MaxAttempts`; a handler should not assume `Attempt == MaxAttempts` is
certainly its last run.

## Observability

Every job is observable without instrumenting it.

**Logs.** The worker logs one structured entry per outcome (`job succeeded`,
`job failed`, `job failed for good`, …) with `job_id`, `job`, `queue`,
`attempt`, `duration`, `waited` (time in the queue since it became available),
the failure `kind`, and the propagated request and trace IDs under `metadata`.
`job_id` is the same on every attempt of a job, so filtering on it shows its
whole history, retries included; `attempt` orders them.

**Traces.** The app's registry carries OpenTelemetry context through the
envelope (`jobs.OTelPropagator`: W3C `traceparent`/`tracestate` and baggage,
or your global propagator), and every run is a span, `job <name>`, a child of
the span that dispatched it, with the job's name, ID, version, and attempt,
and an error status when it fails. That needs an OpenTelemetry SDK in your app
(`otel.SetTracerProvider`); without one both are no-ops.

**Metrics** (Prometheus text format):

| Metric | Labels | |
|--------|--------|--|
| `gombit_jobs_processed_total` | `job_name`, `queue`, `result` | deliveries by outcome: `succeeded`, `retried`, `failed` (given up), `interrupted`, `postponed` (waited on another run's `Once` lock), `abandoned` (lease lost), `undecodable`, `unsettled` (the ack, release, or bury failed; the job returns when its lease expires) |
| `gombit_jobs_run_seconds` (histogram) | `job_name`, `queue` | handler time |
| `gombit_jobs_wait_seconds` (histogram) | `job_name`, `queue` | queue latency: from available (dispatch time, delay, retry time, or an expired lease) to started; buckets 10ms to 30m |
| `gombit_jobs_in_flight` | `queue` | running now |
| `gombit_jobs_queued` | `queue`, `state` | `ready` (a lapsed lease counts: the job is due again), `scheduled`, `reserved`, `failed` |
| `gombit_jobs_oldest_ready_seconds` | `queue` | age of the longest-waiting ready job |

A worker process serves no HTTP, so pass `--metrics-addr :9091` and scrape
that; it also answers `/livez`. The queue gauges are read from the queue at
scrape time, for the queues the worker consumes; when a queue does not answer,
the scrape fails (503), so Prometheus keeps the last good sample rather than
record an empty queue. A worker running inside the web process
(`framework.RunWorker`) records into the app's own `/metrics`, queue gauges
included; there a queue that does not answer is left out and reported as
`gombit_jobs_queue_stats_up{queue} 0`, so an outage does not also take the
HTTP series down.

Outcomes count once the queue committed them: `succeeded` after the ack,
`retried`, `interrupted`, and `postponed` after the release, `failed` and `undecodable`
after the bury. When that call fails the job stays leased and returns after
the lease; it counts as `unsettled` (or `abandoned`, when the lease was lost).

The job's name is the `job_name` label (Prometheus reserves `job` for the
scrape job). Only registered job names appear; any other name counts as
`unknown`, so an envelope cannot create series.

## Duplicates

Delivery is **at least once**. A job can run more than once:

- its worker crashed, or its lease ran out mid-run, after the side effect but
  before the acknowledgement: another worker runs it again;
- it failed after the side effect (a later step errored): its retry runs the
  whole handler again;
- a deploy interrupted it, and it goes back to the queue.

A job is never lost to these; it is repeated. Two tools keep the repeats
harmless.

### Unique dispatch

```go
app.Jobs().Dispatch(ctx, RebuildReport{AccountID: id},
	jobs.Unique("rebuild-report:"+id, time.Hour))       // one queued or running at a time
app.Jobs().Dispatch(ctx, SendDigest{UserID: id},
	jobs.UniqueFor("digest:"+id, 24*time.Hour))         // at most one per day
```

`Unique(key, ttl)` refuses a second job with the same key on the same queue
while one is queued, delayed, or running; the key frees when that job succeeds
or is given up on, and after `ttl` in any case, so a job that vanishes cannot
hold it forever. `UniqueFor(key, window)` refuses duplicates for `window` after
the first dispatch, whether or not it has run. A refused dispatch returns a
`*jobs.DuplicateError` (`errors.Is(err, jobs.ErrDuplicateDispatch)`) naming the
job that holds the key; nothing is queued. The `sync` driver has no queue to
hold a key and runs every dispatch.

### Once per side effect

```go
func sendWelcome(ctx context.Context, job SendWelcomeEmail) error {
	info, _ := jobs.InfoFromContext(ctx)
	return jobs.Once(ctx, "welcome-email:"+info.ID, func(ctx context.Context) error {
		return mailer.Welcome(ctx, job.UserID)
	})
}
```

`jobs.Once(ctx, key, fn)` runs `fn` unless an effect with that key already
completed, and records it when `fn` succeeds, so a redelivered job skips it. Key
it on the job ID and the effect. While one run holds a key, another gets
`jobs.ErrInProgress`: the worker postpones the job for the backoff and takes
back the attempt (`Queue.Postpone`), so it waits out the other run without
spending any of `MaxAttempts`. A failing `fn` releases the key for the retry. The lock expires
after `jobs.LockFor` (default 15m), so a crashed run cannot block the key
forever. Completions are remembered for `jobs.KeepFor` (default 7 days), and
then the record expires: failed jobs are kept until forgotten or purged, so a
redelivery after `KeepFor` (`gombit jobs retry` a week later) runs `fn` again.
Set `KeepFor` past the longest a job carrying the key can come back.

`Once` narrows duplicates, it does not make them impossible: a run that dies
between `fn` succeeding and the record keeps the lock until it expires, and the
next delivery then runs `fn` again. When an effect must happen exactly once,
make it idempotent at its destination too: a unique constraint, an idempotency
key the other system honors (payment APIs take one; use the job ID). Outside a
worker (the `sync` driver, a unit test) `Once` just runs `fn`; `jobstest`'s
`RunAll` remembers effects like a worker does. A consumer of your own gives
its handlers a store with `jobs.ContextWithOnceStore(ctx, store)` (the memory
and Redis queues are `OnceStore`s).

## Failed jobs

A job the worker gave up on is kept, not deleted: its queue's failed jobs hold
the original envelope (payload, metadata, version), the number of attempts,
and why: the reason (`attempts exhausted`, `permanent failure`, or
`undecodable envelope`), the failure kind, the last error, and when. Manage
them with `gombit jobs`, which reads the queue directly with the app's
configuration (`GOMBIT_JOBS_DRIVER=redis`, `GOMBIT_REDIS_*`) and does not need
the app's code:

```sh
gombit jobs failed [--queue mail] [--limit 50] [--json]
gombit jobs inspect <id>            # payload included
gombit jobs retry <id>... | --all   # back on the queue, available now, fresh attempts
gombit jobs forget <id>...
gombit jobs purge --force [--older-than 720h]
```

In code the same operations are `Queue.Failed`, `FailedJob`, `RetryFailed`,
`ForgetFailed`, and `PurgeFailed`. Forget and purge delete the job; retry puts
the same job (ID, payload) back on its queue. So the ID stays taken, and
dispatching a new job with it onto the same queue fails, until the job is
forgotten or purged while failed, or its retried run is acknowledged.

**Payloads and personal data.** A failed job's payload stays in Redis until
someone forgets or purges it. Retrying does not remove it: the payload goes
back on the queue and leaves when that run is acknowledged (or, if it fails
again, stays with the failed jobs). `inspect` (and `failed --json`) prints it;
logs never include it. Keep payloads to IDs and versions, never
secrets, tokens, or document bodies, and let the handler load the rest. Purge
old failures on a schedule (`gombit jobs purge --force --older-than 720h`);
nothing expires them on its own. That is also a capacity matter: a bug that
fails every job keeps every one of them in Redis until it is purged, so alert
on the `job failed for good` log and give Redis headroom.

## Changing a payload

Additive changes need no new version: a worker ignores fields it does not
know, and a field an older producer did not send decodes as its zero value.
That keeps a rolling deploy safe in both directions.

For a breaking change (a renamed, removed, or retyped field), implement
`JobVersion` and register a step for each older version that may still be
queued:

```go
func (SendWelcomeEmail) JobVersion() int { return 2 }

jobs.MustRegister(registry, handler, jobs.UpgradeFrom(1, func(p json.RawMessage) (json.RawMessage, error) {
	var v1 struct{ UserID uint `json:"user_id"`; Email string `json:"email"` }
	if err := json.Unmarshal(p, &v1); err != nil {
		return nil, err
	}
	return json.Marshal(SendWelcomeEmail{UserID: v1.UserID})
}))
```

Like `JobName`, `JobVersion` is a constant with a value receiver: `Register`
rejects a pointer receiver and a step given twice, and `Encode` rejects a value
that reports another version. `Run` chains the steps (1→2→3…) before decoding,
and an empty or `null` result from any step is a `decode` failure, never a
zero-value job. A version newer than the
binary, or an older one with a missing step, is `unsupported_version`. Keep an
upgrade step until no queue can still hold jobs of that version.

## Testing

Package `jobs/jobstest` tests code that dispatches jobs without Redis or a
worker. Its `Queue` is an in-memory queue that records every job pushed to
it; attach its dispatcher to the app and assert what was queued:

```go
func TestSignUpQueuesTheWelcomeEmail(t *testing.T) {
	q := jobstest.New()
	app, err := framework.New(framework.WithConfig(config.Default()), framework.WithJobs(q.Dispatcher()))
	if err != nil {
		t.Fatal(err)
	}
	registerJobs(app.Jobs().Registry()) // the app's own registration

	// ... call the code under test, which dispatches ...

	d := q.AssertDispatched(t, "send_welcome_email") // the last one
	if job := jobstest.Payload[SendWelcomeEmail](t, d); job.UserID != 42 {
		t.Fatalf("queued %+v", job)
	}
	q.AssertNotDispatched(t, "send_receipt")
}
```

Nothing runs until the test says so. The assertions are `AssertDispatched`,
`AssertDispatchedTimes(t, name, n)`, `AssertNotDispatched`, and
`AssertNothingDispatched`; `q.Dispatched(name)` returns the records (queue,
envelope, `AvailableAt`, and the `Unique` key, if any) and
`jobstest.Payloads[T](t, q)` decodes every `T` dispatched. A `Unique` dispatch
refused as a duplicate is not recorded. `Reset` forgets the records, leaving
the queued jobs queued.

`q.RunAll(ctx)` runs the queued jobs in the test's goroutine, through the
registry, until none is left: jobs they dispatch run too, and `jobs.Once`
remembers effects as under a worker. Each job gets one attempt; a failure is
kept with the failed jobs (`q.Failed`) and its error returned, joined with
the others, once the rest ran; when `ctx` ends, `RunAll` stops and leaves
the interrupted job queued. The queue's clock stands still (from
`jobstest.WithStart(t)`, or the time `New` was called), so delayed jobs wait
until `q.Advance(d)` moves it past them.

The queue has a registry of its own, stamped with its clock and without the
app's propagators; add them with
`jobstest.WithRegistryOptions(jobs.WithPropagator(framework.JobPropagator()))`
when a test checks the request ID a job carries (or pass a whole registry
with `jobstest.WithRegistry`). Put jobs on the queue through `q` (its
dispatcher or `q.Push`), which is what records them.

The queue is a real one (a `*jobs.MemoryQueue` underneath), so a test of a
retry policy runs a `jobs.Worker` over it. The worker schedules retries by
the wall clock, so build that queue with `jobstest.WithWallClock()`.

## Context propagation

A `jobs.Propagator` copies values from the dispatching context into
`Envelope.Metadata` and back into the handler's context. With
`framework.JobPropagator()`, a job carries the request and trace IDs of the
request that queued it, so `framework.GetRequestIDFromContext(ctx)` in the
handler (and the logs that use it) correlate the job with that request.

See [`examples/jobs`](../examples/jobs/main.go) for the whole contract in one
program.
