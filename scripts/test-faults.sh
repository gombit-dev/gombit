#!/usr/bin/env bash
# The deterministic fault-injection suite: `make test-faults` (CHAOS-4,
# INV-7 — the required fault suite is reproducible and never depends on
# random scheduling). It runs, under the race detector:
#
#   - the internal/faulttest harness's own tests, whole;
#   - every TestFault_* test, in every package that declares one (found by
#     name, so a new fault test joins the suite without editing this file);
#   - with FAULT_POSTGRES_DSN, FAULT_MYSQL_DSN, and/or FAULT_REDIS_ADDR set,
#     the same packages again under the `integration` tag against those
#     dependencies, one package at a time (they share tables such as the
#     auth ones), each with the -<pkg>.postgres-dsn / -<pkg>.mysql-dsn /
#     -<pkg>.redis-addr flags it defines.
#
# Plain bash 3.2+ (macOS) and POSIX tools.
#
# FAULT_COUNT (default 1) repeats every test: FAULT_COUNT=100 is the flake
# soak. FAULT_SHARD=i/n runs one of n slices (CI's parallel jobs), and
# FAULT_BUDGET_SECONDS fails a run that takes longer. Nothing here reaches the internet or needs credentials beyond the
# test databases; plain `go test ./...` is unchanged by it.
#
# Every `go test` gets a -timeout per test binary that grows with
# FAULT_COUNT, since all the repetitions of a package run in one binary:
# FAULT_COUNT x 40s, never under go test's own 10m default, so a PR shard
# (FAULT_COUNT=1) runs as before and the x100 soak gets 4000s. The slowest
# package, auth against PostgreSQL and MySQL, takes about 16s a repetition,
# so 40s leaves it more than twice that. FAULT_TIMEOUT sets the timeout
# instead, in whole hours, minutes and seconds: one or more groups of a
# base-10 integer without leading zeros and a unit, h, m or s (90m, 1h30m,
# 4000s), more than zero in all. Fractions and the units below a second are
# refused, since go test reads 0.1ns as 0, which turns the timeout off. A
# timeout, given or computed, cannot pass the longest Go duration (about 292
# years), so a FAULT_COUNT whose 40s each would is refused too.
#
#   bash scripts/test-faults.sh
#   FAULT_POSTGRES_DSN=postgres://... FAULT_COUNT=100 bash scripts/test-faults.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

start=$(date +%s)

# positive checks $2 is a plain base-10 positive integer: go test's flags
# parse base 0, so a leading zero would be octal (010 is 8) and 00 is zero
# (run nothing, pass).
positive() {
  case "$2" in
  '' | *[!0-9]* | 0*)
    echo "test-faults: $1 must be a positive base-10 integer without leading zeros, got '$2'" >&2
    exit 2
    ;;
  esac
}

# maxTimeoutSeconds is the longest -timeout go test takes: a Go duration
# counts nanoseconds in an int64.
maxTimeoutSeconds=9223372036

count="${FAULT_COUNT:-1}"
positive FAULT_COUNT "$count"
# Checked before count x 40 is computed, which bash would let overflow (and
# fall back to the 10m floor). Nine digits are still under the limit, so the
# comparison never sees a number bash cannot hold.
if [ "${#count}" -gt 9 ] || [ "$count" -gt $((maxTimeoutSeconds / 40)) ]; then
  echo "test-faults: FAULT_COUNT must be at most $((maxTimeoutSeconds / 40)), so that its timeout (40s a repetition) fits a Go duration, got '$count'" >&2
  exit 2
fi

# duration checks $2 is a timeout in whole hours, minutes and seconds (see
# the top of this file) and sets durationSeconds to it. A group of up to ten
# digits, times 3600, cannot overflow bash's arithmetic, and the total is
# checked after each group.
duration() {
  local rest="$2" total=0 n
  local group='^(0|[1-9][0-9]{0,9})([hms])(.*)$'
  while [ -n "$rest" ] && [[ "$rest" =~ $group ]]; do
    n="${BASH_REMATCH[1]}"
    case "${BASH_REMATCH[2]}" in
    h) n=$((n * 3600)) ;;
    m) n=$((n * 60)) ;;
    esac
    rest="${BASH_REMATCH[3]}"
    total=$((total + n))
    if [ "$total" -gt "$maxTimeoutSeconds" ]; then
      break
    fi
  done
  if [ -n "$rest" ] || [ "$total" -eq 0 ] || [ "$total" -gt "$maxTimeoutSeconds" ]; then
    echo "test-faults: $1 must be whole hours, minutes and seconds such as 90m, 1h30m or 4000s, more than zero and at most ${maxTimeoutSeconds}s, got '$2'" >&2
    exit 2
  fi
  durationSeconds="$total"
}

testTimeout=""
if [ -n "${FAULT_TIMEOUT:-}" ]; then
  duration FAULT_TIMEOUT "$FAULT_TIMEOUT"
  testTimeout="${durationSeconds}s"
fi

# FAULT_SHARD=i/n runs the i-th of n round-robin slices of the suite's
# packages (CI runs the slices as parallel jobs, since compiling every test
# binary under -race serially outgrows one job's budget).
shard=1
shards=1
if [ -n "${FAULT_SHARD:-}" ]; then
  case "$FAULT_SHARD" in
  */*)
    shard="${FAULT_SHARD%/*}"
    shards="${FAULT_SHARD#*/}"
    ;;
  *)
    echo "test-faults: FAULT_SHARD must be i/n, got '$FAULT_SHARD'" >&2
    exit 2
    ;;
  esac
  positive "FAULT_SHARD's i" "$shard"
  positive "FAULT_SHARD's n" "$shards"
  if [ "$shard" -gt "$shards" ]; then
    echo "test-faults: FAULT_SHARD $FAULT_SHARD: i is larger than n" >&2
    exit 2
  fi
fi

# FAULT_COMPILE_ONLY=1 builds every test binary the run would use and runs
# no test (CI compiles, cached, in a step of its own, so the budget below
# measures the suite, not a cold -race build).
compileOnly="${FAULT_COMPILE_ONLY:-}"

# FAULT_BUDGET_SECONDS fails the run when it takes longer (the PR job's
# budget).
budget="${FAULT_BUDGET_SECONDS:-}"
if [ -n "$budget" ]; then
  positive FAULT_BUDGET_SECONDS "$budget"
fi

# run echoes a command (database DSNs masked) and runs it.
run() {
  local shown="$*"
  for dsn in "${FAULT_POSTGRES_DSN:-}" "${FAULT_MYSQL_DSN:-}" "${FAULT_REDIS_ADDR:-}"; do
    if [ -n "$dsn" ]; then
      shown="${shown//"$dsn"/<dsn>}"
    fi
  done
  echo "+ $shown"
  "$@"
}

harness=./internal/faulttest
# The packages of this module (`go list` leaves out testdata and nested
# modules such as the benchmark apps) that declare a TestFault_ test.
# Portable to macOS's bash 3.2 and BSD tools: no mapfile, no xargs -r.
# (Listed first, not piped, so a failing `go list` fails the script.)
dirs="$(go list -buildvcs=false -f '{{.Dir}}' ./...)"
pkgs=()
while IFS= read -r dir; do
  if grep -qsE '^func TestFault_' "$dir"/*_test.go; then
    pkgs+=("./${dir#"$ROOT"/}")
  fi
done <<<"$dirs"
if [ "${#pkgs[@]}" -eq 0 ]; then
  echo "test-faults: no TestFault_ tests found" >&2
  exit 1
fi

# This shard's packages: every n-th of the harness plus the fault packages.
all=("$harness" "${pkgs[@]}")
mine=()
i=0
for pkg in "${all[@]}"; do
  if [ $((i % shards + 1)) -eq "$shard" ]; then
    mine+=("$pkg")
  fi
  i=$((i + 1))
done

# The tests each run selects: the harness whole, elsewhere TestFault_ only;
# nothing when only compiling.
harnessRun='.'
faultRun='^TestFault_'
if [ -n "$compileOnly" ]; then
  harnessRun='^$'
  faultRun='^$'
  count=1
fi

# The per-binary timeout (see the top of this file).
if [ -z "$testTimeout" ]; then
  timeoutSeconds=$((count * 40))
  if [ "$timeoutSeconds" -lt 600 ]; then
    timeoutSeconds=600
  fi
  testTimeout="${timeoutSeconds}s"
fi

faultPkgs=()
for pkg in ${mine[@]+"${mine[@]}"}; do
  if [ "$pkg" = "$harness" ]; then
    run go test -race -count="$count" -timeout="$testTimeout" -run "$harnessRun" "$harness"
  else
    faultPkgs+=("$pkg")
  fi
done
if [ "${#faultPkgs[@]}" -gt 0 ]; then
  run go test -race -count="$count" -timeout="$testTimeout" -run "$faultRun" "${faultPkgs[@]}"
fi

if [ -n "${FAULT_POSTGRES_DSN:-}${FAULT_MYSQL_DSN:-}${FAULT_REDIS_ADDR:-}" ]; then
  # flagFor prints the -<prefix>.<name> flag pkg's tests define (e.g.
  # -framework.postgres-dsn), or nothing when they define none.
  flagFor() {
    local prefix
    prefix="$({ grep -rhoE --include='*_test.go' "flag\.String\(\"[a-z]+\.$2\"" "$1" 2>/dev/null || true; } |
      head -n1 | sed -E "s/.*\"([a-z]+)\.$2\"/\1/")"
    if [ -n "$prefix" ]; then
      echo "-$prefix.$2"
    fi
  }

  # Each package gets the flags it defines, for the dependencies configured.
  for pkg in ${mine[@]+"${mine[@]}"}; do
    args=()
    for pair in "postgres-dsn:${FAULT_POSTGRES_DSN:-}" "mysql-dsn:${FAULT_MYSQL_DSN:-}" "redis-addr:${FAULT_REDIS_ADDR:-}"; do
      name="${pair%%:*}"
      value="${pair#*:}"
      [ -n "$value" ] || continue
      flag="$(flagFor "$pkg" "$name")"
      if [ -n "$flag" ]; then
        args+=("$flag" "$value")
      fi
    done
    [ "${#args[@]}" -gt 0 ] || continue
    filter="$faultRun"
    if [ "$pkg" = "$harness" ]; then
      filter="$harnessRun" # the harness's database tests run whole
    fi
    run go test -tags integration -race -count="$count" -timeout="$testTimeout" -run "$filter" "$pkg" "${args[@]}"
  done
fi

elapsed=$(($(date +%s) - start))
echo "test-faults: shard $shard/$shards took ${elapsed}s${budget:+ (budget ${budget}s)}"
if [ -z "$compileOnly" ] && [ -n "$budget" ] && [ "$elapsed" -gt "$budget" ]; then
  echo "test-faults: over budget: ${elapsed}s > ${budget}s; add a shard rather than drop scenarios" >&2
  exit 1
fi
