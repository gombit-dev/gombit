# BENCH-1 methodology

This is the full write-up behind the numbers in the root `README.md`
`## Performance` block. Those numbers are generated from
`benchmarks/results/latest/` by `make benchmark-report`; this document explains
what was measured and, just as importantly, what the numbers **do not** mean.

> **Read [How not to interpret these results](#how-not-to-interpret-these-results)
> first.** It is not boilerplate — the topology below makes several intuitive
> readings wrong.

## What is measured

One **canonical application** — the same `/api/projects` CRUD API
([schema](../docs/schema.md)) — is implemented six times and measured
identically:

| Stack | Framework | Runtime |
| --- | --- | --- |
| Go control | Gin + GORM | Go |
| Gombit | Gombit (Huma + GORM + Atlas) | Go |
| Python | Django + DRF | CPython |
| Ruby | Rails + ActiveRecord | Ruby |
| PHP | Laravel + Eloquent (nginx + PHP-FPM) | PHP |
| Node | NestJS + TypeORM | Node |

The headline comparison the suite exists for is **Gin+GORM vs Gombit**: same
language, runtime, and ORM family, so the delta isolates Gombit's incremental
cost. The four ecosystem apps are *context*, not a language leaderboard (see
below).

Two families of measurement:

1. **Throughput / latency** — `GET /api/projects?page=1&limit=20` under a
   closed-loop `constant-vus` load. The **canonical protocol** (the one a
   publishable, dedicated-host run uses) is the concurrency sweep
   **1/10/100/500/1000, 5 trials × 30 s each, 10 s discarded warm-up**, pinned
   in `benchmarks/config/versions.env`. `summary.md` carries every concurrency
   level; the README's headline table reports a **single concurrency (100
   clients)** with median requests/sec **and** median p50/p95/p99 latency, and a
   ⚠ flag on any group whose trials disagree by more than 5 % (coefficient of
   variation). `make benchmark-crud-all`.
   The abstraction-cost microbenchmark — net/http → Gin → Huma → Gombit,
   `make benchmark-micro` — is the first README table: each stack runs in its own
   `go test` process (a `framework.App` constructor mutates a process global),
   `-count` samples are all persisted to `microbench.json`, and the README
   publishes the **validated typed POST** scenario (§13 A's representative typed
   path, not the Hello-World GET) as median ns/op / B/op / allocs/op with a
   column relative to net/http. The other four scenarios (plaintext, json,
   path-param, invalid-post) live in `microbench.json`.
2. **Operational footprint** — container-start → first `/livez` 200 cold start
   (≥20 restarts, median/p95), idle memory, memory + CPU under the same load,
   and image size. `make benchmark-footprint`.

### Cross-core scaling (parallel microbench)

`BenchmarkFrameworkTaxParallel` (issue #243) drives the plaintext and
valid-POST scenarios through the same four in-process handlers as the
abstraction-cost microbench, but concurrently via `b.RunParallel`. Its purpose
is orthogonal to the single-goroutine `BenchmarkFrameworkTax`: it exposes
**per-request serialization** — a shared lock on the hot path — that a
single-goroutine benchmark cannot see. Run one row across core counts and read
whether its per-op time falls with more cores:

```
go test ./benchmarks/micro/... -bench=BenchmarkFrameworkTaxParallel -benchmem -cpu=1,2,4,8,16
```

A row whose ns/op drops roughly in proportion to `-cpu` scales; a row whose
ns/op flattens as cores are added is contention-bound (every request is
funnelling through a shared lock). This is the measurement that surfaced the
metrics-middleware mutex in #239 — invisible to the single-goroutine numbers,
obvious here. It is a **diagnostic**, not part of the published README snapshot:
the numbers are host- and scheduler-sensitive, so read them as a scaling *shape*
on your own machine, not as an absolute to compare across hosts.

### Canonical protocol vs. a particular snapshot

The sweep above is the **canonical protocol**: the parameters a run must use to
be published as *the* benchmark. Any individual run may narrow it — a laptop
smoke, a reduced development snapshot — by overriding the pins on the command
line (e.g. `make benchmark-crud-all CONCURRENCY=1,10,100 TRIALS=3 DURATION_SECONDS=10`).

A run therefore records *its own* parameters, not the canonical ones, in
`metadata.json` (`concurrency`, `trials`, `duration_seconds`, `warmup_seconds`),
and the generated README prints them verbatim under **How these were measured**.
When they differ from the canonical protocol, `make benchmark-report` stamps the
README block **“Reduced development snapshot”**; when the tree is dirty it stamps
it **“UNPUBLISHABLE DEVELOPMENT RUN”**. So the numbers in the README are always
the numbers of *that snapshot*, never assumed to be the canonical 5 × 30 s,
1→1000 sweep. Read the metadata block, not this paragraph, for what a given
snapshot actually ran.

## Fairness controls (issue #141 §7/§16/§18)

- **Identical schema and seed.** All six use the same `users`/`projects` tables
  and the same deterministic seed (1,000 users, 100,000 projects, round-robin
  ownership), so every app answers the same query against the same rows.
- **Fixed resource limits.** Each app container is pinned to **2 vCPU / 1 GiB**
  and PostgreSQL to **2 vCPU / 2 GiB** (compose `deploy.resources.limits`).
  Because a compose file only *requests* limits, the suite reads the *applied*
  cgroup limit off each live container (`inspect-limits`) and records that honest
  classification (`enforced` / `partial` / `not applied`) — it never assumes the
  ceiling held. Each app's verdict is kept **per framework** in
  `metadata.resource_limits_by_framework`, and the shared database container's in
  `metadata.postgres_resource_limits`, so a `partial` on one app is never
  overwritten by an `enforced` on the next when the six runs merge into one
  snapshot. (The scalar `metadata.resource_limits` remains the standalone
  `make benchmark-crud` path's single intended-budget string.)
- **Connection pooling** capped at 20 total per app (pre-fork servers divide it
  across workers so the *total* matches).
- **Production configuration.** gunicorn / Puma (clustered) / nginx+PHP-FPM /
  compiled Node — never a framework dev server. Per-request access logging is
  off.
- **Pinned versions.** Every language, framework, database, and the load
  generator are pinned exactly (`benchmarks/config/versions.env`, each app's
  manifest); the recorded framework/runtime versions are derived from those
  sources, not hand-copied.

## Topology

The load generator (k6) runs in a container **on the same host** as the app —
the issue's "another container on the same host". It is not a separate
load-generation machine. At high concurrency k6 therefore contends with the app
for CPU; that is the recorded topology, not an error.

The load is **closed-loop** (`constant-vus`: N clients, each sending the next
request only after the previous response). This matches the issue's
concurrency-as-client-count axis, but it means the numbers are subject to
**coordinated omission**: when the app slows, a client simply waits before its
next request, so reported tail latency *understates* true client-observed wait.

## Reproducing

```sh
make benchmark-crud-all      # throughput -> results.json
make benchmark-footprint     # footprint  -> footprint.json
make benchmark-summary       # results.json -> summary.md
make benchmark-report        # README ## Performance block (+ summary.md)
```

The `metadata.json` beside the results records the host (CPU, cores, RAM),
commit + dirty state, OS/kernel/arch, pinned versions, the applied resource
limits (per app and for Postgres), and this run's own protocol parameters, so a
published table always carries the conditions it was produced under — and the
report labels it a reduced or unpublishable run when those conditions fall short
of the canonical protocol or a clean tree.

### Provenance is recorded per measurement group

`results/latest/` looks like one artifact but holds three independently produced
groups, and they cost wildly different amounts to produce:

| group | target | cost |
| --- | --- | --- |
| `microbench` (framework tax) | `make benchmark-micro` | ~40s, pure Go, no Docker |
| `crud` (PostgreSQL CRUD read) | `make benchmark-crud-all` | hours: 6 containers × the full sweep |
| `footprint` (operational footprint) | `make benchmark-footprint` | minutes, Docker |

Each target stamps its own `git_commit`, timestamp, host and Go toolchain under
`metadata.json`'s `groups` key, and the report captions each table with that
group's provenance. A single snapshot-wide commit could not describe all three
without misattributing at least two: a cheap microbenchmark refresh would either
have to claim the hours-long CRUD sweep ran at the new commit, or publish its own
fresh numbers under the old one.

The Go toolchain is part of a group's provenance because it moves the numbers on
its own. The framework-tax ladder's `net/http` rung is stdlib plus the harness —
no Gombit code can affect it — and it has still shifted by 3 allocs/op between
toolchain releases with no source change. **Never splice a refreshed row onto
stale baseline rows:** the four rungs are only comparable when they were produced
by one run, on one host, under one toolchain.

A snapshot with no `groups` key predates this and is read with its top-level
block as every group's provenance, which for a single-run snapshot is exact.

#### Reading `metadata.json`: `groups` is authoritative, the top level is not

`groups.<name>` is the answer to "when, where and at which commit was *this
table* measured". The flat top-level fields (`git_commit`, `timestamp`,
`cpu_model`, `go_version`, …) describe the last **whole-snapshot** collection,
which in practice is the CRUD sweep.

A group-only refresh — `make benchmark-micro`, `make benchmark-footprint` —
stamps its own group and **deliberately leaves the top-level fields alone**. So
after refreshing the microbenchmark you will see a top-level `git_commit` that
is *older* than `groups.microbench.git_commit`. That is correct. Advancing it
would make the CRUD and footprint tables — whose rows did not change — claim a
commit and a host they never ran on, which is the same misattribution in the
opposite direction.

The practical rule: **read `groups.<name>` for a table's provenance; read the
top level only for the shared run parameters** (database, resource limits, sweep
protocol, load generator) and as the fallback for pre-`groups` snapshots.

The shape is additive, so `schema_version` stays `1`: a reader that predates
`groups` still sees exactly the flat fields it always saw.

#### Known limitation: the `crud` group is per-sweep, not per-app

The CRUD table is six independently produced app runs, but they share one
`groups.crud` entry: `run-crud` stamps the group once per app and the last
writer wins. Re-run a single app after a full sweep — `make benchmark-crud-all
APPS=gombit` — and the table is captioned with that re-run's commit while the
other five rows still come from the earlier sweep.

This is the same misattribution described above, one level finer, and it is not
new: the top-level block behaved this way before per-group provenance existed.
It is left as-is deliberately, because the provenance granularity matches the
report's granularity — one caption per table. Refreshing a single app and then
publishing the table is therefore **not** a supported operation the way
refreshing the whole microbenchmark group is; re-run the whole sweep.

## How not to interpret these results

- **This is not a language or framework leaderboard.** The apps differ in
  language, runtime, ORM, and process model all at once. "Framework X does N
  rps" tells you about *this app, this query, this host, this day* — nothing
  about X in general. The only apples-to-apples comparison here is Gin+GORM vs
  Gombit (same everything but the framework); treat the rest as ecosystem
  context.
- **Tail latency is optimistic.** Closed-loop load hides queueing via
  coordinated omission (above). Do not read p95/p99 here as a service-level
  objective.
- **Same-host contention is included, not controlled out.** At 500/1000 VUs the
  load generator competes with the app for the same cores. High-concurrency
  rows measure "app + k6 sharing 2 vCPU", which is deliberately conservative but
  not the same as "app alone on 2 vCPU".
- **It is one machine per table, and the tables are separate runs.** Each table
  is a single snapshot on one host; the committed `metadata.json` names that host
  per measurement group and the README caption repeats it under each table.
  Numbers from different hardware — including from a *different table here* — are
  not comparable. Re-run on your own hardware before drawing operational
  conclusions.
- **Memory is the container working set**, from `docker stats` (cgroup usage
  minus reclaimable cache) — a deployment-footprint proxy for RSS, not a precise
  heap measurement.
- **Cold start is container-start → ready**, not process fork time or
  serverless cold start; it excludes image pull (the image is pre-pulled).
- **Higher rps is not "better" in isolation.** Read it next to the footprint
  table: a stack's throughput, its memory, and its cold start are the same
  trade-off surface.

If a number here is going to influence a decision, reproduce it on your target
hardware with your workload first.
