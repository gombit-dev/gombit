# Fault injection and chaos testing

Gombit's ordinary tests ask *does the framework work when its dependencies
work?* This suite asks the other question: *what happens when they fail
halfway through?* A database that drops a connection mid-transaction, a
dependency that stops answering, a client that goes away, a COMMIT that
fails: these are the paths behind most production incidents, and the ones
ordinary tests skip because they are hard to reproduce by hand.

Here infrastructure failure is input, not an exceptional condition. The suite
pins down a small set of invariants (INV-1 to INV-8 below), tests each one
where it can break, and keeps one property above all: **a red build means
something is actually wrong.**

- [The three layers](#the-three-layers)
- [Invariants](#invariants): what is verified, and by which tests
- [Guarantees vs application responsibilities](#guarantees-vs-application-responsibilities)
- [Running the suites](#running-the-suites)
- [Reproducing a chaos failure](#reproducing-a-chaos-failure)
- [Adding a scenario](#adding-a-scenario): primitives, naming, failure classes
- [Worked examples](#worked-examples)
- [Rules for contributors](#rules-for-contributors)

## The three layers

```text
                    ┌──────────────────────────┐
                    │  3. Stochastic chaos     │  make test-chaos
                    │  nightly / on demand     │  never a PR check
                    └────────────┬─────────────┘
                    ┌────────────▼─────────────┐
                    │  2. Network faults       │  make test-faults (integration)
                    │  real Postgres / Redis   │  PR CI
                    │  through a TCP proxy     │
                    └────────────┬─────────────┘
                 ┌───────────────▼───────────────┐
                 │  1. Deterministic injection   │  make test-faults
                 │  wrapped driver / loopback    │  PR CI
                 │  HTTP dependency              │
                 └───────────────────────────────┘
```

1. **Deterministic fault injection.** `internal/faulttest` wraps a real
   `database/sql` driver (SQLite, PostgreSQL, MySQL) and serves a scripted
   loopback HTTP dependency. A test fails *exactly* the call it names, such as
   "the second insert into `fault_tx_children`" or "the first COMMIT", rather
   than "15% of calls". These tests are `TestFault_*`, run on every PR, and
   make up most of the suite.
2. **Network faults.** The same kind of test against a real Postgres or Redis
   reached through `faulttest.TCPProxy`, an in-process proxy that the test
   tells to stall, drop (FIN or RST), refuse, or heal at an explicit point.
   These catch what a mock cannot, such as a driver that waits out a dead
   socket past its caller's deadline. They are still deterministic and still
   run on PRs.
3. **Stochastic chaos.** `internal/chaos` draws the fault, its boundary, and
   the sizes at random, many times over, to find the cases nobody wrote down.
   Every run prints one seed that replays it exactly. It runs nightly and on
   demand, never as a PR check.

A chaos failure that is understood becomes a layer-1 or layer-2 test, so the
regression is caught on every PR from then on.

## Invariants

The tables claim only what a test asserts, and each row says what that is.
Regressions were planted to check that the tests catch them:

- `App.Tx` ignoring its commit error;
- `App.Tx` or the request middleware dropping the caller's context (the
  `go downstreamWork(context.Background())` class);
- a non-atomic token rotation;
- an admin write outside a transaction;
- a response body never closed;
- a missing request timeout;
- unbounded, cancellation-deaf, and overflowing retry policies.

A component with no test for an invariant does not claim it.

### INV-1: no partial transaction state

A failed transactional operation leaves no partially committed domain state,
and never reports success for a write that did not persist.

| Where it holds | Enforced by |
| --- | --- |
| `App.Tx`: a statement fails, the COMMIT fails, the context is canceled mid-statement, `fn` panics (even when the rollback also fails), every insert into its tables fails | [`framework/fault_test.go`](../../framework/fault_test.go): `TestFault_Database_TransactionRollback`, `_CommitFailure`, `_Cancellation`, `_PanicRollsBack`, `_Unavailable`, `_RetryableError` |
| `App.Tx` when the connection drops before COMMIT, over a real network | [`framework/network_fault_integration_test.go`](../../framework/network_fault_integration_test.go): `TestFault_Network_ConnectionLostMidTransaction` |
| A losing writer whose COMMIT fails while holding the row lock leaves nothing, and the winner's write stands | [`framework/concurrency_fault_test.go`](../../framework/concurrency_fault_test.go): `TestFault_Concurrency_ConflictingWriters` |
| Cancellation during COMMIT gives a consistent answer: an error means nothing persisted, success means it did | [`framework/concurrency_fault_test.go`](../../framework/concurrency_fault_test.go): `TestFault_Concurrency_CancelDuringCommit` |
| Refresh-token rotation (revoke old + insert new) is atomic under a failed insert, a failed commit, and cancellation | [`auth/fault_test.go`](../../auth/fault_test.go): `TestFault_Database_RefreshRotationAtomic`, `_RefreshRotationCommitFailure`, `_RefreshRotationCanceled` |
| Admin creates, including many-to-many join rows, are atomic, and a failed commit is an error response | [`admin/fault_test.go`](../../admin/fault_test.go): `TestFault_Database_AdminManyToManyRollback`, `_AdminCommitFailure` |
| All or nothing at a random failure boundary. A counter equals the number of transactions that reported success, under random failed COMMITs. Cancellation at a random point matches what persisted | [`internal/chaos/scenarios_test.go`](../../internal/chaos/scenarios_test.go): `database/failure-boundary`, `database/concurrent-writers`, `context/cancellation-stress` |

Every one of these database tests except
`TestFault_Network_ConnectionLostMidTransaction` also checks, with
`faulttest.Idle`, that no transaction was left holding a connection
(directly, or through `assertNoFamily`, `assertUnrotated`, or
`assertNoEngine`). A row count alone cannot see an open transaction. The
network test checks the error, the row count, and that the pool recovers.

### INV-2: bounded waiting

Where a context deadline or cancellation exists, a failing dependency cannot
block the caller past it.

| Where it holds | Enforced by |
| --- | --- |
| A handler's query, or its `App.Tx`, ends at `HTTP.RequestTimeout` | [`framework/context_fault_test.go`](../../framework/context_fault_test.go): `TestFault_Context_HandlerDeadline_DB`, `_HandlerDeadline_Tx` |
| A handler's outbound HTTP call to a hung dependency ends at the request deadline | [`framework/http_fault_test.go`](../../framework/http_fault_test.go): `TestFault_HTTP_HandlerDeadline` |
| The response a handler writes at its request deadline reaches the client over a real connection (the server's write deadline outlasts the handler's) | [`framework/http_fault_test.go`](../../framework/http_fault_test.go): `TestFault_HTTP_HandlerDeadlineResponseDelivered` |
| Shutdown with requests stuck in the database returns within drain delay + shutdown timeout | [`framework/context_fault_test.go`](../../framework/context_fault_test.go): `TestFault_Context_ShutdownUnderLoad` |
| A query against a stalled database ends at the caller's deadline (`context.DeadlineExceeded`). A caller waiting on an exhausted pool honors its deadline. A refusing database fails the call at once (under a second) | [`framework/network_fault_integration_test.go`](../../framework/network_fault_integration_test.go): `TestFault_Network_Latency`, `_PoolExhaustion`, `_Unavailable` |
| A cache call against a stalled Redis ends at the caller's deadline | [`cache/network_fault_integration_test.go`](../../cache/network_fault_integration_test.go): `TestFault_Network_RedisLatency` |
| `gombit openapi` / `gombit dev` spec fetches against a hung server end at the caller's deadline | [`cli/fault_test.go`](../../cli/fault_test.go): `TestFault_HTTP_Timeout`; [`dev/fault_test.go`](../../dev/fault_test.go): `TestFault_HTTP_DevSpecFetchTimeout` |
| Random stalls, drops, and restarts of Postgres; random HTTP dependency scripts behind a request timeout | [`internal/chaos/scenarios_test.go`](../../internal/chaos/scenarios_test.go): `postgres/interruption`, `http/dependency-faults` |

Three tests bound a failure where the caller sets **no** deadline, so they
check a fixed ceiling rather than a deadline:

- a query whose connection is dropped (FIN or RST) while the database stays
  unreachable fails within 5 seconds of the drop (`TestFault_Network_ConnectionLost`);
- a Redis command whose connection drops fails within 5 seconds
  (`TestFault_Network_RedisConnectionLost`);
- a refusing Redis fails within go-redis's bounded dial retries, about 2
  seconds and under a 4-second ceiling (`TestFault_Network_RedisUnavailable`).

### INV-3: cancellation propagation

Cancellation reaches the downstream work it started: the query, the
transaction, the outbound call. It is not left running.

| Where it holds | Enforced by |
| --- | --- |
| A client that disconnects cancels its in-flight query (`context.Canceled`) and its outbound HTTP call. The dependency sees the call abandoned | [`framework/context_fault_test.go`](../../framework/context_fault_test.go): `TestFault_Context_ClientDisconnect_DB`, `_ClientDisconnect_HTTP` |
| The request deadline reaches the dependency, whose outbound call is abandoned there with its body closed | [`framework/http_fault_test.go`](../../framework/http_fault_test.go): `TestFault_HTTP_HandlerDeadline` |
| `App.Tx` carries the caller's context into every statement, so cancellation rolls it back | [`framework/fault_test.go`](../../framework/fault_test.go): `TestFault_Database_Cancellation`; [`framework/context_fault_test.go`](../../framework/context_fault_test.go): `TestFault_Context_HandlerDeadline_Tx`; [`auth/fault_test.go`](../../auth/fault_test.go): `TestFault_Database_RefreshRotationCanceled` |
| Shutdown cancels the stuck requests' queries, leaving none running | [`framework/context_fault_test.go`](../../framework/context_fault_test.go): `TestFault_Context_ShutdownUnderLoad` |
| The CLI's abandoned fetch does not keep running on the server | [`cli/fault_test.go`](../../cli/fault_test.go): `TestFault_HTTP_Timeout`; [`dev/fault_test.go`](../../dev/fault_test.go): `TestFault_HTTP_DevSpecFetchTimeout` |
| Canceled during a random insert, a transaction ends with `context.Canceled` and nothing persisted. The held insert is never released, so a transaction that dropped its context fails the scenario instead of passing | [`internal/chaos/scenarios_test.go`](../../internal/chaos/scenarios_test.go): `context/cancellation-stress` |

### INV-4: bounded retry

Every retry ends in success or an observable terminal error, within a bounded
number of attempts, and stops when its context is canceled.

Apart from background jobs, Gombit's own code paths mostly **do not retry**,
which is the simplest way to keep this invariant, and that is what the tests
pin. Jobs are the exception: a failed job is retried under its `jobs.Options` policy
(`MaxAttempts`, `Backoff`; see [jobs.md](../jobs.md#retries-and-timeouts)), and
the `jobs` package's own tests hold it to this invariant:

| Where it holds | Enforced by |
| --- | --- |
| `App.Tx` returns a driver's retryable error (serialization failure, deadlock, `SQLITE_BUSY`) as a terminal error the caller can recognize, and runs `fn` once | [`framework/fault_test.go`](../../framework/fault_test.go): `TestFault_Database_RetryableError` |
| The CLI's spec fetch reports a 429 at once and does not retry | [`cli/fault_test.go`](../../cli/fault_test.go): `TestFault_HTTP_TooManyRequests` |
| Callers that rotate a refresh token while another rotation of it is in flight join that rotation. The test pins four of them inside it before the stalled rotation fails. Every caller gets the terminal error, none is wedged, and only the one rotation touched the database (a single INSERT: nobody retried or ran their own) | [`auth/concurrency_fault_test.go`](../../auth/concurrency_fault_test.go): `TestFault_Concurrency_RotationLeaderFails` |
| The one retrying client Gombit wraps, go-redis, finishes its dial retries in bounded time | [`cache/network_fault_integration_test.go`](../../cache/network_fault_integration_test.go): `TestFault_Network_RedisUnavailable` |
| A failing job runs at most `MaxAttempts` times and a `jobs.Permanent` failure is not retried; after that the worker moves it to the failed jobs | [`jobs/worker_test.go`](../../jobs/worker_test.go): `TestWorkerRetriesAFailedJob`, `TestWorkerGivesUp` |
| Job backoffs are capped and do not overflow | [`jobs/policy_test.go`](../../jobs/policy_test.go): `TestBackoffs`, `TestExponentialDoesNotOverflow` |
| While the queue is unreachable, the worker's poll loop backs off: the wait doubles from the poll interval, capped at 30s | [`jobs/worker_test.go`](../../jobs/worker_test.go): `TestReserveBackoff` |

Job retries do not wait in-process: the worker releases the job back to the
queue with its backoff as the time it becomes available again, so there is no
sleep for `faulttest.CheckRetryPolicy` to drive, and no production code calls it
today. An in-process retry policy Gombit adds (one that sleeps between
attempts) must pass `faulttest.CheckRetryPolicy` (see
[Adding a scenario](#retry-policies)). The checker's own tests prove that it
rejects unbounded, cancellation-deaf, and overflowing policies:
[`internal/faulttest/retry_test.go`](../../internal/faulttest/retry_test.go),
`TestCheckRetryPolicyRejectsBrokenPolicies`.

### INV-5: no panic on infrastructure failure

An expected dependency failure is an error the caller handles, not a
process-level panic. Every `TestFault_*` test fails on a panic. These in
particular drive a dependency into total failure:

- [`framework/fault_test.go`](../../framework/fault_test.go): `TestFault_Database_Unavailable` (every insert into its tables fails: an error from `App.Tx`, not a panic, and nothing persists).
- [`framework/network_fault_integration_test.go`](../../framework/network_fault_integration_test.go): `TestFault_Network_Unavailable`, `_ConnectionLost` (refused and dropped connections).
- [`cli/fault_test.go`](../../cli/fault_test.go): `TestFault_HTTP_ServerError`, `_ConnectionReset`, `_MalformedResponse` (a malformed body is rejected, never written).
- [`dev/fault_test.go`](../../dev/fault_test.go): `TestFault_HTTP_DevSpecFetch`.

A panic raised by *application code* inside `App.Tx` is a different case. It
is not swallowed: the transaction rolls back and the panic propagates
(`TestFault_Database_PanicRollsBack`).

### INV-6: recovery

Once a transient outage is lifted, the next independent operation succeeds
without restarting the application.

| Where it holds | Enforced by |
| --- | --- |
| Every Postgres network fault (stall, drop, reset, refuse, pool exhaustion, mid-transaction loss) ends with a healed proxy and a successful next operation on the same `App` | [`framework/network_fault_integration_test.go`](../../framework/network_fault_integration_test.go): every `TestFault_Network_*`, through `assertRecovers` |
| `/readyz` reports `503 not_ready` while the database is unreachable or stalled, and 200 again once it is back | [`framework/network_fault_integration_test.go`](../../framework/network_fault_integration_test.go): `TestFault_Network_ReadyzRecovery` |
| Every Redis network fault ends with a successful cache write on the same client | [`cache/network_fault_integration_test.go`](../../cache/network_fault_integration_test.go): every `TestFault_Network_Redis*`, through `assertRecovers` |
| After an injected statement or commit fault, the next transaction goes through. After a failed rotation, the token still rotates | [`framework/fault_test.go`](../../framework/fault_test.go): `TestFault_Database_TransactionRollback`, `_CommitFailure`; [`auth/concurrency_fault_test.go`](../../auth/concurrency_fault_test.go): `TestFault_Concurrency_RotationLeaderFails` |
| After a random Postgres interruption clears, the next call succeeds | [`internal/chaos/scenarios_test.go`](../../internal/chaos/scenarios_test.go): `postgres/interruption` |

### INV-7: deterministic PR checks

The fault suite that runs on PRs is reproducible and never depends on random
scheduling to pass. No single test enforces this; the harness and the
process do:

- Faults are placed by call number and by explicit synchronization (`Block`,
  `Reached(n)`, `proxy.Held()`), never by sleeping. See the
  [rules](#rules-for-contributors).
- A new fault test must pass `go test -race -count=50` before it lands.
- The **Fault soak** workflow ([`fault-soak.yml`](../../.github/workflows/fault-soak.yml))
  runs `FAULT_COUNT=100 make test-faults` against all three databases and
  Redis weekly and on demand. The **Fault injection** check becomes a
  required status check only after the soak stays clean.
- The runner's own behavior (discovery by name, sharding, count and budget
  validation) is pinned by [`scripts/test-faults_test.sh`](../../scripts/test-faults_test.sh).

### INV-8: reproducible chaos

Every stochastic failure can be rerun from the seed its run reported.

| Where it holds | Enforced by |
| --- | --- |
| Every registered scenario, run twice at one (seed, iteration), draws the same configuration and reports the same failures. This includes an iteration that cancels during an insert, and the Postgres scenario when `CHAOS_POSTGRES_DSN` is set | [`internal/chaos/replay_test.go`](../../internal/chaos/replay_test.go): `TestScenarioReplays` |
| The random source is a pure function of (seed, scenario, iteration) | [`internal/chaos/replay_test.go`](../../internal/chaos/replay_test.go): `TestRandIsAPureFunctionOfTheKey` |
| No scenario file imports a random source (`math/rand`, `crypto/rand`). This is an import check only: a draw from the clock or from map order is not caught by a test, which is why a seed that does not replay is treated as a harness bug | [`internal/chaos/replay_test.go`](../../internal/chaos/replay_test.go): `TestScenariosDrawOnlyFromEnvRand` |
| A replay cannot pass by not running. The replay command pins `CHAOS_POSTGRES`, and `CHAOS_POSTGRES=1` refuses to run without a DSN. A selected scenario that needs Postgres fails instead of skipping | [`internal/chaos/replay_test.go`](../../internal/chaos/replay_test.go): `TestPostgresModeIsPinned` (the selected-scenario rule is `Environment.RequirePostgres`, checked by hand) |
| The nightly summary's replay commands carry the run's quoted DSN, `CHAOS_POSTGRES`, and iteration count, and hand the suite the same DSN back. The artifacts describe only the run that produced them | [`scripts/chaos-run_test.sh`](../../scripts/chaos-run_test.sh) |

## Guarantees vs application responsibilities

The invariants above describe Gombit's code. Your application keeps them only
if it uses that code the way the tests do. Everything below either restates
a tested guarantee or states explicitly that Gombit does **not** provide
something.

### What Gombit guarantees

- **`App.Tx` is all or nothing.** The transaction rolls back and `App.Tx`
  returns an error (or re-panics) when any of these happens:
  - `fn` returns an error or panics;
  - a statement fails;
  - the COMMIT fails;
  - the context is canceled or times out while a statement is running.

  A context canceled **during COMMIT** gets an answer that matches what
  persisted, and that answer depends on the driver. On SQLite and MySQL the
  commit completes and `App.Tx` returns `nil` with the row there. On
  PostgreSQL it is aborted and `App.Tx` returns `context.Canceled` with
  nothing persisted (`TestFault_Concurrency_CancelDuringCommit`). Either
  way, `App.Tx` never returns `nil` for a write that did not persist, nor an
  error for one that did. A connection *lost* during COMMIT is different:
  see the application responsibilities.
- **`App.Tx` does not retry.** A retryable driver error comes back as the
  driver's error, so you can recognize it (for example `pgconn.PgError`
  code `40001`, MySQL error `1213`, or `SQLITE_BUSY`), and `fn` runs once.
- **The request context carries the request's deadline and its cancellation.**
  With `HTTP.RequestTimeout` set (`GOMBIT_HTTP_REQUEST_TIMEOUT`, which is off by
  default), a handler's context ends at the timeout. When
  the client disconnects, it is canceled. `App.Tx` carries that context into
  every statement.
- **Shutdown is bounded.** `/readyz` reports draining, `RunContext` returns
  within drain delay + shutdown timeout, and queries stuck in the database are
  canceled.
- **The database pool recovers.** After an outage, the next operation on the
  same `App` succeeds, and `/readyz` returns to 200 without a restart. A caller
  waiting for a pooled connection honors its deadline.
- **The Redis cache honors the caller's deadline.** It does not fall back to
  the client's own read timeout.
- **Framework features built on these are atomic.** A refresh-token
  rotation revokes the old token and inserts the new one in one
  transaction, under a failed insert, a failed commit, or cancellation.
  Callers that rotate the same token while its rotation is in flight share
  that rotation's result, failure included. A caller arriving after it
  ended starts a new rotation. Admin creates with many-to-many rows are
  atomic.
- **The CLI's spec fetches** (`gombit openapi generate --url`, and
  `gombit dev`'s fetch from the app server) end at their deadline. `gombit
  openapi generate` also reports 5xx and 429 at once without retrying, and
  rejects a malformed or non-OpenAPI body instead of writing it.

### What your application is responsible for

- **Pass the context through.** Every guarantee about deadlines and
  cancellation assumes your handler's database and outbound calls use the
  request's context (`c.Request.Context()` in Gin, `ctx` in a Huma handler, the
  `tx` `App.Tx` hands you). A call made with `context.Background()`, or on a
  goroutine that outlives the request, is outside INV-2 and INV-3.
- **Set your deadlines.** `HTTP.RequestTimeout` defaults to 0 (no deadline),
  so set it. Give your own
  `http.Client` and any long-running work a timeout. Gombit bounds what it
  owns, not a client you construct.
- **Retry deliberately, if at all.** Gombit does not retry transactions. To
  retry serialization failures or deadlocks, write a bounded policy (with
  attempts, backoff, and context) around a whole `App.Tx`, and make `fn` safe
  to run again. `faulttest.CheckRetryPolicy` is Gombit's check for an
  in-process retry policy, and you can copy its contract.
- **Side effects outside the database are not rolled back.** An email sent,
  an HTTP call made, or a file written inside `fn` stays done when the
  transaction rolls back. Gombit provides **no exactly-once semantics**. A
  request whose effect succeeded but whose response was lost can be retried by
  its client and applied twice. Use idempotency keys, an outbox, or
  compensation where that matters.
- **An interrupted COMMIT can be ambiguous.** When a context is canceled
  during COMMIT, `App.Tx`'s answer matches what persisted
  (`TestFault_Concurrency_CancelDuringCommit`). When the *connection* is lost
  while the COMMIT is in flight, the client cannot know whether the server
  committed, and Gombit does not resolve that for you. Design writes that
  matter so they can be checked or safely repeated.
- **Writes outside `App.Tx` are not atomic together.** Two `Create` calls
  outside a transaction can leave one without the other. Concurrent updates
  to one row are last-writer-wins unless you use optimistic locking (see
  [validation.md](../validation.md)).
- **Validate what your dependencies return.** Gombit rejects a malformed
  OpenAPI document in its own CLI. Your handlers must reject a malformed
  response from a service they call.

Components not listed here have no resilience guarantees from this suite
yet, even where they have ordinary tests. They gain them as their own
`TestFault_*` tests land.

## Running the suites

### `make test-faults`: deterministic, PR-safe

```bash
make test-faults
```

This runs, under the race detector, the `internal/faulttest` harness's own
tests and every `TestFault_*` test in the repository. Tests are found by name,
so a new one joins without editing anything. With no environment set it runs
on SQLite with the loopback HTTP dependency. Set these to add the real
dependencies:

```bash
FAULT_POSTGRES_DSN='postgres://gombit:gombit@127.0.0.1:5432/gombit?sslmode=disable' \
FAULT_MYSQL_DSN='gombit:gombit@tcp(127.0.0.1:3306)/gombit?parseTime=true' \
FAULT_REDIS_ADDR=127.0.0.1:6379 \
  make test-faults
```

(Start those with the `docker run` lines in
[CONTRIBUTING.md](../../CONTRIBUTING.md#the-database-matrix), plus
`docker run --rm -d -p 6379:6379 redis:7-alpine`.)

| Variable | Effect |
| --- | --- |
| `FAULT_POSTGRES_DSN`, `FAULT_MYSQL_DSN`, `FAULT_REDIS_ADDR` | Rerun the packages under the `integration` tag against that dependency, one package at a time. They share tables such as the auth ones |
| `FAULT_COUNT=n` | Repeat every test `n` times. `FAULT_COUNT=100` is the soak. It must be plain base-10, since `go test` reads `010` as octal |
| `FAULT_SHARD=i/n` | Run the `i`-th of `n` round-robin package slices, as CI's six parallel jobs do |
| `FAULT_COMPILE_ONLY=1` | Only compile the test binaries (under `-race`), without running them. CI's shards do this first, so the budget measures the run, not the build |
| `FAULT_BUDGET_SECONDS=s` | Fail a run that takes longer than `s` seconds. CI uses 120 per shard, after a `FAULT_COMPILE_ONLY=1` step. The remedy for a slow shard is another shard, never a dropped scenario |

To iterate on one test, run it directly:

```bash
go test -race -run 'TestFault_Database_CommitFailure' ./framework
go test -race -tags integration -run 'TestFault_Network_' ./framework \
  -framework.postgres-dsn 'postgres://gombit:gombit@127.0.0.1:5432/gombit?sslmode=disable'
```

`auth` and `admin` both create and drop the auth tables, so against one
shared database run those packages one at a time (`go test -p 1 ...`, or one
package per command), as `make test-faults` does.

The suite needs no credentials beyond the test databases and never reaches
the internet. A plain `go test ./...` still runs the SQLite-level `TestFault_*`
tests: they are ordinary tests, not a build tag.

### `make test-chaos`: stochastic, never a PR check

```bash
make test-chaos                                    # every scenario, a fresh seed, 20 iterations
CHAOS_POSTGRES_DSN='postgres://...' make test-chaos # add the Postgres scenarios
CHAOS_ITERATIONS=200 make test-chaos               # a longer run
```

The suite lives in `internal/chaos` behind the `chaos` build tag, so
`go test ./...` never compiles it. Its first line of output is the seed:

```text
CHAOS_SEED=8303673706723925916
```

| Variable | Effect |
| --- | --- |
| `CHAOS_SEED` | Replay a run. Without it, a seed is drawn and printed |
| `CHAOS_SCENARIO` | Run one scenario (`database/failure-boundary`, …) |
| `CHAOS_ITERATION` | Run one iteration |
| `CHAOS_ITERATIONS` | How many iterations (default 20) |
| `CHAOS_POSTGRES_DSN` | Adds the Postgres scenarios and lets the database scenarios draw Postgres. Without it they use SQLite, and `postgres/interruption` skips in a full run (a run that selects it fails instead) |
| `CHAOS_POSTGRES` | Pins the Postgres configuration, as every replay command does. `1` refuses to run without `CHAOS_POSTGRES_DSN`, `0` ignores it, unset follows the DSN |
| `CHAOS_REPORT_DIR` | Also write each failure report to a file there |

The **Chaos** workflow ([`chaos.yml`](../../.github/workflows/chaos.yml)) runs
50 iterations nightly against an ephemeral Postgres, and on demand (Actions →
Chaos → Run workflow, with an optional seed, scenario, and iteration count).
It is never a PR check and not a required status check.

## Reproducing a chaos failure

A failing iteration prints a report. This one came from `App.Tx` planted to
ignore its commit error:

```text
CHAOS FAILURE

scenario: database/failure-boundary
component: database
seed: 8303673706723925916
iteration: 2
package: github.com/gombit-dev/gombit/internal/chaos
test: TestChaos/database/failure-boundary/iter-2
postgres: not configured

drawn:
  sqlite, 3 inserts, fault at COMMIT after 3 inserts

sqlite: a transaction failing at COMMIT after 3 inserts

expected:
  the injected fault and 0 rows

observed:
  <nil> and 0 rows

replay:
  CHAOS_POSTGRES=0 CHAOS_SEED=8303673706723925916 CHAOS_SCENARIO=database/failure-boundary CHAOS_ITERATION=2 make test-chaos
```

The report holds every message the scenario recorded: its draws
(`env.Drew`), each violated invariant (`env.Mismatch`), why it stopped
early (`env.Fatalf`), and any failure a faulttest helper reported through
`env.TB(t)`, such as `stopped: 1 connection(s) still in use`. The replay line pins `CHAOS_POSTGRES` but never
prints the DSN.

1. **Get the report.** Locally it is in the test output.

   From the nightly run, open the job summary. It lists the seed, a command
   that replays the whole run, and every failure's replay command in
   iteration order. Those commands carry everything that decides what runs:
   the run's quoted `CHAOS_POSTGRES_DSN` (the workflow's throwaway database
   at `127.0.0.1:5432`), `CHAOS_POSTGRES=1`, and, for the whole run, the
   iteration count.

   For more, download the `chaos-<run id>` artifact. It describes that run
   only:
   - `summary.md`: the same commands;
   - `reports/<scenario>-iter-<n>.txt`: one report per failed iteration, as
     above. The scenario's `/` becomes `_`, so for example
     `reports/database_failure-boundary-iter-2.txt`;
   - `test.json`: the full `go test -json` output, including race reports;
   - `go-stderr.txt`: the `go` command's own stderr, where a build error
     lands;
   - `environment.txt`: the `CHAOS_*` settings in effect;
   - `postgres.log` and `postgres-state.json`: the database container's logs
     and state.
2. **Run the replay line.** It reruns that scenario and iteration with the
   same draws, and the `drawn:` section lists them: database, fault,
   boundary, sizes.

   For a run that had Postgres, start one at the DSN's address (the
   `docker run` in [CONTRIBUTING.md](../../CONTRIBUTING.md#the-database-matrix),
   on port 5432) or export your own `CHAOS_POSTGRES_DSN`.
   `CHAOS_POSTGRES=1` refuses to run without it, so a replay cannot quietly
   switch a Postgres failure to SQLite. A scenario draws the same way
   whatever address the database is at.
3. **Rerun the whole run** with the summary's whole-run command if the
   failure only shows among its neighbors.
4. **Pin it.** Once you understand the failure, write a deterministic
   `TestFault_*` test that fails at that exact boundary, fix the bug, and keep
   the test. The chaos suite finds bugs; the fault suite keeps them fixed.

The seed fixes every choice a scenario makes, but not the Go scheduler. In
`database/concurrent-writers` the seed fixes how many writers there are and
which COMMITs (in commit order) fail. Which goroutine reaches which COMMIT
is the scheduler's choice. The invariant holds for every interleaving, so
the replay recreates the conditions but not necessarily the thread order.
Run it with `-count` until it reproduces:

```bash
CHAOS_POSTGRES=0 CHAOS_SEED=<seed> CHAOS_SCENARIO=database/concurrent-writers \
  CHAOS_ITERATION=<i> go test -tags chaos -race -count=20 -run TestChaos ./internal/chaos
```

**A failure that does not reproduce from its seed is a harness bug, not a
flake.** Find the randomness the scenario drew from somewhere other than
`env.Rand`: the global `math/rand`, the clock, map iteration order, or an
unsynchronized goroutine. Only the first of those is caught by a test.

## Adding a scenario

### Primitives

Everything is in `internal/faulttest` (test-only; production code never
imports it).

| Primitive | What it does |
| --- | --- |
| `FailAlways(err)`, `FailOnce(err)`, `FailNTimes(n, err)`, `FailOnCall(n, err)` | An `*Injector` that fails exactly those calls |
| `Delay(d)`, `BlockUntil(release)` | Hold calls for a duration, or until a channel closes or the call's context ends. On `DBFaults.Commit` and `Rollback` there is no call context (see `OpenDB`), so a hold there ends only on its timer or channel: release it yourself after canceling |
| `Sequence(steps...)`, `SequenceThen(rest, steps...)` | Script the first calls from `Success()`, `Failure(err)`, `Wait(d)`, `Block(release)`, `BlockThenFail(release, err)`; later calls succeed, or do `rest` |
| `inj.Calls()`, `inj.Failures()`, `inj.Reached(n)`, `inj.Reset()`, `inj.Disarm()` / `Arm()` | Counters; a channel closed when call `n` begins (to pin an interleaving); start over; pass calls through uncounted during setup |
| `OpenDB(kind, dsn, &DBFaults{Connect, Begin, Statement, Match, Commit, Rollback})` | A `*database.DB` over the real driver with injectors at each boundary. `Match` (for example `Inserts("table")`) restricts `Statement` to the statements it accepts. `Connect`, `Begin`, and `Statement` are hit with the caller's context. `database/sql` gives a transaction's `Commit` and `Rollback` no context, so those two are hit with `context.Background()`, and canceling the caller does not release a hold on them |
| `ForEachDB(t, dbs, fn)`, `SQLiteDB()` | Run on SQLite, plus PostgreSQL and MySQL under the `integration` tag |
| `Idle(t, db)` | Fail unless every pooled connection is back, so no transaction was left open |
| `NewHTTPDependency(t, steps...)` | A loopback server answering each call with the next step: `Respond(status, body)`, `ServerError()`, `TooManyRequests(retryAfter)`, `Hang(release)`, `CutBody(partial)`, `ResetConnection()`. It has `Calls`, `Reached`, `InFlight`, `Abandoned`, `WaitIdle` |
| `TrackBodies(transport)` | A `RoundTripper` counting response bodies left open |
| `NewTCPProxy(t, upstream)` | The network fault proxy: `Hold()` / `Held()`, `Cut()`, `Reset()`, `Refuse()`, `Heal()`. `PostgresHostPort(dsn)` and `PostgresDSNVia(dsn, addr)` route a DSN through it |
| `Sleeper`, `RealSleeper`, `FakeSleeper`, `CheckRetryPolicy(t, RetryContract{...})` | The time seam and the conformance check for retry policies |

### Naming

A fault test is named `TestFault_<Component>_<Scenario>`, where the scenario
usually names its failure class:

- `TestFault_Database_CommitFailure`
- `TestFault_Network_PoolExhaustion`
- `TestFault_Context_ClientDisconnect_HTTP`
- `TestFault_Concurrency_ConflictingWriters`

The component says what fails or which layer is exercised: `Database`,
`Network` (a real dependency through the TCP proxy), `HTTP`, `Context`,
`Concurrency`, and in future `Retry`, `Jobs`, and so on. The `TestFault_`
prefix is what puts a test in `make test-faults`. CI's integration database
jobs skip that prefix, so the Postgres/MySQL/Redis fault matrix runs only in
the `Fault injection` job. The SQLite-level fault tests also run in the
ordinary `Test` job's `go test ./...`.

A chaos scenario is named `<component>/<scenario>` in kebab case (for
example `database/failure-boundary`), with `Component` set to match.

### Failure classes

Describe what a test injects with one of these classes, in its name or its
doc comment:

| Class | Meaning |
| --- | --- |
| `availability` | The dependency is down: refused, every call failing |
| `latency` | The dependency answers slowly or stops answering |
| `timeout` | A deadline expires while waiting |
| `cancellation` | The caller goes away: a client disconnect, `cancel()`, shutdown |
| `connection-loss` | An open connection drops (FIN or RST), mid-query or mid-transaction |
| `partial-operation` | A multi-step operation fails between steps: statement *k* of *n*, or COMMIT |
| `retry` | A retryable error and what is (or is not) done about it |
| `duplicate-delivery` | The same logical work arrives twice |
| `concurrency` | A fault at a boundary another operation is racing |
| `resource-exhaustion` | Pool, connections, or memory used up |
| `recovery` | The fault is lifted and the next operation must succeed |

### Deterministic fault test

1. Pick the invariant and the exact boundary: "the second insert fails",
   "COMMIT fails", "the dependency stalls after the request is sent".
2. Build the dependency with its injectors **disarmed**, create the tables and
   fixtures, then `Arm()`, so setup is never numbered.
3. Drive the operation, then assert **what the outside world sees**: the
   returned error (`errors.Is` the injected one, or `context.Canceled`), the
   persisted rows, `faulttest.Idle`, `dep.WaitIdle`, body counts. Assert call
   counts only where the call sequence is itself the contract (for example
   "`App.Tx` runs `fn` once").
4. Where recovery is part of the contract, lift the fault and assert that the
   next independent operation succeeds.
5. Check the test catches its regression: plant the bug, watch it fail, then
   revert. Then run `go test -race -count=50` (`-count=100` for a
   `TestFault_Concurrency_*` scenario).

### Chaos scenario

Add a function to `internal/chaos/scenarios_test.go` (or a sibling file) and
register it:

```go
func init() {
	register(Scenario{Name: "database/my-scenario", Component: "database", Run: myScenario})
}
```

Every random choice comes from `env.Rand`. Record what was drawn with
`env.Drew`. Report a violated invariant with `env.Mismatch`, which puts the
expected and observed values in the failure report. Stop with
`env.Fatalf`, not `t.Fatalf`, and hand the faulttest helpers `env.TB(t)`
(`faulttest.Idle(env.TB(t), db)`), so the report says why, even when a
leak check stops the scenario. Bound every wait, a query's included: run it
on a goroutine and `select` with a timeout, so a call that ignores its
deadline fails the scenario instead of hanging the suite until the package
timeout.

A scenario that needs Postgres calls `env.RequirePostgres(t)`: it skips in
a full run without a DSN, and fails when the run selected the scenario, so a
replay never passes by skipping. A database scenario picks its database with
`pickDB`, which makes its draw whether or not Postgres is configured, so the
draws after it do not shift. `TestScenarioReplays` runs every registered
scenario twice under one seed, so a new scenario is checked for replay
automatically.

### Retry policies

An in-process retry policy in Gombit waits between attempts through a
`faulttest.Sleeper` and must pass the conformance check. (Job retries are
scheduled through the queue instead and are covered by the `jobs` tests; see
[INV-4](#inv-4-bounded-retry).)

```go
faulttest.CheckRetryPolicy(t, faulttest.RetryContract{
	Do:            policy.Do, // runs op, sleeping through the given Sleeper
	Retryable:     errBusy,
	Permanent:     errInvalid,
	MaxAttempts:   5,
	Delay:         policy.Delay, // wait after the attempt-th failure
	MaxDelay:      10 * time.Second,
	Deterministic: true,
})
```

The check drives the policy with scripted failures and a `FakeSleeper`, so
nothing really waits. `MaxAttempts` is the policy's **budget**: an op that
keeps failing retryably must run exactly that many times. The check reports
every violated property:

- more attempts than `MaxAttempts`, or fewer (a retryable failure not
  retried while budget remains);
- an attempt started on a context that had already ended (`Do` must return
  the context's error without calling the op);
- a cancellation mid-backoff that does not end it;
- a permanent error retried;
- a wrong number of waits (anything but one fewer than the attempts);
- a wait outside `[0, MaxDelay]`;
- with `Deterministic` set only, a wait that is not `Delay(n)` for the
  `n`-th failure (a jittered policy sets it to false and is held to the
  count and the bounds);
- a `Delay` outside `[0, MaxDelay]` at any **sampled** attempt. The sample is
  every attempt up to `max(MaxAttempts, 128)`, and 1000, with 20 calls each
  for jitter. That catches a shift or doubling that overflows, since any base
  of 1ns or more wraps by attempt 64. It does **not** catch an attempt
  multiplied by a unit, which wraps at an attempt that depends on the unit,
  usually far past 1000. Compute a delay so it cannot overflow: cap before
  the arithmetic, not after;
- a success that does not end it;
- a final error that hides the last attempt's error.

A production sleeper must fail the wait at once on a context that has
already ended, however short the delay, as `faulttest.RealSleeper` does.
(Production code cannot import `internal/faulttest`: copy the pattern of
checking `ctx.Err()` before arming the timer.)

## Worked examples

### A deterministic fault test

`App.Tx` must never report success for a transaction whose COMMIT failed.
This is the test that catches `tx.Commit() // error ignored` from
[`framework/fault_test.go`](../../framework/fault_test.go):

```go
func TestFault_Database_CommitFailure(t *testing.T) {
	faulttest.ForEachDB(t, faultDBs, func(t *testing.T, kind database.Driver, dsn string) {
		commit := faulttest.FailOnce(faulttest.ErrInjected)
		// newFaultApp opens the database with the faults disarmed, creates the
		// tables, then arms them: only the test's own calls are counted.
		app := newFaultApp(t, kind, dsn, &faulttest.DBFaults{Commit: commit})

		// A parent and its child in one App.Tx; the first COMMIT fails.
		if err := writeFamily(context.Background(), app); !errors.Is(err, faulttest.ErrInjected) {
			t.Fatalf("Tx = %v, want the commit fault", err)
		}
		if commit.Failures() != 1 {
			t.Fatalf("commit failures = %d, want 1: the Tx did not reach its commit", commit.Failures())
		}
		// Neither row persisted, and no transaction is left holding a connection.
		assertNoFamily(t, app)
		// The fault has passed: the next transaction goes through (INV-6).
		if err := writeFamily(context.Background(), app); err != nil {
			t.Fatalf("the next Tx = %v, want success", err)
		}
	})
}
```

It runs on SQLite under a plain `go test`, and on PostgreSQL and MySQL under
`-tags integration` with the package's DSN flags. `make test-faults` does both.

### A network fault test

A database that stops answering must not hold a query past its caller's
deadline, and the pool must recover once it answers again. From
[`framework/network_fault_integration_test.go`](../../framework/network_fault_integration_test.go):

```go
func TestFault_Network_Latency(t *testing.T) {
	app, proxy := proxiedApp(t, 0) // Postgres through faulttest.NewTCPProxy
	if err := ping(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	proxy.Hold() // the database stops answering
	err := withinDeadline(t, 200*time.Millisecond, func(ctx context.Context) error { return ping(ctx, app) })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a stalled query = %v, want context.DeadlineExceeded", err)
	}
	assertRecovers(t, app, proxy) // Heal(), then the next query succeeds on the same App
}
```

### A chaos scenario

A transaction of *n* inserts fails at a random statement, at COMMIT, or not
at all, and must be all or nothing. Condensed from
[`internal/chaos/scenarios_test.go`](../../internal/chaos/scenarios_test.go):

```go
func failureBoundary(t *testing.T, env Environment) {
	kind, dsn := pickDB(t, env) // SQLite, or Postgres when configured: drawn
	n := 2 + env.Rand.IntN(5)
	faults := &faulttest.DBFaults{Match: faulttest.Inserts("chaos_rows")}
	var boundary string
	switch env.Rand.IntN(3) {
	case 0:
		k := 1 + env.Rand.IntN(n)
		faults.Statement = faulttest.FailOnCall(k, faulttest.ErrInjected)
		boundary = fmt.Sprintf("insert %d of %d", k, n)
	case 1:
		faults.Commit = faulttest.FailOnce(faulttest.ErrInjected)
		boundary = fmt.Sprintf("COMMIT after %d inserts", n)
	default:
		boundary = "none"
	}
	env.Drew(t, "%s, %d inserts, fault at %s", kind, n, boundary)

	app, _ := newApp(t, env, kind, dsn, faults)
	// ... run the n inserts in one App.Tx, count what persisted ...

	if boundary != "none" && (!errors.Is(err, faulttest.ErrInjected) || got != 0) {
		env.Mismatch(t, fmt.Sprintf("%s: a transaction failing at %s", kind, boundary),
			"the injected fault and 0 rows", fmt.Sprintf("%v and %d rows", err, got))
	}
}
```

With `App.Tx` changed to ignore its commit error, this scenario produced the
report shown under [Reproducing a chaos failure](#reproducing-a-chaos-failure).

## Rules for contributors

- **No `time.Sleep` orchestration.** Never wait "long enough" for another
  goroutine to reach a point. Hold the call with `Block` or `BlockThenFail`,
  wait on `inj.Reached(n)` or `proxy.Held()`, act, then release. A sleep is
  only acceptable as the fault itself (`Delay`), never as synchronization.
- **Every wait in a test is bounded.** Select on a timeout so a regression
  fails the test instead of hanging it until the 10-minute `go test` limit.
  Release blocked faults before closing a server (`defer release()` after
  `defer srv.Close()`), because `Close` waits for in-flight handlers.
- **No unbounded retries,** in production code or tests. Every retry policy
  has a maximum number of attempts or a deadline, honors its context, has
  bounded backoff, and surfaces its final error. An in-process policy must pass
  `faulttest.CheckRetryPolicy`; job retries, which the queue schedules, are
  covered by the `jobs` package's tests.
- **Fault injection is explicit opt-in.** Faults come from wrappers and
  proxies that a test constructs: `OpenDB`, `NewHTTPDependency`,
  `NewTCPProxy`. There is no global chaos switch, no environment variable
  that changes production behavior, and production code never imports
  `internal/faulttest`. Add a seam (such as a `Sleeper`, an injectable
  `*http.Client`, or a `driver.Connector`) only where a failure path needs it.
- **Assert invariants, not implementation details.** Check returned errors,
  persisted state, released resources, and recovery. Check call counts only
  when the call sequence is the contract.
- **Leak checks use explicit synchronization,** not global goroutine counts:
  `faulttest.Idle` for connections, `dep.WaitIdle` for outbound calls,
  `TrackBodies` for response bodies, injector counters for cancellation. The
  suite does not use `goleak`: a process-wide goroutine snapshot also sees the
  database pool's and the HTTP transport's own goroutines, and needs ignore
  lists that drift.
- **A fault test must pass `go test -race -count=50`** before it lands, and
  a concurrency + fault scenario (`TestFault_Concurrency_*`)
  `-race -count=100`. A flaky failure-path test is worse than none.
- **Chaos scenarios draw only from `env.Rand`,** record their draws with
  `env.Drew`, and report with `env.Mismatch`. Once a chaos failure is
  understood, pin it as a `TestFault_*` test.
- **No credentials, no internet.** Fault tests use loopback servers, the
  in-process proxy, and ephemeral test databases, and never target anything
  created outside the test run.
- **Keep this document true.** A PR that adds, changes, or removes a fault
  test updates the invariant tables here. A guarantee with no enforcing test
  does not belong in them.
