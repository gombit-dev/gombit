#!/usr/bin/env bash
# Measure the operational footprint of every containerized implementation (issue
# #141 §"Operational footprint") into OUT_DIR/footprint.{json,csv}: container-
# start → ready cold-start (median/p95 over COLD_START_RUNS restarts), idle
# memory, and memory + CPU under load, for all six apps.
#
# It reuses run-crud-all.sh (sourced) for app identity derivation, the compose
# wrapper, and the health wait; the throughput half is `make benchmark-crud-all`.
#
# The embedded-Gombit single-binary variant (`gombit build --embed`: binary +
# image size, cold start, idle memory) is a follow-up slice — the footprint
# schema already carries it (variant "embedded", binary_size_bytes) and the
# footprint CLI accepts it; only the `gombit build --embed` frontend-build step
# is not wired here yet.
#
#   make benchmark-footprint
#   COLD_START_RUNS=3 APPS=gin-gorm make benchmark-footprint   # smoke
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"
# shellcheck source=/dev/null
APPS="" source benchmarks/scripts/run-crud-all.sh

OUT="${OUT_DIR:-benchmarks/results/latest}/footprint.json"
APPS="${APPS:-gin-gorm gombit django rails laravel nestjs}"
COLD_START_RUNS="${COLD_START_RUNS:-20}"
LOAD_VUS="${LOAD_VUS:-100}"
LOAD_SECONDS="${LOAD_SECONDS:-10}"
IDLE_SETTLE="${IDLE_SETTLE:-10}" # issue #141's suggested settle before idle RSS

# Seams over the Go tools so the orchestration can be driven with fakes in the
# test. run_load drives *validated* load (k6load keeps + validates the summary
# and exits non-zero on dirty/absent traffic — the load generator is the
# authority for whether load happened); record_footprint writes the row.
# run_load uses a PRE-BUILT k6load binary (K6LOAD_BIN, built once in main) rather
# than `go run` so the ~1-2s Go compile does not delay k6's start and leave the
# early samples reading an idle app. Falls back to `go run` if unset (tests
# override this seam anyway).
run_load() {
  if [ -n "${K6LOAD_BIN:-}" ]; then "$K6LOAD_BIN" "$@"; else go run ./benchmarks/scripts/k6load "$@"; fi
}
record_footprint() { go run ./benchmarks/scripts/footprint "$@"; }

# median_int / median_float read numbers on stdin and echo the median (integer
# floored for byte counts). Empty input FAILS (non-zero) rather than printing a
# 0 sentinel — an absent series is not "0 bytes", and the caller must refuse the
# row, not record a zero (issue #141: don't publish bogus data).
median_int() { sort -n | awk '{a[NR]=$1} END{ if(NR==0){exit 1} if(NR%2){printf "%d", a[(NR+1)/2]} else {printf "%d", (a[NR/2]+a[NR/2+1])/2} }'; }
median_float() { sort -n | awk '{a[NR]=$1} END{ if(NR==0){exit 1} if(NR%2){print a[(NR+1)/2]} else {print (a[NR/2]+a[NR/2+1])/2} }'; }

# to_bytes converts a `docker stats` size token (45.2MiB, 1.1GiB, 900B, …) to an
# integer byte count on stdin.
to_bytes() {
  awk '{
    n = $0 + 0
    if ($0 ~ /GiB/)      printf "%d", n * 1073741824
    else if ($0 ~ /MiB/) printf "%d", n * 1048576
    else if ($0 ~ /KiB/) printf "%d", n * 1024
    else if ($0 ~ /GB/)  printf "%d", n * 1000000000
    else if ($0 ~ /MB/)  printf "%d", n * 1000000
    else if ($0 ~ /kB|KB/) printf "%d", n * 1000
    else                 printf "%d", n
  }'
}

# mem_bytes CONTAINER — the container's memory usage (cgroup working set), bytes.
mem_bytes() {
  docker stats --no-stream --format '{{.MemUsage}}' "$1" | awk -F' / ' '{print $1}' | to_bytes
}
# cpu_pct CONTAINER — CPU percent (docker stats; 100 == one core).
cpu_pct() { docker stats --no-stream --format '{{.CPUPerc}}' "$1" | tr -d '% '; }

# stats_sample CONTAINER — one "<mem_bytes> <cpu_pct>" line from a single
# `docker stats --no-stream` call (which already blocks ~1s, so this is the 1s
# tick — no extra sleep needed, and it reads mem+cpu from the same instant).
stats_sample() {
  docker stats --no-stream --format '{{.MemUsage}}|{{.CPUPerc}}' "$1" \
    | awk -F'|' '{gsub(/ .*/, "", $1); print $1, $2}' \
    | { read -r mem cpu; printf '%s %s\n' "$(printf '%s' "$mem" | to_bytes)" "$(printf '%s' "$cpu" | tr -d '% ')"; }
}

# now_ms — MONOTONIC milliseconds (CLOCK_MONOTONIC via /proc/uptime, ~10ms
# resolution). Elapsed timing uses this, NOT `date +%s%3N` (CLOCK_REALTIME):
# two realtime reads can straddle a wall-clock adjustment and yield a garbage
# difference (a real run produced -1609081096361592473, the shape of an ms read
# minus a nanosecond-epoch read). /proc/uptime never runs backwards, so
# start->ready elapsed is always a sane non-negative number.
now_ms() { awk '{printf "%d", $1 * 1000}' /proc/uptime; }

# wait_ready_ms URL — poll until HTTP 200, echo elapsed monotonic milliseconds
# (or fail).
wait_ready_ms() {
  local url="$1" start now code
  start="$(now_ms)"
  for _ in $(seq 1 600); do
    code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 "$url" 2>/dev/null || true)"
    if [ "$code" = "200" ]; then
      now="$(now_ms)"
      echo "$((now - start))"
      return 0
    fi
    sleep 0.05
  done
  return 1
}

# sample_stats CONTAINER FILE STOPFILE — until STOPFILE exists, append one
# "<mem_bytes> <cpu_pct>" line per `docker stats` tick (~1s). Runs concurrently
# with the load so the samples are provably taken while k6 drives the app.
sample_stats() {
  while [ ! -f "$3" ]; do
    stats_sample "$1" >> "$2"
  done
}

# aggregate_load_samples FILE — echo "<loaded_rss_bytes> <cpu_pct>" as the
# steady-state median of the samples, dropping the first (ramp) sample. FAILS
# (non-zero) unless at least two in-load samples remain: a missing or one-sample
# series must not become a 0-byte/0% "loaded" reading (that would be a sentinel
# with a field name, not a measurement). median_* also fail on empty input.
aggregate_load_samples() {
  local n loaded cpu
  n="$(wc -l < "$1")"
  if [ "$n" -lt 3 ]; then
    return 1 # need >= 2 after dropping the ramp sample
  fi
  loaded="$(tail -n +2 "$1" | awk '{print $1}' | median_int)" || return 1
  cpu="$(tail -n +2 "$1" | awk '{print $2}' | median_float)" || return 1
  printf '%s %s' "$loaded" "$cpu"
}

# measure_under_load CONTAINER TARGET_URL — echo "<loaded_rss_bytes> <cpu_pct>"
# from steady-state samples taken *while validated load runs*, or fail (non-zero)
# if the load was not a clean measurement (k6load Validate) OR too few in-load
# samples exist — so neither a failed/absent k6 nor an empty sample series yields
# a loaded/CPU number.
measure_under_load() {
  local cid="$1" turl="$2" sf stop spid out load_ok=0
  sf="$(mktemp)"; stop="$sf.stop"; rm -f "$stop"
  sample_stats "$cid" "$sf" "$stop" &
  spid=$!
  # run_load's own output must not pollute this function's stdout (the caller
  # captures it for "<loaded> <cpu>"); send it to stderr.
  if run_load -target-url "$turl" -vus "$LOAD_VUS" -duration "${LOAD_SECONDS}s" \
      -k6-image "grafana/k6:$K6_VERSION" -workload "$ROOT/benchmarks/workloads/crud-list.js" >&2; then
    load_ok=1
  fi
  touch "$stop"; wait "$spid" 2>/dev/null || true

  if [ "$load_ok" -ne 1 ]; then
    rm -f "$sf" "$stop"
    return 1
  fi
  out="$(aggregate_load_samples "$sf")" || { rm -f "$sf" "$stop"; return 1; }
  rm -f "$sf" "$stop"
  printf '%s' "$out"
}

# cold_start_samples APP LIVEZ_URL — echo COLD_START_RUNS comma-separated
# start→ready samples (ms), stopping/starting the existing container each time.
cold_start_samples() {
  local app="$1" url="$2" samples="" ms attempt
  for _ in $(seq 1 "$COLD_START_RUNS"); do
    ms=""
    # wait_ready_ms measures elapsed from the MONOTONIC clock (now_ms), so a
    # sample can't be corrupted by a wall-clock step. This is defense in depth on
    # top of that: a well-formed sample is a non-negative integer <= 60000 ms
    # (wait_ready_ms polls at most ~30s), and anything else — a malformed reading
    # from some other cause — is neither recorded nor allowed to fail-close the
    # whole run; we REDO the stop/start (a fresh cold start), up to 5 times, then
    # fail. It is a garbage filter, not clock-step detection.
    for attempt in 1 2 3 4 5; do
      "${COMPOSE[@]}" stop "$app" >/dev/null
      "${COMPOSE[@]}" start "$app" >/dev/null
      ms="$(wait_ready_ms "$url")" || { echo "footprint: $app cold-start did not become ready" >&2; return 1; }
      if [[ "$ms" =~ ^[0-9]+$ ]] && [ "$ms" -le 60000 ]; then
        break
      fi
      echo "footprint: $app cold-start sample '$ms' malformed; redoing stop/start ($attempt/5)" >&2
      ms=""
    done
    [ -n "$ms" ] || { echo "footprint: $app cold-start kept producing a malformed sample" >&2; return 1; }
    samples="${samples:+$samples,}$ms"
  done
  printf '%s' "$samples"
}

# do_measure APP CID — the measured half (cold-start, idle, load, record).
# Called as `do_measure ... || rc`, so set -e is disabled inside it; every
# failure a bad row could hide behind is checked explicitly, and a failed load
# never publishes loaded/CPU (measure_under_load fails closed).
do_measure() {
  local app="$1" cid="$2" livez turl samples idle image loaded_cpu loaded cpu
  livez="http://127.0.0.1:${PORT}/livez"
  turl="http://127.0.0.1:${PORT}/api/projects?page=1&limit=20"

  wait_healthy "$cid" || return 1
  samples="$(cold_start_samples "$app" "$livez")" || return 1

  sleep "$IDLE_SETTLE" # settle before reading idle memory
  idle="$(mem_bytes "$cid")" || return 1

  loaded_cpu="$(measure_under_load "$cid" "$turl")" || {
    echo "footprint: $app load was not a clean measurement; not publishing a row" >&2
    return 1
  }
  loaded="${loaded_cpu% *}"
  cpu="${loaded_cpu#* }"

  # Inspect the app image by the TAG the container was created with
  # (.Config.Image, e.g. bench-gin-gorm:local), not the resolved image ID
  # (.Image). measure_container rebuilds the image just above; a rebuild retags
  # to a new ID and can leave the old ID (which a reused container still pins via
  # .Image) dangling/removed, so `docker image inspect <old-id>` fails with "No
  # such image". The tag always resolves to the current app image, which is what
  # the footprint's image-size measures anyway.
  image="$(docker image inspect "$(docker inspect "$cid" --format '{{.Config.Image}}')" --format '{{.Size}}')" || return 1

  record_footprint \
    -framework "$app" -framework-version "$FRAMEWORK_VERSION" \
    -runtime "$RUNTIME" -runtime-version "$RUNTIME_VERSION" \
    -variant container \
    -cold-start-ms "$samples" \
    -idle-rss-bytes "$idle" -loaded-rss-bytes "$loaded" -cpu-percent "$cpu" \
    -image-size-bytes "$image" \
    -out "$OUT"
}

# measure_container APP — bring the app up, measure it, and ALWAYS stop it
# before returning (even when do_measure fails under set -e), so the SUT never
# shares the host with the next app's load. Mirrors run-crud-all.sh's run_one.
measure_container() {
  local app="$1" cid rc=0
  app_identity "$app"
  echo "=== footprint: $app ($FRAMEWORK_VERSION on $RUNTIME $RUNTIME_VERSION) ==="

  "${COMPOSE[@]}" build "$app" >/dev/null
  "${COMPOSE[@]}" run --rm "$app" migrate
  "${COMPOSE[@]}" run --rm "$app" seed
  "${COMPOSE[@]}" up -d "$app" >/dev/null
  cid="$("${COMPOSE[@]}" ps -q "$app")"

  do_measure "$app" "$cid" || rc=$?
  "${COMPOSE[@]}" stop "$app" >/dev/null
  return "$rc"
}

main() {
  echo "footprint: ensuring postgres is up"
  "${COMPOSE[@]}" up -d postgres >/dev/null
  # Pre-pull the load generator and pre-build k6load so neither an image pull
  # nor a Go compile lands inside a measured load window (either would leave the
  # samples reading idle, not loaded).
  docker pull -q "grafana/k6:$K6_VERSION" >/dev/null
  # Not `local`: the EXIT trap fires in the global scope after main returns, so a
  # function-local bindir would be unbound there and `set -u` would abort the
  # whole run at exit (even after the footprint rows were written successfully).
  bindir="$(mktemp -d)"
  trap 'rm -rf "${bindir:-}"' EXIT
  K6LOAD_BIN="$bindir/k6load"
  go build -o "$K6LOAD_BIN" ./benchmarks/scripts/k6load
  for app in $APPS; do
    measure_container "$app"
  done
  # Record which commit and host produced these rows. Footprint is a separate
  # measurement group from the CRUD sweep and the microbenchmark, so it stamps
  # its own provenance and leaves the rest of metadata.json untouched — a
  # snapshot-wide commit would vouch for numbers that never ran at it
  # (issue #266).
  mkdir -p "$(dirname "$OUT")"
  go run ./benchmarks/scripts/collect-host-info -group footprint -out "$(dirname "$OUT")/metadata.json"
  echo "footprint: done. Rows in $OUT (and .csv)."
}

if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  main "$@"
fi
