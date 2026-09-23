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

### Per-layer ablation (issue #265 / PERF-7)

The framework-tax matrix reports four rows (net/http → Gin → Huma → Gombit) but
nothing in between, so a regression or a win *inside* the Gombit runtime cannot
be attributed to a layer. `BenchmarkAblation` (in
[`framework/ablation_bench_test.go`](../../framework/ablation_bench_test.go),
in-package because the middleware constructors are unexported) fills that gap.
It builds the runtime stack one middleware at a time and reports a ladder of
stacks under `gombit-ablation/<row>` in `microbench.json`:

```
baseline → recovery → request_context → metrics → security_headers → xss → request_timeout → full-app
```

- **`baseline`** is bare Huma+Gin with **no** Gombit middleware, built with
  `contract.HumaConfigFor` — deliberately *not* `huma.DefaultConfig` (the
  `benchmarks/micro/huma` matrix row uses that, and it keeps a schema-link
  transformer Gombit disables, costing ≈3 allocs Gombit does not). Using the
  same Huma config as the layers makes `recovery`'s delta a like-for-like
  measurement rather than a mix of middleware cost and config difference.
- Each middle row adds exactly one layer of `runtimeMiddlewareStack`, in install
  order, for the production config (so CSRF — cookie-mode only — is absent).
- **`full-app`** is a real `framework.App`. `TestAblationFullAppMatchesRuntimeStack`
  asserts its allocs/op equals the last layer's, so the reconstructed ladder is
  proven to match the genuine application and a middleware added to
  `runtimeMiddlewareStack` cannot silently escape measurement.

**Read a layer's cost as the delta between its row and the row immediately
above it.** The rows are cumulative, so the allocs/op the ablation attributes to
`security_headers`, for example, is `gombit-ablation/security_headers` minus
`gombit-ablation/metrics` (the row before it). The total framework tax over bare
Huma+Gin is `full-app` minus `baseline`.

```sh
make benchmark-micro-ablation   # persists the gombit-ablation/* stacks
# or, without persisting:
go test ./framework -run='^$' -bench='^BenchmarkAblation$' -benchmem -count=10
```

Two harness facts are load-bearing when reading these deltas:

- **`httptest.NewRecorder` contributes 3 allocs/op to every row.** It is a
  constant, so it cancels out of any delta between two rows — never attribute it
  to a layer. It does inflate each row's absolute allocs/op above what a real
  network server would show.
- **`Header.Clone` is *not* a harness artifact.** `net/http` performs the same
  clone on `WriteHeader`, so a layer that pushes the response past a header-count
  threshold (Go's 8-slot swiss-map group) pays real map-growth allocations that a
  production server pays too. Header-count effects seen in the ladder are genuine
  framework cost, not measurement noise. (This is exactly the effect PERF-9 /
  #267 tuned in `security_headers`.)

`BenchmarkAblationSolo` is the parallel-scaling variant of the same ladder (see
Cross-core scaling above); its rows are not persisted.

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
| `microbench` (framework tax) | `make benchmark-micro` | minutes, pure Go, no Docker |
| `crud` (PostgreSQL CRUD read) | `make benchmark-crud-all` | hours: 6 containers × the full sweep |
| `footprint` (operational footprint) | `make benchmark-footprint` | minutes, Docker |

Provenance is recorded per **unit**, not per group. A unit is the merge key of
that group's data file — the thing a single run can replace on its own:

| group | unit | recorded by |
| --- | --- | --- |
| `microbench` | stack namespace (`nethttp`/`gin`/`huma`/`gombit`, `gombit-ablation`) | `scripts/microbench` |
| `crud` | framework + benchmark (workload) | `scripts/run-crud` |
| `footprint` | framework + variant | `scripts/footprint` |

**The program that writes the row writes the provenance.** All three of these
files merge row-wise, and subset runs are supported (`APPS="gin-gorm gombit"`),
so a group-wide stamp would let a run that replaced ONE row relabel a whole
table: six footprint rows measured at commit A, then
`APPS=gombit make benchmark-footprint` at B, would caption five untouched rows
as B. Keying provenance to the same unit the data merges on makes that state
unrepresentable rather than merely detectable.

The report derives each table's caption from the units that table actually
publishes. When they agree it collapses to one line; when they disagree it
refuses to make a table-wide claim and states each unit instead — the same shape
the per-app resource-limit verdicts use, for the same reason.

A single snapshot-wide commit could not describe the three groups either: a cheap
microbenchmark refresh would have to claim the hours-long CRUD sweep ran at the
new commit, or publish its own fresh numbers under the old one.

The Go toolchain is part of a group's provenance because it moves the numbers on
its own. The framework-tax ladder's `net/http` rung is stdlib plus the harness —
no Gombit code can affect it — and it has still shifted by 3 allocs/op between
toolchain releases with no source change. **Never splice a refreshed row onto
stale baseline rows:** the four rungs are only comparable when they were produced
by one run, on one host, under one toolchain.

A snapshot that records no unit at all predates this and is read with its
top-level block as every unit's provenance, which for a single-run snapshot is
exact — such a snapshot still renders one caption per table, exactly as before.
That fallback is **all or nothing**. Once any unit is recorded, a unit without an
entry is captioned as *unrecorded*, never with the top-level block: that block is
rewritten by producers that did not measure the unit (next section), so borrowing
it would re-caption untouched rows with someone else's commit and host. `make
benchmark-metadata` measures nothing, so on a snapshot that records no unit it
keeps the existing top-level block rather than replacing the only provenance those
rows have.

The footprint unit is the row's full merge key, `framework:variant`
(`gombit:container`, `gombit:embedded`), because that is what `footprint.Merge`
keys on. Measuring the embedded single binary therefore cannot relabel the
container row the README publishes.

The CRUD unit is likewise the row's full merge key, `framework:benchmark`
(`gombit:crud-list`), where the benchmark is the workload script that ran
(`workloads/<benchmark>.js`). Recording another workload for an app therefore
neither replaces nor relabels its `crud-list` rows.

Provenance is per unit; the sweep protocol and load generator are not. The
top-level `concurrency`, `trials`, `duration_seconds`, `warmup_seconds` and
`benchmark_tool` are recorded once for the whole snapshot, and the README prints
one "Protocol" line and one reduced-snapshot banner from them. They are only
true while every row in the file was measured under them, so `run-crud` refuses
a run whose values differ from the recorded ones while rows it does not replace
remain (another app's, or another workload's), and modifies nothing. A
populated snapshot cannot be moved to a new protocol one app at a time, because
each app's run is refused while the others' rows remain: write to a separate
`OUT_DIR`, or remove the old snapshot and start over. A fresh `OUT_DIR`, a
snapshot that records no parameters, and a run that replaces every row in the
file are unaffected. `make benchmark-metadata` also writes these fields and is
not guarded. Recording a protocol per unit, so that workloads with different
pins can share a snapshot, is a separate change.

Because the incoming values are written whole, `run-crud` compares every one of
them as a value, never as "unstated": it rejects `-trials` below 1, an empty or
non-positive concurrency level, a duration that is not positive, a negative
warm-up and an empty `-k6-image` before measuring anything. A zero-second
warm-up is accepted and recorded as a value, so it matches a snapshot recorded
with no warm-up and conflicts with one recorded with a warm-up. On the recorded
side, a snapshot that states none of the five fields has no protocol to
misdescribe; one that states any of them is compared on all five.

The producers do not lock `OUT_DIR`. `run-crud` checks the snapshot before the
sweep and again on the snapshot it merges into, which catches another producer
that finished while it was measuring, but a write that overlaps its own
read-merge-write is not detected. Running two producers against one `OUT_DIR`
at the same time is unsupported.

#### Reading `metadata.json`: `groups` is authoritative, the top level is not

`groups.<group>.<unit>` is the answer to "when, where and at which commit was
*this row* measured". The flat top-level fields (`git_commit`, `timestamp`,
`cpu_model`, `go_version`, …) describe whichever collection last rewrote the
whole record, and they are **not a caption for any table**:

- `make benchmark-micro` and `make benchmark-footprint` stamp only the units they
  measured and leave the top-level fields alone, so afterwards the top-level
  `git_commit` is *older* than those units' entries.
- `make benchmark-crud-all` rewrites the top-level fields on every app it runs —
  including an `APPS=gombit` subset of one app — so afterwards the top-level
  `git_commit` can be *newer* than five of the six CRUD rows, and than every
  footprint and microbench row.
- `make benchmark-metadata` rewrites them with a collection that measured nothing.

Both directions are expected; neither is stale bookkeeping to "fix" by editing the
top level.

The practical rule: **read `groups.<group>.<unit>` for a row's provenance; read
the top level only for the shared run parameters** (database, resource limits, sweep
protocol, load generator), and as every row's provenance only in a snapshot that
records no unit at all.

The shape is additive, so `schema_version` stays `1`: a reader that predates
`groups` still sees exactly the flat fields it always saw.

#### CI warns when the microbench snapshot predates the code (PERF-14 / #293)

`benchmark-report-drift` verifies `README ≡ f(snapshot)`; it does **not** verify
`snapshot ≡ f(main)`. `benchmarks/scripts/microbench-staleness.sh` (run in that
job) closes the gap for the cheap-to-refresh microbench group: when the commit
that last refreshed `benchmarks/results/latest/` is a strict ancestor of the last
commit touching `framework/`, `benchmarks/micro/`, or
`benchmarks/internal/microbench/`, it emits a `::warning::` naming the commits
and the remedy (`make benchmark-micro benchmark-report`). It **never gates** the
build (BENCH-1 §3) — it exits 0 in every case — and it compares commits, never
measured values. It keys the ancestor test on the snapshot's *commit in history*
rather than the recorded `groups.microbench.gombit.git_commit`, because this repo
squash-merges and the recorded provenance commit is usually orphaned from `main`;
that commit is still surfaced in the warning. `crud`/`footprint` are out of scope
— refreshing them is hours of Docker, so a per-PR warning would be noise nobody
can act on.

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
