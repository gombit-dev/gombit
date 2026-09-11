#!/usr/bin/env bash
# Orchestrate the headline CRUD-read workload across all six containerized
# implementations and merge the result into one results.json.
#
# For each app it: builds the image, applies migrations and seeds (the app's
# own `migrate`/`seed` verbs, which create their database idempotently), brings
# the service up under its §7 cgroup budget, waits for /livez to report healthy,
# classifies the *actually applied* limit off the live container
# (benchmarks/scripts/inspect-limits) and records that honest string, runs the
# per-implementation measurement engine (benchmarks/scripts/run-crud) against
# it, then stops the service so the next app is measured alone with Postgres —
# the load generator shares the host, so only the app under test should run.
#
# run-crud merges by framework key, so the six iterations accumulate into
# benchmarks/results/latest/{results.json,results.csv,metadata.json}. Regenerate
# the human report afterwards with `make benchmark-summary`.
#
#   make benchmark-crud-all                           # all six
#   APPS="gin-gorm gombit" make benchmark-crud-all    # a subset
#   make benchmark-crud-all CONCURRENCY=1 TRIALS=1 DURATION_SECONDS=3   # smoke
#
# Versions are DERIVED from each app's own source-of-truth (framework manifest
# file, Dockerfile base image) and the run fails closed if any comes back empty
# — nothing about the recorded identity is hand-copied here. The framework key
# is the compose service name, matching the rest of the harness.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

CONFIG="benchmarks/config/versions.env"

# load_pins sets each versions.env pin ONLY if it is not already in the
# environment, so `make benchmark-crud-all CONCURRENCY=1` (make exports
# command-line variables to recipe environments) and `CONCURRENCY=1 bash
# run-crud-all.sh` override the file, instead of the file clobbering them.
# printf -v sets the named variable without eval.
load_pins() {
  local k v
  while IFS='=' read -r k v; do
    case "$k" in ''|\#*) continue ;; esac
    if [ -z "${!k-}" ]; then
      printf -v "$k" '%s' "$v"
    fi
  done < <(grep -vE '^[[:space:]]*#|^[[:space:]]*$' "$CONFIG")
}
load_pins

COMPOSE=(docker compose --env-file "$CONFIG" -f benchmarks/compose.yml)
OUT_DIR="${OUT_DIR:-benchmarks/results/latest}"
# Which apps to run (default: all six, in a stable order).
APPS="${APPS:-gin-gorm gombit django rails laravel nestjs}"
# The shared Postgres container's applied-limit verdict, classified once in
# main (verify_postgres_limits) and recorded on every app's row. Declared here
# so measure() references it safely under set -u even if verification is skipped.
POSTGRES_LIMITS="${POSTGRES_LIMITS:-}"

# Seams over the Go tools so the orchestration can be driven with fakes in the
# test without Docker or a real build.
inspect_limits() { go run ./benchmarks/scripts/inspect-limits "$@"; }
run_crud() { go run ./benchmarks/scripts/run-crud "$@"; }

# req NAME VALUE — echo VALUE, or abort if it is empty (fail closed so a broken
# extractor never records a blank version).
req() {
  if [ -z "${2:-}" ]; then
    echo "run-crud-all: could not determine $1" >&2
    exit 1
  fi
  printf '%s' "$2"
}

# base_tag APP LANG — the base image tag from the app's Dockerfile `FROM
# <lang>:<tag>` line, with any -slim/-alpine/-fpm suffix stripped (3.12-slim ->
# 3.12, 1.26.0-alpine -> 1.26.0).
base_tag() {
  local tag
  tag="$(sed -nE "s/^FROM ${2}:([^ ]+).*/\1/p" "benchmarks/apps/${1}/Dockerfile" | head -1)"
  printf '%s' "${tag%%-*}"
}

# app_identity APP — sets PORT FRAMEWORK FRAMEWORK_VERSION RUNTIME
# RUNTIME_VERSION. FRAMEWORK is the compose service name (== the run-crud merge
# key the rest of the harness uses); versions are derived from source.
app_identity() {
  FRAMEWORK="$1"
  case "$1" in
    gin-gorm)
      PORT=8081; RUNTIME=go
      FRAMEWORK_VERSION="$(req gin "$(awk '/gin-gonic\/gin /{print $2; exit}' go.mod)")"
      RUNTIME_VERSION="go$(req go "$(base_tag gin-gorm golang)")" ;;
    gombit)
      PORT=8080; RUNTIME=go
      FRAMEWORK_VERSION="$(req gombit "$(git describe --tags --always --dirty)")"
      RUNTIME_VERSION="go$(req go "$(base_tag gombit golang)")" ;;
    django)
      PORT=8082; RUNTIME=python
      FRAMEWORK_VERSION="$(req django "$(sed -nE 's/^Django==([0-9.]+).*/\1/p' benchmarks/apps/django/requirements.txt)")"
      RUNTIME_VERSION="$(req python "$(base_tag django python)")" ;;
    rails)
      PORT=8083; RUNTIME=ruby
      FRAMEWORK_VERSION="$(req rails "$(sed -nE 's/^ *rails \(([0-9.]+)\).*/\1/p' benchmarks/apps/rails/Gemfile.lock | head -1)")"
      RUNTIME_VERSION="$(req ruby "$(base_tag rails ruby)")" ;;
    laravel)
      PORT=8084; RUNTIME=php
      FRAMEWORK_VERSION="$(req laravel "$(sed -nE 's/.*"laravel\/framework": "([0-9.]+)".*/\1/p' benchmarks/apps/laravel/composer.json)")"
      RUNTIME_VERSION="$(req php "$(base_tag laravel php)")" ;;
    nestjs)
      PORT=8085; RUNTIME=node
      FRAMEWORK_VERSION="$(req nestjs "$(sed -nE 's/.*"@nestjs\/core": "([0-9.]+)".*/\1/p' benchmarks/apps/nestjs/package.json)")"
      RUNTIME_VERSION="$(req node "$(base_tag nestjs node)")" ;;
    *)
      echo "run-crud-all: unknown app '$1'" >&2; exit 1 ;;
  esac
}

# wait_healthy CONTAINER — block until the container's health check passes.
wait_healthy() {
  local cid="$1" i status
  for i in $(seq 1 60); do
    status="$(docker inspect --format '{{.State.Health.Status}}' "$cid" 2>/dev/null || echo missing)"
    case "$status" in
      healthy) return 0 ;;
      unhealthy|missing) echo "run-crud-all: $cid is $status" >&2; return 1 ;;
    esac
    sleep 2
  done
  echo "run-crud-all: $cid did not become healthy in time" >&2
  return 1
}

# measure APP CID — classify the applied limit and run the workload. §7 is
# detect/report, so enforced / partial / not-applied are all recorded as-is;
# but inspect-limits exiting non-zero is a TOOL failure (bad args, docker
# error), and on that we fail the app rather than record a blank
# resource_limits (which would override run-crud's honest default and, via
# mergedMetadata's last-write, blank the field for the whole snapshot).
measure() {
  local app="$1" cid="$2" limits
  # measure is called as `measure || rc=$?`, which disables set -e inside it, so
  # every failure a blank row could hide from is checked explicitly here.
  wait_healthy "$cid" || return 1
  if ! limits="$(inspect_limits -container "$cid" -cpus "$APP_CPUS" -memory "$APP_MEMORY")"; then
    echo "run-crud-all: inspect-limits failed for $app; not publishing a row" >&2
    return 1
  fi
  echo "run-crud-all: $app applied limit: $limits"
  run_crud \
    -target-url "http://127.0.0.1:${PORT}/api/projects?page=1&limit=20" \
    -framework "$FRAMEWORK" -framework-version "$FRAMEWORK_VERSION" \
    -runtime "$RUNTIME" -runtime-version "$RUNTIME_VERSION" \
    -concurrency "$CONCURRENCY" \
    -duration "${DURATION_SECONDS}s" -warmup "${WARMUP_SECONDS}s" -trials "$TRIALS" \
    -k6-image "grafana/k6:$K6_VERSION" \
    -out-dir "$OUT_DIR" \
    -postgres-version "$POSTGRES_IMAGE" \
    -resource-limits "$limits" \
    -postgres-resource-limits "$POSTGRES_LIMITS"
}

# verify_postgres_limits — classify the shared Postgres container's applied
# limit once for the whole snapshot (it's the same container across every app),
# into the POSTGRES_LIMITS global that measure() records. Postgres is up before
# this runs. It never aborts the run since the DB is shared context, not a
# per-app SUT.
#
# When it runs but cannot classify (missing container / inspect-tool failure) it
# records an EXPLICIT "unknown …" string — NOT empty. Empty is reserved for the
# standalone `benchmark-crud` path that never re-verified: mergedMetadata keeps a
# prior verdict on empty ("not provided") but must overwrite it on a verified
# unknown ("this run looked and could not tell"), so a stale enforced/partial can
# never stick across a re-run whose check failed. One sentinel, two meanings, is
# exactly the bug this distinction avoids.
verify_postgres_limits() {
  local cid
  cid="$("${COMPOSE[@]}" ps -q postgres)"
  if [ -z "$cid" ]; then
    echo "run-crud-all: postgres container not found; postgres limit unknown" >&2
    POSTGRES_LIMITS="unknown (postgres container not found)"
    return 0
  fi
  if ! POSTGRES_LIMITS="$(inspect_limits -container "$cid" -cpus "$POSTGRES_CPUS" -memory "$POSTGRES_MEMORY")"; then
    echo "run-crud-all: inspect-limits failed for postgres; postgres limit unknown" >&2
    POSTGRES_LIMITS="unknown (inspect-limits failed)"
    return 0
  fi
  echo "run-crud-all: postgres applied limit: $POSTGRES_LIMITS"
}

# run_one APP — the full measured cycle for one implementation. The SUT is
# always stopped before returning (even when measure fails under set -e) so it
# never shares the host with the next app's run.
run_one() {
  local app="$1" cid rc=0
  app_identity "$app"
  echo "=== $app ($FRAMEWORK $FRAMEWORK_VERSION on $RUNTIME $RUNTIME_VERSION, :$PORT) ==="

  "${COMPOSE[@]}" build "$app" >/dev/null
  "${COMPOSE[@]}" run --rm "$app" migrate
  "${COMPOSE[@]}" run --rm "$app" seed
  "${COMPOSE[@]}" up -d "$app" >/dev/null
  cid="$("${COMPOSE[@]}" ps -q "$app")"

  measure "$app" "$cid" || rc=$?
  "${COMPOSE[@]}" stop "$app" >/dev/null
  return "$rc"
}

main() {
  echo "run-crud-all: ensuring postgres is up"
  "${COMPOSE[@]}" up -d postgres >/dev/null
  verify_postgres_limits
  for app in $APPS; do
    run_one "$app"
  done
  echo "run-crud-all: done. Results in $OUT_DIR; run 'make benchmark-summary' to render summary.md."
}

# Only run the orchestration when executed directly; sourcing (e.g. from the
# test) just defines the functions above.
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  main "$@"
fi
