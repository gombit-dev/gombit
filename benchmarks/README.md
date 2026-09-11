# Gombit benchmarks

Reproducible benchmark suite for BENCH-1 ([issue #141](https://github.com/gombit-dev/gombit/issues/141)).
Full plan: [docs/plans/BENCH-1-benchmark-suite.md](../docs/plans/BENCH-1-benchmark-suite.md).

## What's here today

```text
benchmarks/
├── compose.yml          PostgreSQL + all six app services (gin-gorm, gombit, django, rails, laravel, nestjs; §7 limits)
├── config/
│   └── versions.env     pinned load generator (k6), Postgres image, resource limits, workload defaults
├── docs/
│   ├── schema.md        canonical schema/API/envelope every benchmarks/apps/ implementation targets
│   └── methodology.md   full method + "How not to interpret these results" (linked from the README table)
├── internal/            machine-readable output plumbing (Phase 1)
│   ├── result/           results.json/results.csv schema + encoders (issue §9)
│   ├── metadata/         reproducibility metadata struct + host/toolchain collector
│   ├── k6/               parses a crud-list.js summary into result rows (+ Validate)
│   ├── summary/          aggregates trials into per-group stats + renders summary.md (CoV flag)
│   ├── reslimits/        classifies a container's Docker-recorded limits (HostConfig) vs the §7 budget (honest detection)
│   ├── footprint/        operational-footprint schema (cold-start/RSS/CPU) + encoders
│   ├── microbench/       framework-tax schema + `go test -bench` parser (ns/op, B/op, allocs/op)
│   └── report/           renders the README ## Performance block from results/footprint/micro/metadata + drift check
├── workloads/
│   └── crud-list.js      the headline GET /api/projects read workload (k6)
├── scripts/
│   ├── collect-host-info/ CLI over internal/metadata: writes metadata.json for a run
│   ├── run-crud/          runs crud-list.js (via the pinned k6 image) against one app → results snapshot
│   ├── run-crud-all.sh    orchestrates run-crud over all six containerized apps (make benchmark-crud-all)
│   ├── footprint/         records one footprint row into footprint.{json,csv} (merge by framework,variant)
│   ├── footprint-all.sh   measures cold-start/RSS/CPU for all six containers (make benchmark-footprint)
│   ├── k6load/            drives validated crud-list load for the footprint loaded/CPU sampling (fail-closed)
│   ├── summarize/         results.json -> summary.md (make benchmark-summary)
│   ├── microbench/        merges `go test -bench` output into microbench.json (make benchmark-micro)
│   ├── report/            regenerates the root README ## Performance block; -check for drift (make benchmark-report)
│   └── inspect-limits/    reports whether a live container got the §7 ceiling (uses internal/reslimits)
├── micro/                Go abstraction-overhead microbenchmarks (Phase 2)
│   ├── scenario/          shared resource types, Huma route registration, correctness assertions
│   ├── nethttp/           net/http row (no router, no framework)
│   ├── gin/               idiomatic plain-Gin row
│   ├── huma/              bare Huma-over-Gin row
│   └── gombit/             full framework.App row
├── apps/                 canonical realistic-application CRUD comparison (Phase 3-4)
│   ├── shared/             response-shape + seed-content types common to both Go implementations
│   ├── gin-gorm/            primary framework-tax control — see its own README
│   ├── gombit/              real Gombit app (Huma, Atlas migrations) — see its own README
│   ├── django/              Django + DRF ecosystem-context app (Phase 4) — see its own README
│   ├── rails/               Rails + ActiveRecord ecosystem-context app (Phase 4) — see its own README
│   ├── laravel/             Laravel + Eloquent ecosystem-context app (Phase 4) — see its own README
│   ├── nestjs/              NestJS + TypeORM ecosystem-context app (Phase 4) — see its own README
│   └── fairness_test.go     cross-implementation check: builds and runs both real binaries, compares over HTTP
└── results/             generated output (issue §9); results/README.md documents the layout
```

All six canonical CRUD implementations run end to end under one command: **all
six** (`gin-gorm`, `gombit`, `django`, `rails`, `laravel`, `nestjs`) are
containerized with `compose.yml` services carrying the §7 resource budget —
`gombit` also applies its real Atlas migrations in-container (pinned `atlas`
image), and every migration-bearing app creates its own database idempotently on
every bring-up (no fresh-volume init scripts) — and `make benchmark-crud-all`
brings each up, seeds it, classifies the applied §7 ceiling on the live
container and records it (`internal/reslimits` + `scripts/inspect-limits`, issue
#141's "detect/report rather than silently pretend"), runs the workload, and
merges all six into one `results.json`. `make benchmark-footprint` captures the
operational footprint (cold-start, idle/loaded memory, CPU-under-load) of the
same six containers into `footprint.json`. `make benchmark-report` regenerates
the root README's `## Performance` block (and `summary.md`) from those files, and
`make benchmark-report-check` fails on drift — a CI job (`benchmark-report-drift`)
runs the same regenerate-then-diff on every PR. The full method and the required
"How not to interpret these results" caveats live in
[docs/methodology.md](docs/methodology.md). A committed `results/latest/`
snapshot already backs the README numbers, but from a **reduced run on a single
dev host** (recorded in `metadata.json` and printed in the README block); still
scoped to later phases: the embedded-Gombit single-binary footprint variant, the
other `make benchmark-*` workloads, extending `fairness_test.go` to all six, and
re-running the full canonical sweep on dedicated hardware. The CI integration is
in — the per-PR `benchmark-smoke` gate and the manual `benchmarks.yml` workflow
(both below).

## Resource limits (§7): intention vs. reality

Issue #141 §7 pins each app to 2 vCPU / 1 GiB and PostgreSQL to 2 vCPU / 2 GiB.
Those live in `benchmarks/config/versions.env` and are requested via
`deploy.resources.limits` in `compose.yml` — but a value in a compose file is
an *intention*, not proof the kernel enforced it (older Compose, Swarm mode, or
a host without the needed cgroup controllers can silently drop it; Compose v2
does enforce it on a plain `up`). So the suite never trusts the file: after
bringing a container up, `scripts/inspect-limits` reads the limits Docker
recorded for the container (`docker inspect`'s `HostConfig.NanoCpus`/`Memory` —
which the daemon zeroes or rejects when it can't apply them, an honest signal of
a dropped ceiling) via `internal/reslimits` and classifies them against the
intended budget — `enforced`, `partial`, or `not applied`.

`make benchmark-crud-all` records that classification as
`metadata.resource_limits`: for each app it classifies the live container and
passes the result to `run-crud -resource-limits`, so the recorded value is
whichever of `enforced` / `partial` / `not applied` the container actually
shows — a record of what was applied, not an assumption that the ceiling held.
(If `inspect-limits` cannot classify the container at all — a tool failure — that
app's row is not published rather than recorded blank.) The standalone
`make benchmark-crud` starts and constrains nothing, so it records `run-crud`'s
honest "not applied" default unless you pass
`-resource-limits "$(inspect-limits …)"` yourself.

## Result schema and run metadata

`go run ./benchmarks/scripts/collect-host-info` prints the reproducibility
metadata (git SHA + dirty state, OS/kernel/arch, CPU/RAM, Go/Docker/Compose
versions, plus the run parameters passed as flags) as `metadata.json`. The
canonical results shape lives in
[`benchmarks/internal/result`](internal/result) (JSON + CSV encoders); the
Markdown report is always generated from that, never the other way around.
Pinned versions and limits are in
[`benchmarks/config/versions.env`](config/versions.env).

## Running the whole suite

One command runs everything into `benchmarks/results/latest/` and regenerates
the README `## Performance` block — the dedicated-host snapshot run:

```sh
# fresh Postgres for clean per-app databases:
docker compose --env-file benchmarks/config/versions.env -f benchmarks/compose.yml down -v
docker compose --env-file benchmarks/config/versions.env -f benchmarks/compose.yml up -d postgres

make benchmark                       # crud-all -> footprint -> micro -> report
# CRUD pins from versions.env by default (1/10/100/500/1000 × 5 × 30s, 10s
# warm-up); cold starts default to footprint-all.sh's COLD_START_RUNS (20).
# Narrow any on the command line for a reduced run:
make benchmark CONCURRENCY=1,10,100  # e.g. if 500/1000 VUs are unsustained
```

`make benchmark` is an explicit command, never the default goal — a bare `make`
prints the target list (`make help`) so it can never accidentally kick off the
multi-hour six-app run and rewrite the committed README. The stages compose as
sequential recursive `$(MAKE)` lines (not prerequisites), so they stay ordered
even under `make -j` and a failed stage halts the chain;
`benchmarks/scripts/benchmark-target_test.sh` locks that (order, pin
propagation, fail-closed) without Docker by stubbing the recursive make.

The individual `make benchmark-crud-all`, `-footprint`, `-micro`, and `-report`
targets below run each stage on its own.

### Per-PR smoke

`make benchmark-smoke` is the fast correctness guard CI runs on every PR (the
`benchmark-smoke` job in `.github/workflows/ci.yml`, `needs: test`):

```sh
make benchmark-smoke
```

It builds **all six** app images (`docker compose build` — a broken Dockerfile
fails here), then runs the containerized harness end to end (compose up →
migrate → seed → k6 → parse) for **all six** apps with a **small deterministic
seed** (20 users / 100 projects, enough for the workload's full 20-row first
page) and a 1-VU × 1 short trial, into a throwaway `mktemp` dir. This is the
issue #141 §11 smoke: it detects broken builds, broken endpoints, schema/
migration drift, orchestration failures, and result-parser breakage. The numbers
are discarded and `results/latest` is never touched — only pass/fail matters (no
perf gate on noisy shared runners). The small seed is what keeps all six
affordable per PR: every app reads `BENCH_SEED_USERS` / `BENCH_SEED_PROJECTS`
with the same semantics (`benchmarks/apps/shared.SeedCounts` and its per-language
ports; unset → the canonical 1,000/100,000). `benchmark-target_test.sh` locks the
target's contract (builds all six, runs all six with the tiny params + small
seed, throwaway `OUT_DIR`) without Docker by stubbing the recursive make and
`docker`.

### Manual runs (`benchmarks.yml`)

For an on-demand full or selected run, dispatch the **Benchmarks (manual)**
workflow (`.github/workflows/benchmarks.yml`, `workflow_dispatch` only). Pick a
`suite` — `full` (crud → footprint → micro → **summary**), `crud`, `footprint`,
or `micro` — and optionally override `concurrency` / `trials` /
`duration_seconds` / `warmup_seconds` (blank = a **reduced, runner-survivable**
default of `1,10,100 × 3 × 10s`, *not* the `versions.env` canonical
1/10/100/500/1000 × 5 × 30s sweep — set the pins explicitly for a wider run on a
dedicated runner). It writes to `benchmarks/results/dispatch/` (never
`results/latest`) and uploads `results.{json,csv}`, `summary.md`,
`metadata.json`, `footprint.{json,csv}`, `microbench.json`, and the raw
per-trial summaries as a run artifact.

It **cannot** touch the published README or the committed snapshot by
construction: `full` runs the three measurement targets plus `summary` — never
`make benchmark`, whose last stage (`benchmark-report`) rewrites the repo-root
README `## Performance` block from `OUT_DIR` — and every suite writes to the
dispatch dir. Regenerating the committed block stays the reviewed, manual local
step (`make benchmark`, then commit). Locally the same mapping is
`benchmarks/scripts/run-suite.sh` (`SUITE=crud CONCURRENCY=1,10 bash
benchmarks/scripts/run-suite.sh`), covered by `run-suite_test.sh` (which runs in
CI — the `benchmark-scripts` job — alongside the other benchmark script tests).
Numbers from GitHub-hosted runners are noisy — for a citable canonical run, use a
dedicated host.

## Running the CRUD-read workload

The headline workload is `GET /api/projects?page=1&limit=20`
([`workloads/crud-list.js`](workloads/crud-list.js)), driven by the pinned k6
image. **Load model and topology (read before citing any number):** the
workload uses a closed-loop `constant-vus` executor — `VUS` concurrent clients
for the measured window — because the issue's concurrency sweep
(1/10/100/500/1000) is a client-count axis. Closed-loop load is subject to
**coordinated omission**: when the app slows, a client waits before its next
request, so the reported tail latency understates true client-observed wait.
This is the issue's "constant-rate *or* document the limitation" path, taken
by documenting it. `gracefulStop` is `0s`, so the measured window is exactly
the requested duration and `duration_seconds` records the *actual* elapsed run
time (from k6's state), not the flag. The k6 container runs on the host
network on the **same machine** as the app (the issue's "another container on
the same host"), so at high concurrency k6 contends for CPU with the app —
this is the recorded topology, not a separate load-generation host.

**All six under compose (the usual way):**

```sh
make benchmark-crud-all      # then: make benchmark-summary
```

This brings each containerized app up under compose with the §7 limits, applies
its migrations and seed (each app creates its own database idempotently), waits
for `/livez` to report healthy, reads the *actually applied* limit off the live
container (`inspect-limits`, recorded as `metadata.resource_limits`), runs the
workload against it, then **stops** it before the next — the load generator
shares the host, so only the app under test runs while it is measured. Each
app's framework/runtime versions are derived from its own source-of-truth
(manifest file, Dockerfile base image), never hand-copied. Override the set with
`APPS="gin-gorm gombit"`. `benchmarks/scripts/run-crud-all.sh` is the
orchestrator; `run-crud-all_test.sh` checks every app's identity resolves.

**Or against one already-running, already-seeded implementation** (what the loop
calls per app):

```sh
# start + seed the app per its own README, e.g. benchmarks/apps/gin-gorm, then:
make benchmark-crud FRAMEWORK=gin-gorm FRAMEWORK_VERSION=v1.12.0 \
  RUNTIME=go RUNTIME_VERSION=go1.25.7 \
  TARGET_URL='http://127.0.0.1:8081/api/projects?page=1&limit=20'
```

It warms up (a discarded run), measures `TRIALS` times at each concurrency
level in `benchmarks/config/versions.env`, and **merges** its rows into
`benchmarks/results/latest/{results.json,results.csv,metadata.json}` (raw k6
summaries under `raw/`) — re-running one framework replaces that framework's
rows while other frameworks are kept, so running each in turn accumulates all
six. A trial that sends no traffic, has any HTTP error, or fails a content
check (a 200 with the wrong page shape) fails the command loudly **with
nothing written** rather than recording a bogus row — the read workload
against a healthy app must be error-free (`benchmarks/internal/k6`'s
`Summary.Validate`). Standalone, `run-crud` does not start or resource-constrain
the app, so its default `metadata.resource_limits` says so honestly; under
`benchmark-crud-all` the app runs in its §7-limited container and the loop passes
the live `inspect-limits` verdict, so the recorded value is that container's
classification (`enforced` / `partial` / `not applied`), not an assumption that
the ceiling held. Per-app memory/CPU footprint is captured separately by
`make benchmark-footprint` (below).

Once `results.json` exists, `make benchmark-summary` regenerates `summary.md`
from it — one table per benchmark, one row per (framework, concurrency). The
headline throughput and latency numbers are the **median** across trials (issue
§7's "report at minimum the median result", robust to one noisy trial); each row
also carries the throughput coefficient of variation (stddev/mean) and a ⚠ flag
on any group whose trials vary by more than 5% (issue §7). The Markdown is
generated from the structured rows, never hand-edited, and leads with the
coordinated-omission / same-host caveats so a copied table can't be read as
more than it is.

## Running the operational footprint

```sh
make benchmark-footprint      # all six; writes footprint.json/.csv
```

For each of the six containerized apps this brings the service up under its §7
budget, seeds it, and records into `benchmarks/results/latest/footprint.{json,csv}`
(a schema separate from throughput — `benchmarks/internal/footprint`):

- **cold-start** — container-start → first `200` on `/livez`, repeated
  `COLD_START_RUNS` times (default 20, the issue's "≥20 runs"), reported as
  median and p95;
- **idle memory** — the container's cgroup working set (`docker stats`) after an
  `IDLE_SETTLE`-second settle (default 10, the issue's suggestion), recorded as
  `idle_rss_bytes`;
- **loaded memory + CPU** — the **steady-state median** working set and CPU,
  sampled once per `docker stats` tick (~1s) *while the crud-list workload drives
  the app*, dropping the first (ramp) sample. Fail-closed on two fronts: the load
  is run through `benchmarks/scripts/k6load`, which keeps and validates the k6
  summary (`benchmarks/internal/k6`'s `Summary.Validate`) — no traffic, any HTTP
  error, or a failed content check rejects the load; and the aggregator refuses
  to publish unless at least two in-load samples remain, so a missing/one-sample
  series can never become a `0`-byte "loaded" reading. Either failure means **no
  loaded/CPU row is published** — the same rule as `run-crud`. The k6 image is
  pre-pulled and `k6load` pre-built so neither a pull nor a Go compile lands
  inside the measured window;
- **image size** — the container image's on-disk size.

Reduce the cost for a smoke with `COLD_START_RUNS=3 LOAD_SECONDS=4 IDLE_SETTLE=1`,
and narrow the set with `APPS="gin-gorm gombit"`. The **embedded-Gombit** single-binary
variant (`gombit build --embed`: binary + image size, cold start, idle memory)
is a follow-up slice — the footprint schema and CLI already carry it (`variant`
`embedded`, `binary_size_bytes`); only the frontend-embedding build is not wired
into the orchestrator yet.

## Running the framework-tax matrix

```sh
make benchmark-micro         # runs each stack + persists microbench.json for the report
# or, without persisting:
go test ./benchmarks/micro/... -bench=BenchmarkFrameworkTax -benchmem -count=10
```

Each row (`nethttp`, `gin`, `huma`, `gombit`) is its own package, so `go test`
runs each as its own process. That's required, not incidental: constructing a
`framework.App` (the `gombit` row) calls `contract.Install`, which replaces
Huma's process-global `huma.NewError` for the rest of that process — see
[micro/gombit/gombit.go](micro/gombit/gombit.go)'s doc comment.
`make benchmark-micro` runs the four packages separately for exactly this reason
and pipes each through `scripts/microbench`, accumulating ns/op / B/op /
allocs/op into `results/latest/microbench.json`; the README's Framework-tax
table publishes the typed-JSON scenario from it.

Every row's `TestScenarios` checks the same four scenarios (plaintext, JSON,
path parameter, validated POST) structurally before it's trusted for
benchmarking — see [micro/scenario/assert.go](micro/scenario/assert.go).

## Per-layer ablation

The four-row matrix shows the framework tax as one lump. The **ablation**
attributes it to individual Gombit middleware layers, reporting a cumulative
ladder — `baseline` (bare Huma+Gin) → each `runtimeMiddlewareStack` layer →
`full-app` (a real `framework.App`) — under the `gombit-ablation/<row>` stacks
in `microbench.json`. Each layer's incremental cost is the delta between
consecutive rows.

```sh
make benchmark-micro-ablation    # runs the ladder + merges into microbench.json
# or, without persisting:
go test ./framework -run='^$' -bench='^BenchmarkAblation$' -benchmem -count=10
```

It lives in [../framework/ablation_bench_test.go](../framework/ablation_bench_test.go),
in `package framework` rather than under `micro/`, because it builds the runtime
stack one **unexported** middleware at a time (issue #265). `TestAblationFullApp-
MatchesRuntimeStack` asserts the reconstructed full ladder allocates identically
to a real `framework.App`, so no runtime layer can escape the ablation. How to
read the deltas — and the two harness facts that matter (`httptest.NewRecorder`'s
3 constant allocs/op cancel in deltas; `Header.Clone` is real server behavior) —
is in [docs/methodology.md](docs/methodology.md#per-layer-ablation-issue-265--perf-7).

## Relationship to the M0-2 spike

This matrix supersedes `internal/contractspike`'s two-stack M0-2 spike
benchmark as the canonical, ongoing framework-tax measurement, but does not
modify or depend on it — that package's widget routes, tests, and committed
`openapi.json` are historical and untouched. See
[docs/spikes/M0-2_HUMA_GIN_SPIKE.md](../docs/spikes/M0-2_HUMA_GIN_SPIKE.md).
