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
gates in `ci.yml`. Resources are **model-first** (ADR-016): `make resource`
scaffolds a user-owned model and `hooks.go`, and `gombit generate` derives the
generator-owned `dto.gen.go` / `handler.gen.go` (`--check` for drift). The
**SCHEMA** epic (#277) is in: `gombit db plan` classifies a change before a
migration is written and `makemigrations --allow` acknowledges destructive or
unsafe steps, `--rename-table` renames tables in place, `db lint` / `db repair`
/ `db check` validate the migration directory and the whole schema chain, and
deletion is physical (`database.Delete`, ADR-019). The **JOBS-0**
background-jobs epic is in: `jobs` holds
the job contract (typed jobs, registry, envelope), the sync, memory, and
Redis queue drivers behind `framework.App.Jobs()`, the worker
(`./server worker`, `gombit worker`), retry policies (attempts, backoff,
timeouts, permanent failures), delayed dispatch (`jobs.Delay`, `jobs.At`),
failed jobs (`gombit jobs failed|inspect|retry|forget|purge`), duplicate
handling (`jobs.Unique`, `jobs.UniqueFor`, `jobs.Once`), and observability
(`gombit_jobs_*` metrics, OpenTelemetry spans). The **STORAGE-0** epic
(#279) is complete: `storage` holds the object storage contract
(`storage.Storage`, portable keys, classified errors), `storage/storagetest`
the driver conformance suite, and `storage/local` (hash-addressed files,
atomic writes), `storage/memory`, and `storage/s3` (S3-compatible, streaming
multipart) the drivers behind `framework.App.Storage()` (`GOMBIT_STORAGE_DRIVER`,
local by default), `storage/upload` the upload helpers (size limits,
content-detected type policy, generated keys), and public and signed URLs
(visibility by key prefix; presigned on S3, `storage/presign` HMAC URLs
served at `/_storage` for local and memory), and direct uploads
(`storage.DirectUploader`, `upload.Authorize` / `upload.Confirm`), and
cleanup semantics (`storage/claims`: a `storage_claims` table of
pending/promoting/held/deleting keys moved by conditional updates, with
leases and tombstones, so cleanup never deletes a file a record holds and
no late upload leaves an orphan; direct uploads are staged under
`_staging/` (a reserved namespace) and promoted by `upload.Confirm`
with a fenceable `storage.PreparePublish`/`Publish`/`Fence`;
`storage.Lister` enumerates objects), and
storage-backed model fields (`file` / `image` kinds, `types.File` /
`types.Image`, runtime `storage/filefield`; MODEL-8) with their admin
widgets (upload grants, previews, cleanup after commit; STORAGE-8). The
**UPGRADE-0** epic (#281) has started: `upgrade` detects an app's upgrade
baseline (framework version from `go.mod`, scaffold version from the
`gombit:` block `gombit new` writes to `gombit.yaml`, scaffold 0 for older
apps), shown and recorded by `gombit upgrade baseline` (UPGRADE-1), and the
compatibility manifest `upgrade/manifest.yaml` (every release's changes,
classified manual/automatic/informational; `Manifest.Path` refuses unlisted
versions), the source of `gombit upgrade notes`, `docs/upgrade-notes.md` and
the release body (UPGRADE-2). The other batteries
(events, scheduler, mail, gRPC, multi-tenancy, i18n) are not here yet.
The **CHAOS-0** resilience suite is in: `internal/faulttest` (deterministic
fault injection: a faulting `database/sql` driver, a scripted HTTP
dependency, a TCP fault proxy, the retry-policy check), `TestFault_*` tests
run by `make test-faults` (a sharded PR check), and the seeded stochastic
suite `internal/chaos` (`make test-chaos`, nightly `chaos.yml`, never a PR
check). See `docs/testing/fault-injection.md`.
Don't assume generated apps are committed in-tree; `gombit new` writes them
on demand. Check `git log` / `ls` before describing "how the code works."

## Source of truth

- `docs/GOMBIT_BUILD_PLAN.md` is the **historical v0.1 build plan** — no
  longer maintained. Its locked decisions (§1-§3) and working agreement (§5)
  still apply as restated in this file; later ADRs in `docs/adr/` supersede
  it where they differ (e.g. ADR-016 on generated resource code). Current
  scope lives in this file, the ADRs, and GitHub issues.
- `docs/GO_FULLSTACK_FRAMEWORK_DESIGN.md` is rationale/prose only, cited by
  backlog entries (e.g. "draft §41") for context — never a source of
  additional scope on its own.
- GitHub issues, titled `[ID] ...` (e.g. `[M1-2] framework.App + lifecycle +
  hooks`, `[JOBS-3] ...`), are the unit of work. The v0.1 ones map to build
  plan §4 entries; later epics (SCHEMA, JOBS, CHAOS, ...) list their children
  in the epic issue. Milestones run `M0 spike` → `M1 runtime` →
  `M2 migrations` → `M3 contract` → `M4 cli` → `M5 frontend-auth` →
  `M6 admin` → `post-v0.1`. Don't start an issue whose "Depends on #N" is
  still open.

## Locked architecture decisions (build plan §1-§3 — do not re-litigate)

- **Contract layer:** Huma-typed handlers over Gin are the source of truth for
  the API contract (OpenAPI 3.1 emitted, not hand-written). Raw `*gin.Engine`
  stays reachable via `app.Router()` as a first-class, tested escape hatch.
- **App layout (generated apps):** feature-package under `internal/<feature>/`
  (user-owned model and `hooks.go`, generator-owned `*.gen.go` per ADR-016;
  `service.go`/`repo.go` only with `--service`/`--repo`). Never Laravel-style `app/controllers`, `app/models`.
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
   "battery" (events, scheduler, mail, storage, gRPC, multi-tenancy, i18n),
   stop and split it out into its own epic.
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
  issues, without asking first.
- If something looks missing from the backlog, flag it — don't silently add
  scope.

## Code review

Before opening or merging a PR, review the diff as an adversarial senior
reviewer: the implementation must satisfy its claimed contract and the
working agreement above. Personality is presentation; technical analysis
comes first. Do not invent findings. In Claude Code, run the project
`code-review` skill (`.claude/skills/code-review/SKILL.md`); in Cursor, run
the project `code-review` skill (`.cursor/skills/code-review/SKILL.md`).

## Onboarding

For a new contributor, Claude Code has the project `onboarding` skill
(`.claude/skills/onboarding/SKILL.md`): required reading, live repo
reconnaissance, issue selection (bug-fix or backlog/epic lane), the
implementation loop, validation, and PR. It is opt-in — run via
`/onboarding`, when a user says they are new to the repo, or when they ask
to be walked through a first issue — and not part of ordinary issue work.
It defers to this file wherever the two disagree.

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
