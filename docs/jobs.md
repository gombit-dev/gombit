# Background jobs

> **Status:** the job contract (JOBS-1), queue drivers (JOBS-2), and the
> worker (JOBS-3). Retry policies, delayed dispatch, and failed-job handling
> land in the rest of the
> [JOBS-0 epic](https://github.com/gombit-dev/gombit/issues/278).

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

In development, `gombit worker` runs `go run ./cmd/server worker` with the
same flags:

```sh
GOMBIT_JOBS_DRIVER=redis gombit worker --concurrency 4
```

| Flag | Default | |
|------|---------|--|
| `--queue` | `GOMBIT_JOBS_QUEUE` | queues to consume, highest priority first; repeat or comma-separate |
| `--concurrency` | `1` | jobs running at once |
| `--lease` | `5m` | how long a reserved job is held; renewed every third of it while the handler runs |
| `--shutdown-timeout` | `30s` | how long in-flight jobs get to finish on SIGINT/SIGTERM |

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
  after 10s per attempt so far (capped at 10m; JOBS-4 adds retry limits and
  policies);
- logs one structured entry per outcome (`job_id`, `job`, `queue`, `attempt`,
  `duration`, the failure `kind`, and the propagated request and trace IDs),
  through the app's logger.

On SIGINT/SIGTERM it stops reserving at once and gives in-flight jobs
`--shutdown-timeout` to finish. A job still running then sees its context
canceled and goes back to the queue, available immediately. Then the app's
`OnStop` hooks run. Give your process manager a stop timeout longer than
`--shutdown-timeout`.

**Nothing is lost when a worker crashes.** A job a dead worker was running
keeps its lease in Redis; when the lease expires it is delivered to another
worker, as its next attempt. That is why delivery is at least once and
handlers must be idempotent.

To run a worker inside another process (tests, a single-process app with the
memory driver), call `framework.RunWorker(ctx, app, jobs.WorkerOptions{...})`,
or build one with `jobs.NewWorker(registry, queue, opts)`.

A stored envelope that no longer decodes cannot run anywhere. `Reserve` still
hands it out, leased, with the failure on `Delivery.Err` (and a nil error, so
an `if err != nil` caller cannot drop the lease); the worker logs it and acks
it away. A consumer of your own must do the same, or the job returns on every
lease expiry. Dead-lettering such jobs arrives with JOBS-6.

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
| `handler` | `ErrHandler` | the handler returned an error, which `errors.Is/As` still reach |

Use `Classify`, not `errors.Is`, to decide a failure's kind: a handler that
returns another job's failure is `handler`, though `errors.Is` still reaches the
inner sentinel through the wrap chain. Which failures are retried is up to the
worker's retry policy (JOBS-4).

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

## Context propagation

A `jobs.Propagator` copies values from the dispatching context into
`Envelope.Metadata` and back into the handler's context. With
`framework.JobPropagator()`, a job carries the request and trace IDs of the
request that queued it, so `framework.GetRequestIDFromContext(ctx)` in the
handler (and the logs that use it) correlate the job with that request.

See [`examples/jobs`](../examples/jobs/main.go) for the whole contract in one
program.
