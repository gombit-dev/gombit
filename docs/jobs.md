# Background jobs

> **Status:** the job contract (JOBS-1). Queue drivers, `gombit worker`,
> retries, delayed and failed jobs land in the rest of the
> [JOBS-0 epic](https://github.com/gombit-dev/gombit/issues/278). This page
> covers what exists: typed jobs, the registry, and the envelope a queue
> stores.

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
- **The payload is the struct's JSON.** Carry IDs, not secrets or large
  bodies: a queue is stored, replicated, and inspected by operators. The
  handler loads what it needs by ID.

## Registering handlers

```go
registry := jobs.NewRegistry(jobs.WithPropagator(framework.JobPropagator()))

jobs.MustRegister(registry, func(ctx context.Context, job SendWelcomeEmail) error {
	return mailer.Welcome(ctx, job.UserID)
})
```

Register every job at startup. `Register` returns an error (and
`MustRegister` panics) for a programming mistake: an invalid name, a name
another type already uses, a pointer type, a payload that cannot be encoded as
JSON, or a bad version. The handler receives the decoded job, typed.

## Dispatch and run

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
| `decode` | `ErrDecode` | the envelope (no name or ID) or payload (empty, `null`, wrong shape) does not decode into the job type |
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

`Run` chains the steps (1→2→3…) before decoding. A version newer than the
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
