# Benchmark developer UX (issue #141 §10). The all-in-one `benchmark` plus the
# crud/crud-all/micro/footprint/summary/report/metadata targets are here;
# -auth and -techempower land with their phases.
#
# Run configuration is sourced from benchmarks/config/versions.env (the single
# source of truth for the pinned k6 version, resource limits, and workload
# defaults); any variable can be overridden on the command line.

# A bare `make` at repo root must NOT run the multi-hour six-app suite and
# rewrite the committed README — it prints the target list instead. `benchmark`
# is an explicit command (issue #141 §10), never the accidental default goal, so
# the default is pinned to `help` here rather than left to fall through to
# whichever target happens to appear first in the file.
.DEFAULT_GOAL := help

BENCH_CONFIG ?= benchmarks/config/versions.env
include $(BENCH_CONFIG)

# Identity of the implementation under test, for the recorded result rows.
FRAMEWORK ?=
FRAMEWORK_VERSION ?=
RUNTIME ?=
RUNTIME_VERSION ?=
# The already-running, already-seeded app's list endpoint.
TARGET_URL ?=
# Where the snapshot is written.
OUT_DIR ?= benchmarks/results/latest

# The pinned intended limits (issue #141 §7). Standalone run-crud does NOT apply
# these — it starts nothing — so it records an honest "not applied" string
# instead; these are only surfaced by benchmark-metadata, labelled as intended.
# benchmark-crud-all is what actually enforces them (compose) and records the
# applied verdict per app (via inspect-limits).
INTENDED_LIMITS ?= intended (applied only under benchmark-crud-all): app $(APP_CPUS)cpu/$(APP_MEMORY); postgres $(POSTGRES_CPUS)cpu/$(POSTGRES_MEMORY)

.PHONY: help benchmark benchmark-smoke benchmark-crud benchmark-crud-all benchmark-micro benchmark-micro-ablation benchmark-footprint benchmark-summary benchmark-metadata benchmark-report benchmark-report-check

## help: list the benchmark targets (the default goal — a bare `make` prints
## this, never a multi-hour run). Full docs: benchmarks/README.md.
help:
	@echo "Gombit benchmark targets (make <target>; see benchmarks/README.md):"
	@echo ""
	@grep -hE '^## [a-z][a-z0-9-]*:' $(MAKEFILE_LIST) | sed -E 's/^## /  make /'

## benchmark: run the whole suite end to end into OUT_DIR and regenerate the
## README ## Performance block + summary.md — the one-command dedicated-host
## run. The CRUD pins come from versions.env (concurrency 1/10/100/500/1000,
## 5 trials x 30s, 10s warm-up); the cold-start count is footprint-all.sh's
## COLD_START_RUNS default (20). Narrow any on the command line for a reduced
## run, e.g.
##
##   make benchmark                          # canonical
##   make benchmark CONCURRENCY=1,10,100      # reduced sweep (unsustained 1000)
##
## Each step is invoked via $(MAKE) so they run strictly in order (they share the
## compose stack and OUT_DIR) even under `make -j`. Bring a fresh Postgres up
## first for clean app databases:
##   docker compose --env-file $(BENCH_CONFIG) -f benchmarks/compose.yml down -v && \
##     docker compose --env-file $(BENCH_CONFIG) -f benchmarks/compose.yml up -d postgres
benchmark:
	$(MAKE) benchmark-crud-all
	$(MAKE) benchmark-footprint
	$(MAKE) benchmark-micro
	$(MAKE) benchmark-report

# The apps the smoke runs end to end (default: all six) and the small
# deterministic seed it uses (issue #141 §11). 20 users / 100 projects is tiny
# but keeps a full 20-row first page, which the read workload asserts. Every app
# reads BENCH_SEED_USERS/BENCH_SEED_PROJECTS with the same semantics
# (benchmarks/apps/shared.SeedCounts and its per-language ports); unset falls
# back to the canonical 1,000/100,000.
BENCH_SMOKE_APPS ?= gin-gorm gombit django rails laravel nestjs
SMOKE_SEED_USERS ?= 20
SMOKE_SEED_PROJECTS ?= 100

## benchmark-smoke: per-PR correctness guard (issue #141 §11) — build all six
## app images (a broken Dockerfile fails here) and run the containerized harness
## end to end (compose up -> migrate -> seed -> k6 -> parse) for all six with a
## tiny deterministic seed and a 1-VU x 1 short trial, into a THROWAWAY dir so it
## never touches results/latest. Numbers are discarded; only pass/fail matters —
## a broken image, endpoint, migration/schema, orchestration, or result parser
## fails the run. This is what CI runs on every PR (the `benchmark-smoke` job in
## ci.yml). The small seed is what keeps all six affordable per PR.
benchmark-smoke:
	docker compose --env-file $(BENCH_CONFIG) -f benchmarks/compose.yml build
	@dir="$$(mktemp -d)"; trap 'rm -rf "$$dir"' EXIT; \
		BENCH_SEED_USERS=$(SMOKE_SEED_USERS) BENCH_SEED_PROJECTS=$(SMOKE_SEED_PROJECTS) \
		$(MAKE) benchmark-crud-all APPS="$(BENCH_SMOKE_APPS)" OUT_DIR="$$dir" \
			CONCURRENCY=1 TRIALS=1 DURATION_SECONDS=3 WARMUP_SECONDS=1

# Framework-tax microbenchmark sample count (-count). Every ns/op sample is
# persisted to microbench.json and the report publishes the median, so this is
# the number of samples per scenario, not a warmup knob.
MICRO_COUNT ?= 10

## benchmark-micro: run the framework-tax microbenchmark (net/http -> Gin ->
## Huma -> Gombit) and persist ns/op / B/op / allocs/op to OUT_DIR/microbench.json
## for the report. Each stack is its own `go test` process (a framework.App
## constructor mutates a process global), piped through the microbench parser.
benchmark-micro:
	@mkdir -p "$(OUT_DIR)"
	@rm -f "$(OUT_DIR)/microbench.json"
	bash -c 'set -euo pipefail; for s in nethttp gin huma gombit; do \
		echo "benchmark-micro: $$s"; \
		out="$$(go test ./benchmarks/micro/$$s -bench=BenchmarkFrameworkTax -benchmem -run="^$$" -count=$(MICRO_COUNT))" \
			|| { echo "$$out" >&2; echo "benchmark-micro: $$s failed" >&2; exit 1; }; \
		printf "%s\n" "$$out" | go run ./benchmarks/scripts/microbench -stack $$s -out "$(OUT_DIR)/microbench.json"; \
	done'

## benchmark-micro-ablation: run the Gombit per-layer middleware ablation
## benchmark (benchmarks/micro/gombit/ablation_bench_test.go) — progressively
## stacking the runtime middleware layers (recovery -> request_context ->
## metrics -> security_headers -> xss -> request_timeout for the production
## config; the CSRF layer is included only under cookie auth) — and merge its
## rows into OUT_DIR/microbench.json under the "gombit-ablation/<layer>"
## stacks. The layer set comes straight from the real runtime stack
## (framework.RuntimeMiddlewareLayers), so every row measures a layer that
## actually runs. This is a diagnostic that isolates each layer's incremental
## ns/op/B/op/allocs/op cost, not part of the published framework-tax ladder
## (which stays net/http -> gin -> huma -> gombit); run it standalone to see
## where in Gombit's stack the framework-tax delta actually comes from.
##
##   make benchmark-micro-ablation
##   go test ./benchmarks/micro/gombit -bench='^BenchmarkAblation$' -benchmem -count=10   # without persisting
benchmark-micro-ablation:
	@mkdir -p "$(OUT_DIR)"
	bash -c 'set -euo pipefail; \
		echo "benchmark-micro-ablation: gombit"; \
		out="$$(go test ./benchmarks/micro/gombit -bench=^BenchmarkAblation$$ -benchmem -run="^$$" -count=$(MICRO_COUNT))" \
			|| { echo "$$out" >&2; echo "benchmark-micro-ablation: failed" >&2; exit 1; }; \
		printf "%s\n" "$$out" | go run ./benchmarks/scripts/microbench -stack gombit-ablation -out "$(OUT_DIR)/microbench.json"'

## benchmark-report: regenerate the derived Markdown from OUT_DIR — the root
## README's `## Performance` block and summary.md. Markdown is generated, never
## hand-edited (issue #141 §9); benchmark-report-check fails on drift (for CI).
benchmark-report:
	@if [ -f "$(OUT_DIR)/results.json" ]; then \
		go run ./benchmarks/scripts/summarize -results "$(OUT_DIR)/results.json" -out "$(OUT_DIR)/summary.md"; \
	else echo "benchmark-report: no $(OUT_DIR)/results.json yet; skipping summary.md"; fi
	go run ./benchmarks/scripts/report \
		-results "$(OUT_DIR)/results.json" -footprint "$(OUT_DIR)/footprint.json" \
		-micro "$(OUT_DIR)/microbench.json" -metadata "$(OUT_DIR)/metadata.json"

## benchmark-report-check: fail if the committed README's Performance block no
## longer matches OUT_DIR (drift). Run before committing; CI runs it too
## (the `benchmark-report-drift` job in ci.yml).
benchmark-report-check:
	go run ./benchmarks/scripts/report -check \
		-results "$(OUT_DIR)/results.json" -footprint "$(OUT_DIR)/footprint.json" \
		-micro "$(OUT_DIR)/microbench.json" -metadata "$(OUT_DIR)/metadata.json"

## benchmark-footprint: measure the operational footprint (container-start cold
## start median/p95, idle memory, memory + CPU under load) of all six
## containerized apps into OUT_DIR/footprint.{json,csv}. Reduce COLD_START_RUNS /
## LOAD_SECONDS for a smoke; APPS="gin-gorm gombit" narrows the set. The
## embedded-Gombit single-binary variant is a follow-up slice.
##
##   make benchmark-footprint
##   make benchmark-footprint COLD_START_RUNS=3 APPS=gin-gorm   # smoke
benchmark-footprint:
	OUT_DIR="$(OUT_DIR)" bash benchmarks/scripts/footprint-all.sh

## benchmark-crud-all: bring every containerized implementation up under compose
## (with the §7 limits), migrate + seed it, classify + record the applied limit
## off the live container, run the CRUD-read workload against it, and merge all
## six into OUT_DIR/results.json. Each app is measured alone (stopped before the
## next) since the load generator shares the host. Run `make benchmark-summary`
## afterwards. Override the set with APPS="gin-gorm gombit"; the workload pins
## (CONCURRENCY/TRIALS/DURATION_SECONDS/...) are overridable on the command line.
##
##   make benchmark-crud-all
##   make benchmark-crud-all CONCURRENCY=1 TRIALS=1 DURATION_SECONDS=3   # smoke
benchmark-crud-all:
	OUT_DIR="$(OUT_DIR)" bash benchmarks/scripts/run-crud-all.sh

## benchmark-crud: run the headline CRUD-read workload against one running,
## seeded implementation and write results.json/results.csv/metadata.json.
## Requires the app to be up and seeded (see benchmarks/apps/<name>/README.md);
## `benchmark-crud-all` orchestrates all six under compose instead.
##
##   make benchmark-crud FRAMEWORK=gin-gorm FRAMEWORK_VERSION=v1.11.0 \
##     RUNTIME=go RUNTIME_VERSION=go1.25.7 \
##     TARGET_URL='http://127.0.0.1:8081/api/projects?page=1&limit=20'
benchmark-crud:
	@test -n "$(TARGET_URL)" || { echo "error: set TARGET_URL=<app list endpoint>"; exit 1; }
	@test -n "$(FRAMEWORK)" || { echo "error: set FRAMEWORK=<name>"; exit 1; }
	go run ./benchmarks/scripts/run-crud \
		-target-url "$(TARGET_URL)" \
		-framework "$(FRAMEWORK)" -framework-version "$(FRAMEWORK_VERSION)" \
		-runtime "$(RUNTIME)" -runtime-version "$(RUNTIME_VERSION)" \
		-concurrency "$(CONCURRENCY)" \
		-duration "$(DURATION_SECONDS)s" -warmup "$(WARMUP_SECONDS)s" -trials "$(TRIALS)" \
		-k6-image "grafana/k6:$(K6_VERSION)" \
		-out-dir "$(OUT_DIR)" \
		-postgres-version "$(POSTGRES_IMAGE)"
	@# resource-limits deliberately omitted: run-crud does not start or
	@# constrain the app, so it records its honest "not applied" default
	@# rather than the intended pins this target never enforces.

## benchmark-summary: regenerate the human report (summary.md) from the
## structured OUT_DIR/results.json — per-(framework, concurrency) aggregates
## with trial variance and the >5% coefficient-of-variation flag. Markdown is
## never hand-edited; re-run this after any run-crud that changed results.json.
benchmark-summary:
	go run ./benchmarks/scripts/summarize \
		-results "$(OUT_DIR)/results.json" \
		-out "$(OUT_DIR)/summary.md"

## benchmark-metadata: write just the reproducibility metadata for the current
## host and pinned run configuration to OUT_DIR/metadata.json.
benchmark-metadata:
	@mkdir -p "$(OUT_DIR)"
	go run ./benchmarks/scripts/collect-host-info \
		-out "$(OUT_DIR)/metadata.json" \
		-benchmark-tool "grafana/k6:$(K6_VERSION)" \
		-postgres-version "$(POSTGRES_IMAGE)" \
		-concurrency "$(CONCURRENCY)" \
		-duration-seconds "$(DURATION_SECONDS)" -warmup-seconds "$(WARMUP_SECONDS)" \
		-trials "$(TRIALS)" \
		-resource-limits "$(INTENDED_LIMITS)"
