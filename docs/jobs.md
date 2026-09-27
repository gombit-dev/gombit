# Background jobs

> **Status:** the job contract (JOBS-1) and queue drivers (JOBS-2).
> `gombit worker`, retries, delayed and failed jobs land in the rest of the
> [JOBS-0 epic](https://github.com/gombit-dev/gombit/issues/278). Until the
> worker ships, a queued job waits for a consumer you run yourself (see
> [Consuming a queue](#consuming-a-queue)).

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
`{<namespace>:jobs:<queue>}` (a ready list, delayed and reserved sorted sets,
and a hash per job), so it works on Redis Cluster, and each operation is one
Lua script: a crash between steps cannot lose or duplicate a job.

## Consuming a queue

Delivery is leased. `Queue.Reserve` hands out the next job with a lease;
`Ack` completes it; `Release` puts it back, optionally delayed. If the lease
runs out first (the worker crashed or stalled), the job is delivered again
and its attempt count goes up. A worker whose lease was taken over gets
`ErrLeaseLost` from `Ack`/`Release`. `gombit worker` (JOBS-3) runs this loop;
until then:

```go
q := app.Jobs().Queue() // nil with the sync driver
d, err := q.Reserve(ctx, []string{"default"}, time.Minute) // jobs.ErrNoJob when empty
if err == nil {
	if err := app.Jobs().Registry().Run(ctx, d.Envelope); err != nil {
		_ = q.Release(ctx, d, time.Now().Add(time.Minute))
	} else {
		_ = q.Ack(ctx, d)
	}
}
```

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
