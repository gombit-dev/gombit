#!/usr/bin/env bash
# Runs the stochastic resilience suite (make test-chaos's tests) for the
# nightly/dispatch chaos workflow and collects everything needed to
# reproduce a failure locally (CHAOS-10):
#
#   $CHAOS_ARTIFACTS/test.json        go test -json output (race reports included)
#   $CHAOS_ARTIFACTS/reports/*.txt    one CHAOS FAILURE block per failed iteration
#   $CHAOS_ARTIFACTS/environment.txt  the CHAOS_* settings in effect (DSNs masked)
#   $CHAOS_ARTIFACTS/summary.md       seed, verdict, and each failure's replay
#                                     command (also appended to $GITHUB_STEP_SUMMARY)
#
# It exits with the suite's status. CHAOS_SEED, CHAOS_SCENARIO,
# CHAOS_ITERATION(S), and CHAOS_POSTGRES_DSN pass through to the suite; an
# empty value means "not set".
#
#   CHAOS_ARTIFACTS=chaos-artifacts bash scripts/chaos-run.sh
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

artifacts="${CHAOS_ARTIFACTS:-chaos-artifacts}"
mkdir -p "$artifacts/reports"
export CHAOS_REPORT_DIR="$artifacts/reports"

# Workflow inputs arrive as empty strings when not given: unset them, so the
# suite chooses (and prints) a seed and runs every scenario.
for name in CHAOS_SEED CHAOS_SCENARIO CHAOS_ITERATION CHAOS_ITERATIONS CHAOS_POSTGRES_DSN; do
  if [ -z "${!name:-}" ]; then
    unset "$name"
  fi
done

{
  echo "# chaos run environment"
  for name in CHAOS_SEED CHAOS_SCENARIO CHAOS_ITERATION CHAOS_ITERATIONS CHAOS_POSTGRES_DSN; do
    if [ -n "${!name:-}" ]; then
      value="${!name}"
      if [ "$name" = CHAOS_POSTGRES_DSN ]; then
        value="(set, masked)"
      fi
      echo "$name=$value"
    fi
  done
  echo "go=$(go version 2>/dev/null || echo unknown)"
} >"$artifacts/environment.txt"

go test -tags chaos -race -count=1 -timeout "${CHAOS_TIMEOUT:-60m}" -json ./internal/chaos >"$artifacts/test.json"
status=$?

# The suite prints CHAOS_SEED=<n> before anything else; in -json output it
# is the first "output" event carrying it.
seed="$(grep -oE '"Output":"CHAOS_SEED=[0-9]+' "$artifacts/test.json" | head -n1 | sed -E 's/.*CHAOS_SEED=//')"

{
  echo "## Chaos run"
  echo
  if [ -n "$seed" ]; then
    echo "Seed: \`$seed\` (replay the whole run: \`CHAOS_SEED=$seed make test-chaos\`)"
  else
    echo "Seed: **not found**: the suite did not start (see test.json)."
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
      # In iteration order (iter-2 before iter-14).
      printf '%s\n' "${reports[@]}" | sort -V | while IFS= read -r report; do
        grep -A1 '^replay:' "$report" | tail -n1 | sed -E 's/^ +/    /'
      done
    else
      echo
      echo "No scenario reported a failure: the run itself failed (build, timeout, or panic); see test.json."
    fi
  fi
} >"$artifacts/summary.md"

cat "$artifacts/summary.md"
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  cat "$artifacts/summary.md" >>"$GITHUB_STEP_SUMMARY"
fi
exit "$status"
