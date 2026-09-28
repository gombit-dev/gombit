#!/usr/bin/env bash
# Runs the stochastic resilience suite (make test-chaos's tests) for the
# nightly/dispatch chaos workflow and collects everything needed to
# reproduce a failure locally (CHAOS-10):
#
#   $CHAOS_ARTIFACTS/test.json        go test -json output (race reports included)
#   $CHAOS_ARTIFACTS/go-stderr.txt    the go command's own stderr (build errors)
#   $CHAOS_ARTIFACTS/reports/*.txt    one CHAOS FAILURE block per failed iteration
#   $CHAOS_ARTIFACTS/environment.txt  the CHAOS_* settings in effect
#   $CHAOS_ARTIFACTS/summary.md       seed, verdict, the command that replays the
#                                     whole run, and each failure's replay command
#                                     (also appended to $GITHUB_STEP_SUMMARY)
#
# Every file describes this run only: the directory's previous contents are
# removed first. It exits with the suite's status. CHAOS_SEED, CHAOS_SCENARIO,
# CHAOS_ITERATION(S), and CHAOS_POSTGRES_DSN pass through to the suite; an
# empty value means "not set".
#
# The replay commands carry every setting that changes what the suite draws
# or runs, CHAOS_POSTGRES_DSN included, quoted, so they rerun this run as it
# was. The DSN is written as given: point this script only at a throwaway
# test database (the chaos workflow's is an ephemeral service container).
# Plain bash 3.2+ (macOS) and POSIX tools.
#
#   CHAOS_ARTIFACTS=chaos-artifacts bash scripts/chaos-run.sh
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

artifacts="${CHAOS_ARTIFACTS:-chaos-artifacts}"
# Only this script's own files: CHAOS_ARTIFACTS may name a directory that
# holds other things.
rm -rf "$artifacts/reports"
rm -f "$artifacts/test.json" "$artifacts/go-stderr.txt" "$artifacts/environment.txt" "$artifacts/summary.md"
mkdir -p "$artifacts/reports"
export CHAOS_REPORT_DIR="$artifacts/reports"

# Workflow inputs arrive as empty strings when not given: unset them, so the
# suite chooses (and prints) a seed and runs every scenario.
for name in CHAOS_SEED CHAOS_SCENARIO CHAOS_ITERATION CHAOS_ITERATIONS CHAOS_POSTGRES_DSN; do
  if [ -z "${!name:-}" ]; then
    unset "$name"
  fi
done

# quote single-quotes a value for a shell command line.
quote() { printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"; }

# The settings that decide what the suite runs and draws, as a command
# prefix: the Postgres configuration always (a replay without it would run
# on SQLite), and the iteration count for a whole-run replay (the suite's
# default is 20; the nightly runs more).
pg_prefix="CHAOS_POSTGRES=0"
if [ -n "${CHAOS_POSTGRES_DSN:-}" ]; then
  pg_prefix="CHAOS_POSTGRES_DSN=$(quote "$CHAOS_POSTGRES_DSN") CHAOS_POSTGRES=1"
fi
run_prefix="$pg_prefix CHAOS_ITERATIONS=${CHAOS_ITERATIONS:-20}"
if [ -n "${CHAOS_SCENARIO:-}" ]; then
  run_prefix="$run_prefix CHAOS_SCENARIO=$(quote "$CHAOS_SCENARIO")"
fi
if [ -n "${CHAOS_ITERATION:-}" ]; then
  run_prefix="$run_prefix CHAOS_ITERATION=$(quote "$CHAOS_ITERATION")"
fi

{
  echo "# chaos run environment"
  for name in CHAOS_SEED CHAOS_SCENARIO CHAOS_ITERATION CHAOS_ITERATIONS CHAOS_POSTGRES_DSN; do
    if [ -n "${!name:-}" ]; then
      echo "$name=${!name}"
    fi
  done
  echo "go=$(go version 2>/dev/null || echo unknown)"
} >"$artifacts/environment.txt"

go test -tags chaos -race -count=1 -timeout "${CHAOS_TIMEOUT:-60m}" -json ./internal/chaos >"$artifacts/test.json" 2>"$artifacts/go-stderr.txt"
status=$?

# The suite prints CHAOS_SEED=<n> before anything else; in -json output it
# is the first "output" event carrying it.
seed="$(grep -oE '"Output":"CHAOS_SEED=[0-9]+' "$artifacts/test.json" | head -n1 | sed -E 's/.*CHAOS_SEED=//')"

{
  echo "## Chaos run"
  echo
  if [ -n "$seed" ]; then
    echo "Seed: \`$seed\`. Replay the whole run:"
    echo
    echo "    $run_prefix CHAOS_SEED=$seed make test-chaos"
  else
    echo "Seed: **not found**: the suite did not start (see go-stderr.txt and test.json)."
  fi
  echo
  if [ "$status" -eq 0 ]; then
    echo "Result: **passed**"
  else
    echo "Result: **failed** (exit $status)"
    reports=("$artifacts"/reports/*.txt)
    if [ -e "${reports[0]}" ]; then
      echo
      echo "Replay each failure locally:"
      echo
      # In iteration order (iter-2 before iter-14), then by scenario; the
      # iteration is the number in the report's name (POSIX sort, no -V).
      for report in "${reports[@]}"; do
        iteration="$(basename "$report" .txt | sed -E 's/.*-iter-([0-9]+)$/\1/')"
        printf '%s\t%s\n' "$iteration" "$report"
      done | sort -t "$(printf '\t')" -k1,1n -k2,2 | while IFS="$(printf '\t')" read -r _ report; do
        # The report pins CHAOS_POSTGRES; add the DSN it needs (never in
        # the report itself).
        command="$(grep -A1 '^replay:' "$report" | tail -n1 | sed -E 's/^ +//')"
        if [ -n "${CHAOS_POSTGRES_DSN:-}" ]; then
          command="CHAOS_POSTGRES_DSN=$(quote "$CHAOS_POSTGRES_DSN") $command"
        fi
        echo "    $command"
      done
    else
      echo
      echo "No scenario reported a failure: the run itself failed (build, timeout, or panic); see go-stderr.txt and test.json."
    fi
  fi
} >"$artifacts/summary.md"

cat "$artifacts/summary.md"
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  cat "$artifacts/summary.md" >>"$GITHUB_STEP_SUMMARY"
fi
exit "$status"
