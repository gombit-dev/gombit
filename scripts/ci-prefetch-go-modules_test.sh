#!/usr/bin/env bash
# Tests for ci-prefetch-go-modules.sh. Stubs `go` and `sleep` on PATH so the
# retry/backoff control flow and the scaffold warm-up are exercised
# hermetically (no network, no real waiting, no real toolchain) and asserts
# exactly what CI issue #208 requires: bounded retries only around dependency
# acquisition, the documented attempt*5s backoff, a failure message when all
# attempts are exhausted, and — under --with-scaffold — that the warm-up tidies
# a generated app pointed at *this* checkout. `go test` is never invoked by the
# script under test: a real test failure must stay visible, never silently
# retried.
#
#   bash scripts/ci-prefetch-go-modules_test.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

fail=0
note() { echo "FAIL: $*" >&2; fail=1; }

SCRIPT="scripts/ci-prefetch-go-modules.sh"

# ---- static: the prefetch script never wraps `go test` in its own retry ----
# Only dependency acquisition may be retried; a genuine test failure must
# fail immediately and visibly. Comment-only mentions (e.g. this file's own
# explanatory header) don't count — only an actual invocation does.
if [ -f "$SCRIPT" ] && grep -vE '^\s*#' "$SCRIPT" | grep -qE 'go test'; then
  note "$SCRIPT invokes 'go test' — only dependency acquisition may be retried"
fi

# ---- fake `go` + `sleep` on PATH, driven by env, for hermetic retry tests ----
fakebin="$(mktemp -d)"
# The fake `go mod download all` below deliberately dirties go.sum, the way the
# real one does. Snapshot it so a broken restore in the script under test fails
# the assertion instead of leaving the checkout modified.
gosum_snapshot="$(mktemp)"
cp go.sum "$gosum_snapshot"
trap 'cp "$gosum_snapshot" go.sum; rm -rf "$fakebin" "$gosum_snapshot"' EXIT

# The fake `go` covers the three subcommands the script drives: the module
# download it retries, the build that produces the scaffolding binary (stubbed
# out as a script that writes the generated go.mod), and the tidy inside the
# generated app — which snapshots that go.mod so the assertions below can see
# it after the script has cleaned up its temp dir.
cat > "$fakebin/go" <<'EOF'
#!/usr/bin/env bash
echo "$*" >> "$CALL_LOG"
case "$1 ${2:-}" in
"mod download")
  # `go mod download all` really does rewrite go.sum; the script is expected
  # to put it back, so give it something unmistakable to put back.
  echo "$GOSUM_MARKER" >> go.sum
  count=$(( $(cat "$COUNT_FILE" 2>/dev/null || echo 0) + 1 ))
  echo "$count" > "$COUNT_FILE"
  if [ "$count" -ge "$SUCCEED_ON_ATTEMPT" ]; then
    exit 0
  fi
  echo "go: mod download all: fake network failure" >&2
  exit 1
  ;;
"mod tidy")
  cp go.mod "$TIDY_GOMOD"
  count=$(( $(cat "$TIDY_COUNT_FILE" 2>/dev/null || echo 0) + 1 ))
  echo "$count" > "$TIDY_COUNT_FILE"
  if [ "$count" -ge "$TIDY_SUCCEED_ON_ATTEMPT" ]; then
    exit 0
  fi
  echo "go: mod tidy: fake network failure" >&2
  exit 1
  ;;
"build -o")
  cat > "$3" <<'GOMBIT'
#!/usr/bin/env bash
echo "$*" >> "$GOMBIT_LOG"
mkdir -p demo
printf 'module github.com/example/demo\n\ngo 1.26.0\n\nrequire github.com/gombit-dev/gombit v0.0.0\n' > demo/go.mod
GOMBIT
  chmod +x "$3"
  exit 0
  ;;
esac
exit 0
EOF
chmod +x "$fakebin/go"

cat > "$fakebin/sleep" <<'EOF'
#!/usr/bin/env bash
echo "$*" >> "$SLEEP_LOG"
exit 0
EOF
chmod +x "$fakebin/sleep"

# Runs the script under test and sets $rc plus the count_file/call_log/
# sleep_log globals below — NOT via $(...), so those assignments survive in
# this shell instead of vanishing with a subshell. $1 is the download attempt
# that succeeds; $2 the tidy attempt that succeeds; the rest are the script's
# own flags.
run_prefetch() {
  local succeed_on="$1" tidy_succeed_on="$2"
  shift 2
  gosum_marker="# fake-prefetch-marker-$RANDOM"
  count_file="$(mktemp)"; call_log="$(mktemp)"; sleep_log="$(mktemp)"
  tidy_count_file="$(mktemp)"; tidy_gomod="$(mktemp)"; gombit_log="$(mktemp)"
  rm -f "$count_file" "$call_log" "$sleep_log" "$tidy_count_file" "$tidy_gomod" "$gombit_log"
  rc=0
  PATH="$fakebin:$PATH" \
    COUNT_FILE="$count_file" CALL_LOG="$call_log" SLEEP_LOG="$sleep_log" \
    TIDY_COUNT_FILE="$tidy_count_file" TIDY_GOMOD="$tidy_gomod" \
    GOMBIT_LOG="$gombit_log" GOSUM_MARKER="$gosum_marker" \
    SUCCEED_ON_ATTEMPT="$succeed_on" TIDY_SUCCEED_ON_ATTEMPT="$tidy_succeed_on" \
    bash "$SCRIPT" "$@" >"$call_log.stdout" 2>"$call_log.stderr" || rc=$?
}

count_of() { [ -f "$count_file" ] && cat "$count_file" || echo 0; }
tidy_count_of() { [ -f "$tidy_count_file" ] && cat "$tidy_count_file" || echo 0; }
sleep_calls() { [ -f "$sleep_log" ] && cat "$sleep_log" || true; }

# ---- 1. immediate success: no retry, no sleep ----
run_prefetch 1 1
[ "$rc" -eq 0 ]              || note "immediate success: exit=$rc, want 0"
[ "$(count_of)" = "1" ]      || note "immediate success: go invoked $(count_of)x, want 1"
[ -z "$(sleep_calls)" ]      || note "immediate success: slept ($(sleep_calls | tr '\n' ',')), want no retries"

# ---- 2. fails twice, succeeds on the 3rd: retried with attempt*5s backoff ----
run_prefetch 3 1
[ "$rc" -eq 0 ]                                || note "eventual success: exit=$rc, want 0"
[ "$(count_of)" = "3" ]                        || note "eventual success: go invoked $(count_of)x, want 3"
[ "$(sleep_calls)" = "$(printf '5\n10')" ]      || note "eventual success: slept ($(sleep_calls | tr '\n' ',')), want 5,10"

# ---- 3. fails all 3 attempts: exits 1, never a 4th try, no sleep after the last ----
run_prefetch 99 1
[ "$rc" -eq 1 ]                                || note "exhausted retries: exit=$rc, want 1"
[ "$(count_of)" = "3" ]                        || note "exhausted retries: go invoked $(count_of)x, want exactly 3 (no 4th attempt)"
[ "$(sleep_calls)" = "$(printf '5\n10')" ]      || note "exhausted retries: slept ($(sleep_calls | tr '\n' ',')), want 5,10 (no sleep after the final failure)"
grep -qF "failed to download Go dependencies after 3 attempts" "$call_log.stderr" \
  || note "exhausted retries: stderr did not report the exhausted-attempts message"

# ---- 4. without --with-scaffold the warm-up does not run at all ----
# The jobs that never build a generated app (conformance, the Postgres/MySQL
# migration suites) must not pay for it.
run_prefetch 1 1
[ "$rc" -eq 0 ]                         || note "no scaffold: exit=$rc, want 0"
[ "$(tidy_count_of)" = "0" ]            || note "no scaffold: ran 'go mod tidy' $(tidy_count_of)x, want 0"
if grep -q 'build -o' "$call_log"; then
  note "no scaffold: built the gombit binary, want no scaffold work"
fi

# ---- 5. --with-scaffold tidies a generated app pinned to this checkout ----
# The local replace is the point: without it the warm-up would resolve a
# published gombit release and cache the wrong module graph.
run_prefetch 1 1 --with-scaffold
[ "$rc" -eq 0 ]                                 || note "scaffold: exit=$rc, want 0"
[ "$(count_of)" = "1" ]                         || note "scaffold: 'go mod download all' ran $(count_of)x, want 1"
[ "$(tidy_count_of)" = "1" ]                    || note "scaffold: 'go mod tidy' ran $(tidy_count_of)x, want 1"
grep -q 'build -o' "$call_log"                  || note "scaffold: never built the gombit binary"
grep -qF 'new demo --module github.com/example/demo --skip-tidy' "$gombit_log" \
  || note "scaffold: gombit was not asked to scaffold with --skip-tidy (got: $(cat "$gombit_log" 2>/dev/null))"
grep -qF "replace github.com/gombit-dev/gombit => $ROOT" "$tidy_gomod" \
  || note "scaffold: tidied go.mod does not replace the framework with this checkout"

# ---- 6. a flaky warm-up is retried on the same bounded schedule ----
run_prefetch 1 3 --with-scaffold
[ "$rc" -eq 0 ]                                || note "flaky scaffold: exit=$rc, want 0"
[ "$(tidy_count_of)" = "3" ]                   || note "flaky scaffold: 'go mod tidy' ran $(tidy_count_of)x, want 3"
[ "$(sleep_calls)" = "$(printf '5\n10')" ]      || note "flaky scaffold: slept ($(sleep_calls | tr '\n' ',')), want 5,10"

# ---- 7. an exhausted warm-up fails the job, named distinctly from the download ----
run_prefetch 1 99 --with-scaffold
[ "$rc" -eq 1 ]                                || note "scaffold exhausted: exit=$rc, want 1"
[ "$(tidy_count_of)" = "3" ]                   || note "scaffold exhausted: 'go mod tidy' ran $(tidy_count_of)x, want exactly 3"
grep -qF "failed to warm the generated-app module closure after 3 attempts" "$call_log.stderr" \
  || note "scaffold exhausted: stderr did not name the warm-up as what failed"

# ---- 8. an unknown flag is rejected rather than silently ignored ----
# A typo'd flag must not quietly skip the warm-up and leave the job to fail
# later inside a test with an opaque GOPROXY=off error.
run_prefetch 1 1 --with-scafold
[ "$rc" -eq 2 ]                 || note "unknown flag: exit=$rc, want 2"
[ "$(count_of)" = "0" ]         || note "unknown flag: downloaded anyway ($(count_of)x), want no work"

# ---- 9. the prefetch leaves go.sum exactly as it found it ----
# The checksums `go mod download all` records for the whole module graph are
# noise `go mod tidy` deletes again; committing them once already produced a
# 473-line unrelated diff. Only the module cache should survive this script.
run_prefetch 1 1
[ "$rc" -eq 0 ]                     || note "go.sum restore: exit=$rc, want 0"
if grep -qF "$gosum_marker" go.sum; then
  note "go.sum restore: the prefetch left go.sum modified"
fi

# ...including when it gives up, so a failed bootstrap doesn't leave a dirty
# tree behind either.
run_prefetch 99 1
[ "$rc" -eq 1 ]                     || note "go.sum restore on failure: exit=$rc, want 1"
if grep -qF "$gosum_marker" go.sum; then
  note "go.sum restore on failure: the prefetch left go.sum modified"
fi

if [ "$fail" -ne 0 ]; then
  echo "ci-prefetch-go-modules_test: FAILED" >&2
  exit 1
fi
echo "ci-prefetch-go-modules_test: retry/backoff, exhausted retries, scaffold warm-up, flag handling, and go.sum restore all pass"
