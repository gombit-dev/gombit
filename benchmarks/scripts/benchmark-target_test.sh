#!/usr/bin/env bash
# Tests for the all-in-one `make benchmark` target (issue #141 §10). The four
# stages are composed as sequential recursive `$(MAKE)` lines in ONE recipe — not
# a prerequisite list — precisely so they stay ordered under `make -j` (four
# compose-sharing stages must never run at once) and so a failed stage aborts the
# chain. None of that is exercised by the per-stage tests, and the obvious
# "cleanup" refactor to `benchmark: benchmark-crud-all benchmark-footprint ...`
# looks identical without `-j`. This locks the contract WITHOUT Docker by
# overriding the special `MAKE` variable so every `$(MAKE) benchmark-<stage>`
# line runs a recording stub instead of the real (hours-long) stage.
#
#   bash benchmarks/scripts/benchmark-target_test.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

fail=0
note() { echo "FAIL: $*" >&2; fail=1; }

# ---- 1. a bare `make` must not be the full suite ----
# The default goal must be an explicit, cheap target (help) — never `benchmark`,
# which would make a bare `make` at repo root a multi-hour run that rewrites the
# committed README. Reading it off `make -p` is the same check the review used.
goal="$(make -p -n 2>/dev/null | grep -E '^\.DEFAULT_GOAL' | head -1 | sed -E 's/.*:= *//')"
[ "$goal" = "help" ] || note "default goal is '$goal', want 'help' (a bare make must not run the suite)"

# ---- 2. `benchmark` is composed as recipe lines, not prerequisites ----
# A prerequisite list (`benchmark: benchmark-crud-all ...`) would let `make -j`
# run the four stages concurrently. The rule's own line must carry no
# prerequisites; the ordering lives in the recipe body.
prereqs="$(sed -nE 's/^benchmark:[[:space:]]*(.*)$/\1/p' Makefile)"
[ -z "$prereqs" ] || note "benchmark: has prerequisites '$prereqs' — -j would parallelize the stages; use recipe lines"

# The make-driven cases below intercept the stages by overriding the special
# MAKE variable, which only reaches recursive `$(MAKE)` lines — NOT a
# prerequisite list, whose stages the parent make would build for real (Docker).
# So if the composition regressed to prerequisites, fail fast here instead of
# launching the real six-app suite.
if [ "$fail" -ne 0 ]; then
  echo "benchmark-target_test: FAILED (structural checks; skipping make-driven cases to avoid running the real suite)" >&2
  exit 1
fi

# ---- stub over the recursive $(MAKE) so no real stage runs ----
stubdir="$(mktemp -d)"
trap 'rm -rf "${stubdir:-}"' EXIT
REC="$stubdir/log"
cat > "$stubdir/recmake" <<'EOF'
#!/usr/bin/env bash
# Records the stage (its make target arg) and the pins it inherited, then fails
# if asked to — standing in for one recursive `$(MAKE) benchmark-<stage>` call.
echo "$1 CONCURRENCY=${CONCURRENCY:-unset} OUT_DIR=${OUT_DIR:-unset} APPS=${APPS:-unset}" >> "$REC"
[ "${FAIL_ON:-}" = "$1" ] && exit 1
exit 0
EOF
chmod +x "$stubdir/recmake"
run_benchmark() { REC="$REC" make benchmark MAKE="$stubdir/recmake" "$@"; }

# ---- 3. stages run in the fixed order, even under -j ----
: > "$REC"
run_benchmark -j4 CONCURRENCY=1,10 OUT_DIR=/tmp/bench-x APPS=gin-gorm >/dev/null 2>&1 || note "benchmark chain failed on a clean stub run"
got="$(cut -d' ' -f1 < "$REC" | paste -sd, -)"
want="benchmark-crud-all,benchmark-footprint,benchmark-micro,benchmark-report"
[ "$got" = "$want" ] || note "stage order under -j = '$got', want '$want'"

# ---- 4. command-line pins reach every stage's environment ----
while read -r line; do
  case "$line" in
    *"CONCURRENCY=1,10 OUT_DIR=/tmp/bench-x APPS=gin-gorm"*) : ;;
    *) note "a stage did not inherit the CLI pins: $line" ;;
  esac
done < "$REC"

# ---- 5. a failed stage aborts the chain (no later stage runs) ----
: > "$REC"
rc=0
FAIL_ON=benchmark-footprint run_benchmark >/dev/null 2>&1 || rc=$?
[ "$rc" -ne 0 ] || note "benchmark should fail when a stage fails"
ran="$(cut -d' ' -f1 < "$REC" | paste -sd, -)"
[ "$ran" = "benchmark-crud-all,benchmark-footprint" ] \
  || note "fail-closed broken: stages that ran = '$ran', want the chain to stop after benchmark-footprint"
if grep -qE '^(benchmark-micro|benchmark-report)' "$REC"; then
  note "a stage ran after the failed one (chain did not abort)"
fi

# ---- 5b. BENCHMARK_HOST_CLASS is checked before any stage runs ----
# A typo would otherwise surface only in the README banner after the whole
# multi-hour run (issue #291). Every value the Go side accepts
# (metadata.ParseHostClass: dedicated, developer, unset) must pass here too.
for v in "" dedicated developer; do
  : > "$REC"
  BENCHMARK_HOST_CLASS="$v" run_benchmark >/dev/null 2>&1 || note "BENCHMARK_HOST_CLASS='$v' was refused; the Go producers accept it"
  [ -s "$REC" ] || note "BENCHMARK_HOST_CLASS='$v': no stage ran"
done
for v in Dedicated dedicaed laptop; do
  : > "$REC"
  rc=0
  BENCHMARK_HOST_CLASS="$v" run_benchmark >/dev/null 2>&1 || rc=$?
  [ "$rc" -ne 0 ] || note "BENCHMARK_HOST_CLASS='$v' was accepted"
  [ ! -s "$REC" ] || note "BENCHMARK_HOST_CLASS='$v': a stage ran before the refusal: $(cat "$REC")"
done

# ---- 5c. every measuring stage refuses a bad class before measuring ----
# Not only the composite target: a stage that lost its guard must fail here. The
# footprint binary records rows only after footprint-all.sh has measured them,
# so the stage target and the script themselves must refuse first. `docker` and
# `go` are stubbed to record any call and fail, so a stage missing its guard
# stops at its first tool call instead of polling a fake stack; a refused stage
# makes none. The scripts are also run directly, as the docs allow.
toolrec="$stubdir/toollog"
for tool in docker go; do
  printf '#!/usr/bin/env bash\necho "%s $*" >> "%s"\nexit 1\n' "$tool" "$toolrec" > "$stubdir/$tool"
  chmod +x "$stubdir/$tool"
done
refused_before_measuring() { # LABEL CMD...
  local label="$1" rc=0; shift
  : > "$toolrec"
  BENCHMARK_HOST_CLASS=Dedicated PATH="$stubdir:$PATH" "$@" >/dev/null 2>&1 || rc=$?
  [ "$rc" -ne 0 ] || note "$label accepted BENCHMARK_HOST_CLASS=Dedicated"
  [ ! -s "$toolrec" ] || note "$label measured before refusing the host class: $(paste -sd';' "$toolrec")"
}
hcout="$(mktemp -d)"
for target in benchmark-crud-all benchmark-footprint benchmark-micro benchmark-micro-ablation benchmark-metadata; do
  refused_before_measuring "make $target" make "$target" OUT_DIR="$hcout"
done
refused_before_measuring "make benchmark-crud" make benchmark-crud OUT_DIR="$hcout" TARGET_URL=http://unused FRAMEWORK=x
refused_before_measuring "run-crud-all.sh" env OUT_DIR="$hcout" bash benchmarks/scripts/run-crud-all.sh
refused_before_measuring "footprint-all.sh" env OUT_DIR="$hcout" bash benchmarks/scripts/footprint-all.sh
[ -z "$(ls -A "$hcout")" ] || note "a refused stage wrote to OUT_DIR: $(ls -A "$hcout")"
rm -rf "$hcout" "$stubdir/docker" "$stubdir/go"

# ---- 6. benchmark-smoke (issue #141 §11): build all six images, run the
#         containerized harness for ALL SIX with a tiny deterministic seed and
#         tiny load, into a THROWAWAY dir (never results/latest) ----
# A recording stub for the recursive $(MAKE) that captures both the argv and the
# BENCH_SEED_* env the recipe exports, and a docker stub so `compose build`
# no-ops.
smokerec="$stubdir/smokelog"; : > "$smokerec"
printf '#!/usr/bin/env bash\necho "$* SEED=${BENCH_SEED_USERS:-unset}/${BENCH_SEED_PROJECTS:-unset}" >> "%s"\nexit 0\n' "$smokerec" > "$stubdir/argmake"
dockrec="$stubdir/dockerlog"; : > "$dockrec"
printf '#!/usr/bin/env bash\necho "$*" >> "%s"\nexit 0\n' "$dockrec" > "$stubdir/docker"
chmod +x "$stubdir/argmake" "$stubdir/docker"

PATH="$stubdir:$PATH" make benchmark-smoke MAKE="$stubdir/argmake" >/dev/null 2>&1 \
  || note "benchmark-smoke failed on a clean stub run"

# Built all six: `docker compose … build` with NO trailing service name.
grep -qE 'compose .*-f benchmarks/compose.yml build$' "$dockrec" \
  || note "benchmark-smoke did not build all app images: $(cat "$dockrec")"

smokeargs="$(cat "$smokerec")"
# All six apps, tiny load params, and a small deterministic seed passed through.
for app in gin-gorm gombit django rails laravel nestjs; do
  case "$smokeargs" in *"$app"*) : ;; *) note "benchmark-smoke did not run app '$app': $smokeargs" ;; esac
done
case "$smokeargs" in
  *"benchmark-crud-all"*"CONCURRENCY=1"*"TRIALS=1"*"DURATION_SECONDS=3"*"WARMUP_SECONDS=1"*) : ;;
  *) note "benchmark-smoke did not run crud-all with the tiny params: $smokeargs" ;;
esac
case "$smokeargs" in
  *"SEED=20/100"*) : ;;
  *) note "benchmark-smoke did not export the small deterministic seed (BENCH_SEED_*): $smokeargs" ;;
esac
case "$smokeargs" in
  *"OUT_DIR=benchmarks/results/latest"*) note "benchmark-smoke targeted results/latest, not a throwaway dir: $smokeargs" ;;
  *"OUT_DIR="*) : ;;
  *) note "benchmark-smoke passed no OUT_DIR (would default to results/latest): $smokeargs" ;;
esac

if [ "$fail" -ne 0 ]; then
  echo "benchmark-target_test: FAILED" >&2
  exit 1
fi
echo "benchmark-target_test: default-goal, no-prereq composition, ordered stages, pin propagation, fail-closed, host-class checks (composite, per stage, scripts), and smoke (build-6/run-6/small-seed/throwaway) all pass"
