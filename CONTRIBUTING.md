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

Gombit is built issue-by-issue. Issues are titled `[ID] …` (e.g. `[M2-2]
gombit db migrate / rollback / status`); larger features are epics whose issue
lists its children. (The v0.1 backlog came from
[`docs/GOMBIT_BUILD_PLAN.md`](docs/GOMBIT_BUILD_PLAN.md) §4, now a historical
record.) Two things follow from that:

- **One issue → one pull request** where practical, and the PR links its issue.
- **Don't start an issue whose "Depends on #N" is still open** — the dependency
  ordering is real.

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

Failure paths get deterministic tests. The full guide is
[docs/testing/fault-injection.md](docs/testing/fault-injection.md). It covers
the invariants (INV-1 to INV-8) and the tests that enforce them, what Gombit
guarantees versus what applications must handle, the `internal/faulttest`
primitives, the naming taxonomy, worked examples, and how to reproduce a
chaos failure from its seed.

```bash
make test-faults    # every TestFault_* test and the faulttest harness, under -race

FAULT_POSTGRES_DSN='postgres://gombit:gombit@127.0.0.1:5432/gombit?sslmode=disable' \
FAULT_MYSQL_DSN='gombit:gombit@tcp(127.0.0.1:3306)/gombit?parseTime=true' \
FAULT_REDIS_ADDR=127.0.0.1:6379 \
  make test-faults  # plus the real databases and Redis, as CI's fault-tests job does

make test-chaos     # the stochastic suite: nightly/on demand only, never a PR check
CHAOS_POSTGRES=0 CHAOS_SEED=<seed> CHAOS_SCENARIO=<name> CHAOS_ITERATION=<i> make test-chaos  # replay: paste the report's line
```

The rules, in short:

- Name fault tests `TestFault_<Component>_<Scenario>`. The prefix is what puts
  them in `make test-faults`, so a new one needs no CI edit.
- Assert invariants (errors, persisted state, released resources, recovery),
  not call counts, unless the call sequence is itself the contract ("`App.Tx`
  runs `fn` once", "one rotation, one INSERT").
- Pin interleavings with the injectors (`Block`, `Reached(n)`,
  `proxy.Held()`), never with `time.Sleep`. Bound every wait in a test.
- No unbounded retries: an in-process retry policy (one that waits through a
  `faulttest.Sleeper`) must pass `faulttest.CheckRetryPolicy`. Job retries,
  which the queue schedules, are bounded by `MaxAttempts` and tested in
  `jobs/policy_test.go` and `jobs/worker_test.go`.
- Fault injection is explicit opt-in, through wrappers and proxies a test
  builds. Production code never imports `internal/faulttest`.
- A fault test must pass `go test -race -count=50` before it lands, and a
  `TestFault_Concurrency_*` scenario `-race -count=100`. A flaky
  failure-path test is worse than none.
- Adding, changing, or removing a fault test means updating the invariant
  tables in the guide.

The `Fault injection` check runs `make test-faults` as six shards on every PR
(`FAULT_SHARD`; each compiles first with `FAULT_COMPILE_ONLY=1`, then runs
under `FAULT_BUDGET_SECONDS=120`). The `Fault soak` workflow
reruns it 100 times weekly, and the check becomes required once that stays
clean. The `Chaos` workflow runs `make test-chaos` nightly, and its artifacts
carry everything needed to replay a failure. `bash scripts/chaos-run.sh`
produces the same artifacts locally.

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

A pull request is not done unless it satisfies the working agreement in
[`AGENTS.md`](AGENTS.md) (carried over from the v0.1 build plan §5). In
short:

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
