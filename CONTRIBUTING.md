# Contributing to Gombit

Thanks for wanting to help. This page covers how to get set up, how to get a
change reviewed, and the bar a change has to clear.

- **Bugs and features** → [open an issue](https://github.com/gombit-dev/gombit/issues/new/choose)
- **Security vulnerabilities** → **not** an issue; see [SECURITY.md](SECURITY.md)
- **Behaviour expectations** → [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md)

## Prerequisites

| Tool | Version | Needed for |
| --- | --- | --- |
| Go | 1.26+ (`go.mod` is authoritative) | everything |
| A C toolchain | gcc/clang, or Xcode CLT on macOS | SQLite (`mattn/go-sqlite3` is cgo-only) |
| Node.js | 22+ | frontend, admin UI, TypeScript client generation |
| Atlas | Community Edition, pinned by CI | `gombit db makemigrations` / `migrate` and the migration tests |
| Docker | any recent | PostgreSQL and MySQL test matrices |

```bash
curl -sSf https://atlasgo.sh | sh -s -- --community
```

That installs whatever `latest` resolves to, which has been a canary build
before now. CI pins Atlas through the `ATLAS_VERSION` variable at the top of
[`.github/workflows/ci.yml`](.github/workflows/ci.yml), and the installer reads
that same variable — so to match CI when reproducing a migration or conformance
failure, export the pinned version first:

```bash
export ATLAS_VERSION=<the version pinned in ci.yml>
curl -sSf https://atlasgo.sh | sh -s -- --community
```

Full setup and troubleshooting: [`docs/installation.md`](docs/installation.md).

## Getting set up

```bash
git clone https://github.com/gombit-dev/gombit.git
cd gombit
go build ./...
go test ./...
```

Inside this repository the CLI runs from source — `go run ./cmd/gombit …`.
The `gombit …` form in the docs is the user-facing one, for an installed
binary.

## How work is organised

Gombit is built issue-by-issue from the backlog in
[`docs/GOMBIT_BUILD_PLAN.md`](docs/GOMBIT_BUILD_PLAN.md) §4. Issues are titled
`[ID] …` (e.g. `[M2-2] gombit db migrate / rollback / status`) and belong to a
milestone. Two things follow from that:

- **One issue → one pull request** where practical, and the PR links its issue.
- **Don't start an issue whose "Depends on #N" is still open** — the dependency
  ordering in §4 is real.

Build plan **§1–§3 record locked architecture decisions** (Huma as the contract
source of truth, Atlas-backed migrations, Cobra for the CLI, the runtime generic
admin, the `{data}` / `{error}` response envelope, the feature-package app
layout). They are settled. A change that reopens one needs to argue against the
[ADR](docs/adr/) that locked it.

If something looks missing from the backlog, **say so in an issue** rather than
adding scope in a PR.

## Making a change

1. Fork, then branch from `main`. Branch names are free-form; `feat/…`,
   `fix/…`, `docs/…` are common here.
2. Write the change **and its tests**. New behaviour without tests is not done.
3. Run the checks below.
4. Open a PR against `main` using
   [the template](.github/pull_request_template.md). Fill in **every** section —
   if an item doesn't apply, mark it N/A with a reason instead of deleting it.
5. Conventional commit prefixes (`feat:`, `fix:`, `docs:`, `chore:`) are
   expected in the title; nothing stricter is enforced.

## Local checks

The baseline, matching CI's `lint → build → test` path:

```bash
go build ./...
go test ./...
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run
```

CLI smoke checks:

```bash
go run ./cmd/gombit --help
go run ./cmd/gombit make --help
go run ./cmd/gombit client check --spec examples/client/openapi.json --out examples/client/frontend/src/api/generated
go run ./cmd/gombit doctor
go run ./cmd/gombit config show
go run ./cmd/gombit routes
go run ./cmd/gombit version
```

`gombit new demo --database sqlite` scaffolds a compiling app — see
[`docs/cli.md`](docs/cli.md).

### The database matrix

DB-touching changes must pass on SQLite, PostgreSQL, **and** MySQL. SQLite runs
by default; the other two are behind the `integration` build tag and need a
DSN flag, so they no-op unless you start a database.

```bash
docker run --rm -d --name gombit-pg -p 5432:5432 \
  -e POSTGRES_USER=gombit -e POSTGRES_PASSWORD=gombit -e POSTGRES_DB=gombit \
  postgres:16-alpine

docker run --rm -d --name gombit-mysql -p 3306:3306 \
  -e MYSQL_ROOT_PASSWORD=root -e MYSQL_DATABASE=gombit \
  -e MYSQL_USER=gombit -e MYSQL_PASSWORD=gombit \
  mysql:8.4
```

```bash
# SQLite
go test ./database ./auth ./admin

# PostgreSQL
go test -tags integration ./database -database.postgres-dsn \
  'postgres://gombit:gombit@127.0.0.1:5432/gombit?sslmode=disable'
go test -tags integration ./auth -auth.postgres-dsn \
  'postgres://gombit:gombit@127.0.0.1:5432/gombit?sslmode=disable'
go test -tags integration ./admin -admin.postgres-dsn \
  'postgres://gombit:gombit@127.0.0.1:5432/gombit?sslmode=disable'

# MySQL
go test -tags integration ./database -database.mysql-dsn \
  'gombit:gombit@tcp(127.0.0.1:3306)/gombit?parseTime=true'
```

Migrations and the conformance suite follow the same shape — see
[`ci.yml`](.github/workflows/ci.yml) for the exact invocations, including
`-conformance.driver` and the `ATLAS_BINARY` environment variable.

### Fault injection

Failure paths get deterministic tests with `internal/faulttest`: an
`Injector` fails exactly the calls a test names (`FailOnce`, `FailNTimes`,
`FailOnCall(n, err)`, `Sequence(...)`), or holds them (`Delay`, `BlockUntil`)
until a timer, a channel, or the call's context ends — never on a guessed
sleep. `faulttest.OpenDB` wraps a real driver so a test can fail the Nth
statement, a `Begin`, a `Commit`, or a `Rollback` on SQLite, PostgreSQL, and
MySQL alike:

```go
db, _ := faulttest.OpenDB(database.DriverSQLite, dsn, &faulttest.DBFaults{
	Commit: faulttest.FailOnce(faulttest.ErrInjected),
})
// the first transaction's commit fails: assert nothing it wrote persists
```

Set up over the same wrapped database with the injectors disarmed
(`inj.Disarm()`), and `Arm()` them for the calls under test, so migrations
and fixtures are never numbered. Name fault tests by the epic's taxonomy
(`TestFault_Database_CommitFailure`) and assert persisted state and returned
errors, not call counts. The `framework`, `auth`, and `admin` fault tests run
on SQLite by default and add PostgreSQL and MySQL under the `integration` tag
with that package's DSN flags. `auth` and `admin` both create and drop the
auth tables, so against one shared database run the packages one at a time
(`go test -p 1 ...`, or one package per command as CI does). After a failed
write, `faulttest.Idle` checks that no transaction was left holding a
connection; row counts alone cannot see an open transaction.

Outbound HTTP gets a loopback dependency, `faulttest.NewHTTPDependency(t,
steps...)`, that answers each call with the next scripted step:
`ServerError()`, `TooManyRequests(retryAfter)`, `Respond(status, body)` (a
malformed body, say), `Hang(release)` (slower than any deadline, released by
a channel or the client going away), `CutBody(partial)`, or
`ResetConnection()`. Point the code under test at `dep.URL()` through
`faulttest.TrackBodies(dep.Client().Transport)` to assert every response
body was closed, and call `dep.WaitIdle(t)` to prove no call was left
running on the dependency.

Real transport faults go through `faulttest.NewTCPProxy(t, upstream)`, an
in-process TCP proxy the test points its dependency at (for Postgres,
`faulttest.PostgresDSNVia(dsn, proxy.Addr())`). The test toggles each fault
at an explicit point: `Hold()` (the dependency stops answering: latency past
any deadline; `Held()` says a call has stalled), `Cut()` / `Reset()` (open
connections drop, with FIN or RST), `Refuse()` (new connections are
refused), and `Heal()` (all lifted, so the next operation must succeed
without a restart). The `framework` and `cache` network tests
(`TestFault_Network_*`) run against a real Postgres and Redis this way.

Cancellation and deadline tests (`TestFault_Context_*`) check for leaks by
explicit synchronization, not by counting goroutines (the suite does not use
`goleak`: a process-wide goroutine snapshot also sees the database pool's and
HTTP transport's own goroutines, and needs ignore-lists that drift). The
downstream call is a fault whose own counters must show the cancellation,
`faulttest.Idle` must find no connection held, `dep.WaitIdle` no outbound
call running, and each handler signals when it returns, under a bounded
wait. Release a blocked fault before closing a server (`defer release()`
after `defer srv.Close()`), so a regression fails the test instead of hanging
the server's `Close`.

Concurrency + fault scenarios (`TestFault_Concurrency_*`) pin their
interleaving with the injectors, never with sleeps: hold a call at a
dangerous boundary (`Block`, or `BlockThenFail(release, err)` for a call
that stalls and then fails, such as a COMMIT holding its row lock), wait on
`Reached(n)`, start the competing operation, then release. `SequenceThen(rest,
steps...)` scripts the first calls and sets what every later one does. Each
scenario states its expected outcome (one winner, rollback, or a terminal
error for everyone) and must pass `-race -count=100`.

Any retry policy Gombit adds must pass `faulttest.CheckRetryPolicy(t,
faulttest.RetryContract{...})` (INV-4, bounded retry). The policy waits
between attempts through a `faulttest.Sleeper` (in production a
`faulttest.RealSleeper`-like timer, in tests a `FakeSleeper` that records the
delays and returns at once, or blocks until canceled), so its backoff is
tested without sleeping. The checker drives the policy with scripted
failures and reports each violated property. `MaxAttempts` is the policy's
budget, so an op that keeps failing retryably must run exactly that many
times. The checker reports:

- more attempts than `MaxAttempts`, or fewer (a retryable failure not
  retried while budget remains);
- an attempt started on a context that had already ended (`Do` must return
  the context's error without calling the op);
- a cancellation mid-backoff that does not end it;
- a permanent error retried;
- a backoff off its schedule;
- a `Delay` outside `[0, MaxDelay]` at any sampled attempt. The sample is
  every attempt up to `max(MaxAttempts, 128)`, plus 1000. That catches a
  shift or doubling that overflows (it wraps by attempt 64). It does not
  catch an attempt multiplied by a unit, which wraps at an attempt that
  depends on the unit, so compute a delay so it cannot overflow;
- a success that does not end it;
- a final error that hides the last attempt's error.

`make test-chaos` is the other half: the stochastic suite in
`internal/chaos` (behind the `chaos` build tag, never in PR CI). Each
scenario draws its fault, boundary, and sizes from a random source derived
from one seed, which every run prints first. A failure prints a `CHAOS
FAILURE` block with:

- the seed, scenario, and iteration;
- what the scenario drew;
- expected and observed;
- the replay command (`CHAOS_POSTGRES=0|1 CHAOS_SEED=... CHAOS_SCENARIO=...
  CHAOS_ITERATION=... make test-chaos`).

`CHAOS_POSTGRES=1` refuses to run without `CHAOS_POSTGRES_DSN`, so a replay
cannot silently switch a Postgres failure to SQLite. A selected scenario
that cannot run fails instead of skipping.

A failure that does not reproduce from its seed is a harness bug, not a
flake: find the randomness a scenario drew from somewhere other than
`env.Rand`.

To add a scenario, call `register(Scenario{Name, Component, Run})`. Record
the scenario's draws with `env.Drew`, report violated invariants with
`env.Mismatch`, stop with `env.Fatalf` (not `t.Fatalf`), and hand the
faulttest helpers `env.TB(t)` (`faulttest.Idle(env.TB(t), db)`), so the
report carries every message. Bound every wait, including a query's (a
hang must fail the scenario, not the package timeout). Once a chaos failure is understood, pin it as a
deterministic `TestFault_*` test.

`make test-faults` runs the whole fault suite: the `internal/faulttest`
harness and every `TestFault_*` test (found by name, so a new one joins
without editing anything), under the race detector. Set `FAULT_POSTGRES_DSN`
and `FAULT_MYSQL_DSN` to add those databases, and `FAULT_REDIS_ADDR` for
Redis, as CI's `fault-tests` job does:

```bash
FAULT_POSTGRES_DSN='postgres://gombit:gombit@127.0.0.1:5432/gombit?sslmode=disable' \
FAULT_MYSQL_DSN='gombit:gombit@tcp(127.0.0.1:3306)/gombit?parseTime=true' \
FAULT_REDIS_ADDR=127.0.0.1:6379 \
  make test-faults
```

A fault test must pass `go test -count=50` (and `-race`) before it lands: a
flaky failure-path test is worse than none. `FAULT_COUNT=100 make
test-faults` is the soak the `Fault soak` workflow runs weekly (and on
demand); the `Fault injection` check becomes required only after that soak
stays clean. (`FAULT_COUNT` is a plain base-10 count: `go test` would read
`010` as octal and `00` as "run nothing".)

In CI the suite runs as six shards (`FAULT_SHARD=i/6`, packages spread
round-robin, so a new fault package needs no CI edit). Each shard first
compiles its test binaries (`FAULT_COMPILE_ONLY=1`), then runs under
`FAULT_BUDGET_SECONDS=120`: a shard whose run takes longer fails, and the
remedy is another shard, never a dropped scenario.

### Generator golden tests

Generators are covered by golden trees in `goldentest`. After an **intentional**
generator change:

```bash
go test ./goldentest -update
```

Review the resulting diff before committing — an unreviewed `-update` defeats
the point. Never commit `replace` directives or machine-specific paths into the
goldens.

### Contract drift

Any API change must regenerate the OpenAPI document and the TypeScript client
**in the same PR**:

```bash
go run ./cmd/gombit client check --write --spec examples/client/openapi.json --out examples/client/frontend/src/api/generated
```

CI fails if `examples/client/openapi.json` or
`examples/client/frontend/src/api/generated` would change.

`gombit client check`'s bare defaults (`openapi.json` /
`frontend/src/api/generated`) target a generated app, not this repository —
outside this repository it also needs `--url` to fetch a live spec, since a
separately compiled `gombit` binary has no Go-level `huma.API` to compare
against. See [`docs/client.md`](docs/client.md).

### Admin UI

```bash
cd internal/adminui && npm ci && npm test
```

## Code review

Before opening or merging a PR, review the diff as an adversarial senior
reviewer against the working agreement and the change's claimed contract.
This repo ships a review skill for it:

- **Claude Code:** run `/code-review` (`.claude/skills/code-review/SKILL.md`)
- **Cursor:** run `/code-review` (`.cursor/skills/code-review/SKILL.md`)

Cursor also ships `/create-feature` and `/bugfix` skills encoding the workflows
above.

## Working agreement

A pull request is not done unless it satisfies the Agent Working Agreement in
[`docs/GOMBIT_BUILD_PLAN.md`](docs/GOMBIT_BUILD_PLAN.md) §5. In short:

- new behavior has tests; DB-touching changes pass the SQLite + PostgreSQL +
  MySQL matrix;
- stable features ship docs and appear in an example app;
- extraction from existing templates preserves contracts — refactor and move,
  don't rewrite code that already passes its tests;
- generators are idempotent, additive, and AST-safe for Go source edits
  (`go/ast` / `go/format`, never regex), and never overwrite user-owned files;
- generated frontend source contains no secrets; `VITE_*` is public;
- API changes regenerate OpenAPI and the TypeScript client in the same PR;
- scope stays inside the issue milestone — no M6 "battery" creep (jobs, events,
  scheduler, mail, storage, gRPC, multi-tenancy, i18n);
- the PR links its issue and states which acceptance criteria it satisfies.

## Releasing

Maintainers only: [`docs/releasing.md`](docs/releasing.md).
