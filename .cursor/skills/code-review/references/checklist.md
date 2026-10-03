# Gombit review checklist

Walk only the sections that the diff touches. These are contracts to attack
during the adversarial review in [adversarial-review.md](adversarial-review.md),
not a substitute for tracing the change end-to-end and not a review template.

This file must stay aligned with `.claude/skills/code-review/references/checklist.md`.

## Working agreement (always)

- [ ] Linked issue exists and the PR states which AC it satisfies
- [ ] Scope is inside that issue's milestone; no unplanned battery (events, scheduler, mail, storage, gRPC, multi-tenancy, i18n)
- [ ] No new issues, labels, or milestones created without asking (labels/milestones per build plan §6)
- [ ] Locked decisions were not reopened

## Architecture

- [ ] Public HTTP contract is Huma-typed handlers, not comment annotations or a hand-written OpenAPI file
- [ ] Raw Gin escape hatch (`app.Router()`) is tested when used, and those routes are absent from OpenAPI
- [ ] Generated app code is feature-package (`internal/<feature>/`), not `app/controllers`
- [ ] Thin handler-over-GORM default; no unsolicited `service.go` / `repo.go`
- [ ] Behavior that could live in the runtime was not generated into user-owned files
- [ ] `*gin.Engine` and `*gorm.DB` remain reachable; no universal ORM interface
- [ ] Cache uses value semantics (`Get/Set/Delete/Increment`), not leaked go-redis types
- [ ] `os.Getenv` stays in the typed config boundary
- [ ] New or expanded CLI surface uses Cobra (D13 / ADR-014), not Kong or a
      parallel long-term hand-rolled router

## Generators and AST

- [ ] Go edits use `go/ast` + `go/format`, never regex
- [ ] Generator is idempotent; `--dry-run` and `--force` exist
- [ ] Re-running does not clobber user-owned files
- [ ] Created/modified files are printed
- [ ] Golden tests exist for generator changes (compile backend, typecheck frontend, idempotency)

## Contract and frontend

- [ ] Success/error bodies match D10 (`data`/`meta`, `error.{code,message,fields,request_id}`)
- [ ] Validation failures populate `error.fields`
- [ ] OpenAPI 3.1 + `openapi-typescript` / `openapi-fetch` client regenerated in this PR
- [ ] Access token is in-memory only — no `localStorage` / `sessionStorage` for secrets
- [ ] No secrets in generated frontend; `VITE_*` treated as public
- [ ] Default UI remains headless; MUI only behind `--ui mui`

## Persistence

- [ ] SQLite + PostgreSQL + MySQL all considered for DB-touching changes
- [ ] Migrations go through Atlas GORM provider + `atlas migrate diff`, not a new DSL
- [ ] Migration metadata is `version, name, batch, applied_at` (no checksums)
- [ ] `repository.New[T]` is runtime-only, not copied per model

## Auth and security

- [ ] Bearer JWT remains the API default; cookie/session is first-class when touched
- [ ] Cookie mode uses HttpOnly / Secure / SameSite + CSRF
- [ ] `X-API-Key` is not enabled by default for browser apps
- [ ] Production validation still fails loud for draft Appendix C cases
- [ ] Middleware order preserved if the HTTP stack moved (recovery → request ID → tracing → access log → proxy/IP → security headers → CORS → body limit → XSS HTML sanitization → timeout → rate limit → auth → app → handler)

## Go idiom (when Go exists)

- [ ] `gofmt` / `go test` / lint clean
- [ ] Context passed explicitly; not stored in structs
- [ ] Table-driven tests with `t.Run` and useful `t.Errorf` messages
- [ ] Exported identifiers have doc comments that start with the name
- [ ] Errors wrapped with enough context; no discarded `error`
- [ ] No reflection-based route/module discovery

## Extraction discipline

- [ ] Existing tests from the template still pass
- [ ] Contracts (JWT/RBAC, request ID, timeouts, rate limit, security headers, XSS HTML sanitization, metrics, probes, graceful shutdown) were moved, not rewritten
- [ ] If a "small extraction" became a rewrite, the review must block and ask for a discussion issue
