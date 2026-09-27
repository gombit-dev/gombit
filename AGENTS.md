# AGENTS.md

Gombit is a Django-for-Go full-stack framework: Go backend (Gin + Huma + GORM),
a generated React+TypeScript frontend, Atlas-backed migrations. Module path
`github.com/gombit-dev/gombit`.

## Current state

This repo has completed M0–M5, **ADMIN-1 through ADMIN-3**, the **REL-1..9**
release-readiness track, and **BENCH-1** (the benchmark suite). It has typed
`config.Config`, `framework.App` lifecycle and routing, Huma contract/OpenAPI/
TypeScript client generation, Atlas-backed migrations, a Cobra `gombit`
command tree (`new`, `dev`, `build --embed`, `make resource`, `make command`,
`db`, `openapi`, `client`), and a Vite + React + TypeScript minimal skeleton
(router, generated client, React Hook Form). Bearer login (M5-2), cookie/CSRF
(M5-3), the MUI preset (M5-4, `--ui mui`), and optional `gombit build --embed`
(M5-5) are in. **ADMIN-0 (ADR-013) is accepted**, **ADMIN-1** ships
`admin.Register`, `GET /api/v1/admin/meta`, and the generic
`/api/v1/admin/resources/{slug}` data plane, **ADMIN-2** ships the
framework-owned SPA under `/admin/` (`internal/adminui`, cookie-only embed),
and **ADMIN-3** adds direct/group permission enforcement with a superuser
bypass. **REL-1..9** covers release readiness: the README rewrite with badges
and quickstart, contributor and community-health files, issue templates,
`gombit version` plus the release workflow, the installation guide, the
tutorial and its example app, the docs index/changelog/release runbook, and
scaffolded apps resolving a published framework version. **BENCH-1** adds the
`benchmarks/` tree — Go micro-benchmarks (`benchmarks/micro`), the operational
footprint/cold-start harness, k6 load workloads, the canonical results
snapshot behind the README performance table, and a manual `benchmarks.yml`
workflow — plus the per-PR `benchmark-report-drift` and `benchmark-smoke`
gates in `ci.yml`. The **JOBS-0** background-jobs epic has started: `jobs` holds
the job contract (typed jobs, registry, envelope), the sync, memory, and
Redis queue drivers behind `framework.App.Jobs()`, the worker
(`./server worker`, `gombit worker`), retry policies (attempts, backoff,
timeouts, permanent failures), and delayed dispatch (`jobs.Delay`,
`jobs.At`). Other M6 batteries are not here yet.
Don't assume generated apps are committed in-tree; `gombit new` writes them
on demand. Check `git log` / `ls` before describing "how the code works."

## Source of truth

- `docs/GOMBIT_BUILD_PLAN.md` is authoritative for scope, decisions, and the
  issue backlog (§4). It wins over the design doc on any conflict.
- `docs/GO_FULLSTACK_FRAMEWORK_DESIGN.md` is rationale/prose only, cited by
  backlog entries (e.g. "draft §41") for context — never a source of
  additional scope on its own.
- GitHub issues, one per §4 backlog entry, titled `[ID] ...` (e.g. `[M1-2]
  framework.App + lifecycle + hooks`), are the unit of work. Milestones run
  `M0 spike` → `M1 runtime` → `M2 migrations` → `M3 contract` → `M4 cli` →
  `M5 frontend-auth` → `M6 admin` → `post-v0.1`. Don't start an issue whose
  "Depends on #N" is still open.

## Locked architecture decisions (build plan §1-§3 — do not re-litigate)

- **Contract layer:** Huma-typed handlers over Gin are the source of truth for
  the API contract (OpenAPI 3.1 emitted, not hand-written). Raw `*gin.Engine`
  stays reachable via `app.Router()` as a first-class, tested escape hatch.
- **App layout (generated apps):** feature-package under `internal/<feature>/`
  (model, handler, routes; `service.go`/`repo.go` only with `--service`/
  `--repo`). Never Laravel-style `app/controllers`, `app/models`.
- **Migrations:** wrap `ariga.io/atlas-provider-gorm` (Program Mode) +
  `atlas migrate diff`. Never hand-roll a migration DSL.
- **Auth:** Bearer JWT (access token in memory, never `localStorage`) is the
  v0.1 API default; session/cookie is first-class, not a preset, and is a
  hard prerequisite of the admin milestone.
- **Response envelope (D10):** success `{"data": ..., "meta"?: ...}`, error
  `{"error": {code, message, fields?, request_id}}`. Don't invent another
  shape.
- **Generators:** idempotent and additive, with `--dry-run`/`--force`. Go
  source is modified via `go/ast`/`go/format` only — never regex — and never
  overwrites user-owned files.
- **CLI:** Cobra (`spf13/cobra`) is the command framework (D13 / ADR-014).
  Nested families and M4-7 app-registered management commands use Cobra
  `AddCommand`. Do not introduce Kong or a parallel hand-rolled router as the
  long-term CLI architecture; pre-M4 stdlib `flag` is temporary until M4-1.
- **Admin (ADR-013):** runtime generic admin over an explicit registry + Huma
  introspection API; framework-owned React app under `/admin/`. Never
  `--admin` generated pages, never request-time reflection over GORM models.

## Agent working agreement (definition of done — build plan §5)

A change is not done unless:

1. New behavior has tests; DB-touching changes pass the SQLite + PostgreSQL + MySQL
   matrix.
2. Stable features ship docs and appear in an example app.
3. Extraction from `golang-rest-api-template` preserves contracts — refactor
   and move, don't rewrite code that already passes its tests.
4. Any API change regenerates the OpenAPI doc + TS client in the same PR.
5. No secrets in generated frontend source; `VITE_*` is treated as public.
6. Scope stays inside the issue's milestone. If work starts pulling in an M6
   "battery" (jobs, events, scheduler, mail, storage, gRPC, multi-tenancy,
   i18n), stop and split it out — v0.1 is one CRUD loop, nothing more.
7. The PR links its issue and states which acceptance criteria it satisfies.

## Working conventions

- One issue → one PR where practical; reference the issue number in the PR.
- Every PR must follow `.github/pull_request_template.md` (Summary, linked
  issue, acceptance criteria, scope notes, validation commands, working
  agreement checklist). Don't strip sections to shorten the PR body — mark
  items N/A with a reason instead.
- Conventional commit prefixes (`feat:`, `fix:`, `docs:`, `chore:`) are fine;
  the build plan doesn't mandate anything stricter.
- Don't create milestones/labels beyond build plan §6, and don't create
  issues beyond §4 backlog entries, without asking first.
- If something looks missing from the backlog, flag it — don't silently add
  scope.

## Code review

Before opening or merging a PR, review the diff as an adversarial senior
reviewer: the implementation must satisfy its claimed contract and the
working agreement above. Personality is presentation; technical analysis
comes first. Do not invent findings. In Claude Code, run the project
`code-review` skill (`.claude/skills/code-review/SKILL.md`); in Cursor, run
the project `code-review` skill (`.cursor/skills/code-review/SKILL.md`).

## Cursor skills

Project skills live in `.cursor/skills/` and encode the workflows above.
Invoke them with `/create-feature`, `/code-review`, or `/bugfix`, or by
asking in those terms.

- **create-feature** — implement one backlog issue or new capability.
  Place code per the generate-vs-runtime rule and feature-package layout;
  finish only when the working agreement is met.
- **code-review** — adversarial review of a diff/PR against this file,
  build plan §5, and the change's claimed contract. Use it before opening
  or merging a PR. It overrides generic or agreeable review habits for
  this repo.
- **bugfix** — reproduce, add a failing test, fix the root cause only,
  then verify. Do not use it for new features.
