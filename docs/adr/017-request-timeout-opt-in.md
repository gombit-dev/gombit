# ADR-017: The per-handler request timeout is opt-in

## Status

**Accepted.** Implements the decision leg of issue #270 (PERF-12), the fourth
child of the framework-tax allocation milestone #264 (PERF-6). Supersedes the
"investigate" leg of #242.

## Context

`framework.App` installed a `request_timeout` middleware on **every** request
with a default `HTTP.RequestTimeout = 60s`. On the common path — a request with
no inbound deadline — `context.WithTimeout` is intrinsic: it allocates a
`timerCtx`, a runtime timer, a cancel closure, and (merged into
`request_context` by #268) a `Request.WithContext`.

Measured cost (per-layer ablation, `alloc_objects`):

- `+request_timeout` = **+5 allocs/op on every scenario** (≈+4 after the
  `request_context` merge in #268), ≈0.45 µs.
- `alloc_objects`: `context.WithDeadlineCause` 3/op, `time.newTimer` 1/op,
  `Request.WithContext` 1/op.
- Under a real `http.Server` the parent is the connection's cancel context, so
  `propagateCancel` registers the child too — one more allocation than the
  httptest harness shows.
- Prototype with the deadline disabled: plaintext 22→18 allocs/op, ≈2.1→1.7 µs.

The per-handler deadline is a genuinely useful feature — it propagates
cancellation into DB/cache calls that honor `ctx` — but paying for it on every
request of every app, on by default, is the wrong trade for a framework whose
milestone target (#264) is cutting per-request allocations.

Separately, `HTTP.RequestTimeout` was overloaded: it drove **both** the
per-handler context deadline **and** the `http.Server`
`ReadTimeout`/`WriteTimeout`/`IdleTimeout`. Those connection-level timeouts are
a DoS safety net (slow/stuck sockets), unrelated to whether an individual
handler's work is bounded.

## Decision

Adopt **option A of #270: the per-handler timeout is opt-in.**

1. **Default `HTTP.RequestTimeout = 0`.** Zero disables the per-handler context
   deadline.
2. **Omit the layer, don't pass through.** `runtimeMiddlewareStack` appends the
   `request_timeout` middleware only when `RequestTimeout > 0`. A disabled
   deadline costs nothing on the request path and produces no ablation row — not
   a no-op handler that still sits in the chain.
3. **Decouple the connection-level safety net.** The `http.Server`
   `ReadTimeout`/`WriteTimeout`/`IdleTimeout` fall back to
   `defaultHTTPServerTimeout` (60s) when `RequestTimeout <= 0`, so disabling the
   per-handler deadline never leaves the server with unbounded connection
   timeouts. `ReadHeaderTimeout` stays a fixed 5s. When `RequestTimeout` is set
   to a positive value it still drives all three, exactly as before.
4. **Scaffolded apps keep today's behavior.** `gombit new` writes
   `GOMBIT_HTTP_REQUEST_TIMEOUT=60s` into `.env.example`, so a new project gets
   a per-handler deadline out of the box and opts in explicitly rather than
   inheriting it invisibly.

## Consequences

- **Breaking for apps that relied on the implicit default.** An app upgrading
  without setting `GOMBIT_HTTP_REQUEST_TIMEOUT` loses the per-handler context
  deadline: a long-running DB query is no longer cancelled at 60s, and a slow
  handler keeps running after `WriteTimeout` closes the connection. Apps that
  want the deadline set `GOMBIT_HTTP_REQUEST_TIMEOUT` (any positive duration);
  scaffolded apps already do. Recorded in the CHANGELOG as a behavior change.
- **The connection-level safety net is unchanged by default** (still 60s), so
  this is not a DoS regression — only the cooperative per-handler cancellation
  is off until opted in.
- **Allocation win.** With the deadline disabled the stack drops ≈4 allocs/op on
  every scenario (plaintext 22→18 in the prototype ladder), contributing to the
  #264 "timeout and sanitizer opt-in" target column.
- **`GOMBIT_HTTP_REQUEST_TIMEOUT=0` no longer disables the server timeouts.**
  Previously an explicit `0` zeroed all four; now `0` (the default) means "no
  per-handler deadline, server timeouts fall back to 60s." Turning the
  connection-level timeouts fully off is not something a value of this variable
  can express any more; that is an intentional narrowing (an unbounded server
  timeout is rarely desirable and never a safe default).

## Alternatives considered

- **B. Keep it on by default** (#270's option B): rejected. Cancellation of
  handler work is worth 4 allocs to some apps, but not to all apps on every
  request; opt-in gives the apps that want it the same behavior while removing
  the tax from the ones that don't.
- **A separate `HTTP.ServerTimeout` config field** so the connection-level
  timeouts are independently tunable from env: deferred. The fixed 60s fallback
  is a sane safety net; a dedicated knob can be added later without another
  breaking change if demand appears. Adding config surface was out of scope for
  this decision.
