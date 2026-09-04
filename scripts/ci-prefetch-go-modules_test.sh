#!/usr/bin/env bash
# Tests for ci-prefetch-go-modules.sh. Stubs `go` and `sleep` on PATH so the
# retry/backoff control flow is exercised hermetically (no network, no real
# waiting) and asserts exactly what CI issue #208 requires: bounded retries
# only around dependency acquisition, the documented attempt*5s backoff, and
# a failure message when all attempts are exhausted. `go test` is never
# invoked by the script under test — a real test failure must stay visible,
# never silently retried.
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
trap 'rm -rf "$fakebin"' EXIT

cat > "$fakebin/go" <<'EOF'
#!/usr/bin/env bash
echo "$*" >> "$CALL_LOG"
if [ "$*" = "mod download all" ]; then
  count=$(( $(cat "$COUNT_FILE" 2>/dev/null || echo 0) + 1 ))
  echo "$count" > "$COUNT_FILE"
  if [ "$count" -ge "$SUCCEED_ON_ATTEMPT" ]; then
    exit 0
  fi
  echo "go: mod download all: fake network failure" >&2
  exit 1
fi
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
# this shell instead of vanishing with a subshell.
run_prefetch() {
  count_file="$(mktemp)"; call_log="$(mktemp)"; sleep_log="$(mktemp)"
  rm -f "$count_file" "$call_log" "$sleep_log"
  rc=0
  PATH="$fakebin:$PATH" \
    COUNT_FILE="$count_file" CALL_LOG="$call_log" SLEEP_LOG="$sleep_log" \
    SUCCEED_ON_ATTEMPT="$1" \
    bash "$SCRIPT" >"$call_log.stdout" 2>"$call_log.stderr" || rc=$?
}

count_of() { [ -f "$count_file" ] && cat "$count_file" || echo 0; }
sleep_calls() { [ -f "$sleep_log" ] && cat "$sleep_log" || true; }

# ---- 1. immediate success: no retry, no sleep ----
run_prefetch 1
[ "$rc" -eq 0 ]              || note "immediate success: exit=$rc, want 0"
[ "$(count_of)" = "1" ]      || note "immediate success: go invoked $(count_of)x, want 1"
[ -z "$(sleep_calls)" ]      || note "immediate success: slept ($(sleep_calls | tr '\n' ',')), want no retries"

# ---- 2. fails twice, succeeds on the 3rd: retried with attempt*5s backoff ----
run_prefetch 3
[ "$rc" -eq 0 ]                                || note "eventual success: exit=$rc, want 0"
[ "$(count_of)" = "3" ]                        || note "eventual success: go invoked $(count_of)x, want 3"
[ "$(sleep_calls)" = "$(printf '5\n10')" ]      || note "eventual success: slept ($(sleep_calls | tr '\n' ',')), want 5,10"

# ---- 3. fails all 3 attempts: exits 1, never a 4th try, no sleep after the last ----
run_prefetch 99
[ "$rc" -eq 1 ]                                || note "exhausted retries: exit=$rc, want 1"
[ "$(count_of)" = "3" ]                        || note "exhausted retries: go invoked $(count_of)x, want exactly 3 (no 4th attempt)"
[ "$(sleep_calls)" = "$(printf '5\n10')" ]      || note "exhausted retries: slept ($(sleep_calls | tr '\n' ',')), want 5,10 (no sleep after the final failure)"
grep -qF "failed to download Go dependencies after 3 attempts" "$call_log.stderr" \
  || note "exhausted retries: stderr did not report the exhausted-attempts message"

if [ "$fail" -ne 0 ]; then
  echo "ci-prefetch-go-modules_test: FAILED" >&2
  exit 1
fi
echo "ci-prefetch-go-modules_test: immediate success, eventual success with backoff, and exhausted-retries all pass"
