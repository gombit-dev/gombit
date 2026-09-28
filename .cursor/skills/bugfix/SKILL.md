---
name: bugfix
description: Reproduces, tests, and fixes a Gombit defect with a minimal, verified change. Use when the user asks to fix a bug, fix a failing test, chase a regression, or resolve an issue that is a defect rather than a new feature.
---

# Bugfix

Reproduce first, then fix the root cause only. Read `AGENTS.md` first. `AGENTS.md` and the ADRs in `docs/adr/` win on conflicts (`docs/GOMBIT_BUILD_PLAN.md` is the historical v0.1 plan).

## When not to use

- New capability or backlog feature → `create-feature`
- Reviewing someone else's fix → `code-review`
- Cannot observe the failure after a genuine reproduction attempt → stop and ask; do not "fix" a guessed bug

## Workflow

```
1. Understand
2. Reproduce
3. Failing test
4. Root cause
5. Minimal fix
6. Verify
7. Hand-off
```

### 1. Understand

- Capture expected vs actual, stack traces, request IDs, and the last good revision.
- If a GitHub issue exists, treat it as the ticket. If the defect is new, do **not** open a backlog-style `[M#]` issue unless asked — issues are created by the maintainer. Describe the bug in the PR instead.
- Check whether the code is an extraction from `golang-rest-api-template` / `crud-template-monorepo`. Prefer restoring the proven contract over rewriting the subsystem.

### 2. Reproduce

Confirm the failure with the smallest command or test that shows it.

| Kind | How |
| --- | --- |
| Unit / logic | `go test` the package (or a focused `go test -run`) |
| HTTP / contract | Handler test or a request against the running app; compare to D10 envelope + OpenAPI |
| DB | SQLite first, then PostgreSQL. Do not declare a dialect bug from one driver. |
| Frontend / auth | Trace token storage and the generated client. Access tokens must stay in memory. |
| Generator | Re-run the generator; check idempotency and whether user-owned files were overwritten |
| Docs-only / pre-code | Reproduce as a contradiction between AGENTS.md, the ADRs, and the file text |

If reproduction fails, report what was tried and stop.

### 3. Failing test

Write a regression test that fails **before** the fix. Run it and confirm the fail.

- Prefer table-driven cases with `t.Run` and messages that include input, got, and want ([Go Test Comments](https://go.dev/wiki/TestComments)).
- Contract bugs: assert the D10 shape (`data`/`meta` or `error.{code,message,fields,request_id}`).
- Generator bugs: golden + idempotency (second run is a no-op without `--force`).
- Do not "fix" the test to match broken behavior.

Until `go.mod` exists, the regression proof is a concrete before/after in the doc or fixture the bug lives in.

### 4. Root cause

Trace from the failing assertion to the responsible change. Classify:

- logic / validation
- contract drift (handler vs OpenAPI vs TS client)
- config / env leak (`os.Getenv` outside the config boundary)
- dialect (SQLite vs PostgreSQL)
- generator / AST edit
- auth / token storage
- extraction regression (moved code, changed contract)

Assess blast radius: other feature-packages, middleware order, migration history, generated clients.

### 5. Minimal fix

- Fix the root cause only. No drive-by refactors, no extra layers, no new batteries.
- Do not re-litigate locked decisions to "simplify" the fix.
- Extraction bugs: restore the old contract; do not take the chance to rewrite.
- API-visible fixes regenerate OpenAPI + the TS client in the same change.
- Generator fixes stay AST-based (`go/ast` / `go/format`), idempotent, and must not start clobbering user-owned files.
- Auth fixes must not move bearer tokens into `localStorage` / `sessionStorage`.
- If the real fix is a new battery or a new product decision, stop and flag it.

### 6. Verify

- The new test passes; neighboring tests still pass.
- DB-touching fixes: SQLite **and** PostgreSQL.
- Runtime extractions: extracted suite still green.
- Lint / `gofmt` clean when Go exists.
- Then run the `code-review` skill on the fix diff. That skill is an adversarial contract review (`# APPROVE` / `# COMMENT` / `# REQUEST CHANGES`), not a checklist dump.

### 7. Hand-off

- One bug per PR. Conventional prefix: `fix:`.
- PR body: root cause, what changed, regression test name, linked issue if any.
- State which AC (if this was a backlog bug) are now satisfied.

## Anti-patterns

- Fixing without a reproduced failure
- Shipping without a regression test
- Rewriting a passing extraction because the bug was nearby
- Editing Go with regex in a generator
- Changing the D10 envelope or inventing a parallel error shape
- Pulling events, mail, gRPC, or other unplanned battery work into the fix
