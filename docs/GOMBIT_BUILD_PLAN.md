# Gombit — Build Plan & Locked Decisions

**Status:** Build-ready v1.0
**Date:** 2026-08-14
**Supersedes:** the "Open Decisions" (§57) of `GO_FULLSTACK_FRAMEWORK_DESIGN.md`, and the specific sections noted inline below.
**Purpose:** This document is the authoritative source for creating GitHub issues and driving agent-based implementation. The original design doc remains the reference for *rationale and prose*; where the two conflict, **this document wins**.

**Spiritual model — Django, not Laravel.** The identity line shifts from "Rails/Laravel/Django cohesion" to specifically **Django-for-Go**: Django's feature set (apps, migrations incl. `makemigrations`, `createsuperuser`, an auto-admin, management commands) and CLI ergonomics, implemented with a Go-idiomatic backend, a React + TypeScript (+ optional MUI) frontend, and **none of Django's runtime metaclass/reflection magic**. The rule is: copy Django's *what*, keep Go's *how* — the same stance principle 6.2 already takes toward Rails/Laravel. This reinforces C2 (Django "apps" ≈ feature-packages) and also differentiates positioning from the existing "Laravel-for-Go" (Goravel).

---

## 0. How to use this document

1. Read §1 (Decisions I changed) first — these reverse the original draft. Veto any you disagree with **before** issues are created; every downstream issue assumes them.
2. §2 locks all remaining open questions so no agent is ever blocked on a product decision.
3. §3 restates the contested architecture decisively (contract pipeline, layout, auth, envelope, generate-vs-runtime rule).
4. §4 is the **issue backlog** — each entry maps 1:1 to a GitHub issue.
5. §5 is the **agent working agreement** — the definition of done. Put this in `CONTRIBUTING.md` and reference it from every issue.

Framework identity is fully decided: **Gombit**, `github.com/gombit-dev/gombit`. Nothing left blocking repo creation — M0 issue #1 can be opened.

---

## 1. Decisions I changed from the draft (veto these first)

| # | Topic | Draft proposed | Locked here | Why the reversal |
|---|---|---|---|---|
| C1 | **Contract layer** | Bespoke `framework.Bind` + hand-built OpenAPI emission | **Adopt Huma over Gin** for typed handlers + validation + OpenAPI 3.1 | Building your own is the "second inferior framework inside the framework" trap (draft §55.4). Huma is the only low-magic way to make "Go is the source of truth" true without comment annotations. |
| C2 | **App layout** | Laravel-style `app/controllers`, `app/models`, `app/services` | **Feature-package under `internal/<feature>/`** | The Laravel tree violates your own principle 6.2 and reads as non-Go. Buffalo/Bud partly died on feeling un-idiomatic; adoption risk you can't afford. Cohesion is kept per-feature. |
| C3 | **Auth default** | Cookie/session default | **Bearer JWT is the v0.1 API default; session/cookie is a first-class mode** (no longer a mere preset) | Bearer is what already exists (true extraction) and stays the API default. But the Django admin **requires sessions**, so session/cookie auth is promoted to first-class and becomes a hard prerequisite of the admin milestone. Generated frontend stores the bearer token **in memory, never localStorage**. |
| C4 | **UI preset default** | MUI default | **Minimal/headless default**, MUI opt-in (`--ui mui`) | Don't couple core to a design system's lifecycle (your own §24.2). Less surface to maintain solo. |
| C5 | **Frontend embedding** | Embed-on-build default | **Split default**, embed via `gombit build --embed` | Split is the simpler mental model and the common deploy; embedding is a nice-to-have, not the default path. |
| C6 | **Per-resource repo/service** | Four-layer stack scaffolded per resource | **Thin controller-over-GORM default**, `--service`/`--repo` opt-in | Resolves the §15.1↔§15.2 contradiction. Pass-through service/repo layers for plain CRUD are boilerplate users delete. |

If any of C1–C6 is wrong for you, say which — each has a cluster of issues hanging off it.

---

## 2. Locked decisions (closes original §57)

- **D1 — Name: DECIDED — `Gombit`.** Module path `github.com/gombit-dev/gombit`, CLI binary `gombit`. Fully closed.
- **D2 — Repo/org:** New dedicated public repo `gombit-dev/gombit`, not a fork of the template. License **MIT** (matches existing repo). Governance: BDFL/solo for now.
- **D3 — Migration representation:** **Wrap Atlas, don't hand-roll a DSL.** `gombit db makemigrations` invokes the Apache-2.0 `ariga.io/atlas-provider-gorm` (Program Mode, so it reads GORM models spread across feature-packages) + `atlas migrate diff` to generate **versioned SQL migrations from model changes** — Django's `makemigrations` for Go+GORM. `gombit db migrate` applies them. This **replaces** the earlier fluent `migration.Schema` builder. Same rationale as C1/Huma: don't build a diff engine that already exists (§55.4). Locked, pending the licensing check in the ADR (M2-0). Escape hatch: hand-written SQL/HCL migrations remain droppable into the migration dir.
- **D4 — Migration metadata:** Track `version, name, batch, applied_at`. **No checksums** — they are near-useless for Go-source migrations (formatting/comments change the hash without changing schema). Locked.
- **D5 — OpenAPI → TS toolchain:** Go side emits OpenAPI 3.1 via Huma. TS side uses **`openapi-typescript`** (types) + **`openapi-fetch`** (client). Both are low-magic and generation-only. Locked.
- **D6 — Package manager:** Detect (prefer `pnpm`, else `npm`). Default `npm` when none present. Locked.
- **D7 — Generated Go file naming:** `snake_case` filenames, PascalCase exported types. Locked.
- **D8 — API prefix:** Default `/api/v1`, configurable. Locked (matches existing repo).
- **D9 — Generic CRUD repo:** `repository.New[T]` lives in the **runtime** as an optional helper. Never generated per-model. Locked.
- **D10 — Response envelope:** `{"data": ..., "meta"?: ...}` success; `{"error": {code, message, fields?, request_id}}` error. This is a **redesign** of the existing `{"error":"string"}`; acceptable for a new framework, but note it in the migration guide. Locked.
- **D11 — Pre-v1 compatibility:** No guarantees before v1.0. Breaking changes documented in CHANGELOG. Locked.
- **D12 — Databases in v0.1:** SQLite + PostgreSQL + MySQL are required and CI-gated. MySQL is promoted back into the first supported database set so generated apps can target common MySQL deployments without application code changes.
- **D13 — CLI framework: DECIDED — Cobra (`spf13/cobra`).** Nested command families (`db`, `make`, `client`, …), help/completions, and M4-7 runtime-registered management commands use Cobra's `AddCommand` model. Kong (and other struct-tag CLIs) are rejected because dynamic app-registered commands fit poorly. Pre-M4 `cmd/gombit` may keep stdlib `flag` until M4-1 adopts Cobra and migrates existing `db` subcommands onto the tree. Recorded in ADR-014.

---

## 3. Decisive architecture (replaces contested sections)

### 3.1 Contract pipeline — replaces §13.4/13.5, §14, §23

The Go handler is the source of truth **via Huma typed handlers**, not comments and not a separate OpenAPI file.

```
Huma-typed handler (input/output structs, validated)
        ↓  (Huma emits)
OpenAPI 3.1 document  (served at /openapi.json; interactive docs at /docs; written by `gombit openapi generate`)
        ↓  (openapi-typescript)
TypeScript types
        ↓  (openapi-fetch, thin generated wrapper)
React client
```

Rules:
- Anything in the public API contract is a Huma handler. Raw `*gin.Engine` remains reachable (`app.Router()`) for anything outside the contract (webhooks, SSE, legacy). The **escape hatch is a first-class, tested path**, not an afterthought.
- Validation lives in the Huma input struct (tags). Validation failures render the D10 error envelope with `fields`.
- The generated TS client and the OpenAPI doc are **build/CI artifacts** — drift between server and client fails CI.

### 3.2 Generated application layout — replaces §10

Feature-package, idiomatic Go:

```
myapp/
├── cmd/server/main.go
├── internal/
│   ├── platform/            # app wiring the framework owns the shape of
│   └── product/             # one package per resource
│       ├── product.go       # model
│       ├── handler.go       # Huma handlers (thin, over GORM by default)
│       ├── service.go       # ONLY if --service
│       ├── repo.go          # ONLY if --repo
│       └── routes.go        # registration
├── database/migrations/
├── database/seeds/
├── config/
├── frontend/                # Vite React app
├── gombit.yaml
├── .env.example
├── go.mod
└── README.md
```

`routes.go` per feature is registered explicitly from `main.go` (no reflection discovery — principle 6.2).

### 3.3 The generate-vs-runtime rule — NEW, resolves §55.7

This single rule determines whether `gombit upgrade` is ever feasible. State it as law:

> **Behavior lives in the versioned runtime. Generated code is a thin, one-time scaffold the user owns. The framework never rewrites user-owned files.**

Consequences, enforced in review:
- Generators are **idempotent and additive**. Re-running never clobbers edits. `--dry-run` and `--force` required.
- Route registration is edited via `go/ast`, never regex, and only appends to a known registration point.
- `gombit upgrade` bumps dependencies and emits **reviewable codemod diffs** — it never edits in place.
- If a feature can live in the runtime instead of in generated code, it must.

### 3.4 Auth — replaces §20 default

- **v0.1 API default:** Bearer JWT + refresh rotation (extracted from the template). Generated frontend holds the access token **in memory only**; refresh via the rotation endpoint. No localStorage.
- **Session/cookie — first-class, not a preset:** HttpOnly/Secure/SameSite cookies + CSRF (`--auth cookie`). Still greenfield, still gets its own hardening issues and threat-model doc (SPA + separate dev origin + SameSite + double-submit), **but it is a required, first-class mode** because the admin (M6-Admin) depends on it. Session auth is a hard prerequisite of the admin milestone.
- Django-style **users / groups / permissions** auth core underpins both modes and the admin's authorization checks.
- The `X-API-Key` service gate stays available but is **off by default** for browser apps and documented as server-to-server only.

---

## 4. Issue backlog (issue-ready)

Format per issue: **[ID] Title** — scope · acceptance criteria · deps · size (S/M/L) · labels.
Milestones are dependency-ordered; do not start Mn+1 issues that depend on unfinished Mn gates.

### M0 — Bootstrap + Contract Spike (the gate)

- **[M0-1] Create repo, module path, CI skeleton** — new repo, `go.mod` with `github.com/gombit-dev/gombit`, MIT license, golangci-lint, GitHub Actions running `go test ./...` + lint. AC: green CI on an empty package; branch protection on `main`. deps: D1. size: S. labels: `infra`.
- **[M0-2] Contract-layer spike: Huma over Gin** — wire Huma to a Gin engine; implement two handlers: one Huma-typed resource (`GET/POST /widgets`) that appears in the emitted OpenAPI, and one raw `*gin.Engine` route (`/raw/ping`) that does not. Emit `openapi.json`. AC: OpenAPI 3.1 validates; typed handler shows request/response schema; raw route works and is absent from the spec; a short latency benchmark vs plain Gin recorded. **This issue is a go/no-go gate.** deps: M0-1. size: M. labels: `spike`, `contract`.
- **[M0-3] ADR-011: Contract layer = Huma** — record the decision, the escape-hatch pattern, and the benchmark. If M0-2 fails the escape-hatch test, this ADR instead records the fallback (bespoke `Bind` + emission) and M3 issues are rewritten. deps: M0-2. size: S. labels: `adr`.

### M1 — Runtime extraction from `golang-rest-api-template`

One issue per seam. Each must keep the existing tests green (see §5).

- **[M1-1] Typed config boundary** — introduce a typed `config.Config`; move all `os.Getenv` reads to the config boundary; low-level packages receive typed config. AC: no `os.Getenv` outside `config`; existing behavior unchanged; config validation errors are explicit. deps: M0-1. size: M. labels: `runtime`, `config`.
- **[M1-2] `framework.App` + lifecycle + hooks** — extract app construction and the 15-step lifecycle (draft §11.2) into the runtime; add `OnStart`/`OnStop` with deterministic ordering and bounded shutdown context. AC: minimal example boots via `framework.Run`; graceful shutdown test passes. deps: M1-1. size: L. labels: `runtime`, `lifecycle`.
- **[M1-3] De-domain the router** — remove `Book`/`User` knowledge from router/bootstrap; route registration becomes application-owned; framework mounts only its own endpoints (probes, metrics, openapi). AC: runtime package contains zero example-domain models; middleware order preserved and tested. deps: M1-2. size: M. labels: `runtime`, `http`.
- **[M1-4] Multi-driver `database.Open` + capability model** — SQLite + Postgres + MySQL via `gorm.Open` switch; `Driver()` and `Capabilities()` exposed; driver-aware pool defaults. AC: same code opens all three; capability flags covered by tests; database CI matrix covers SQLite, Postgres, and MySQL. deps: M1-1. size: M. labels: `runtime`, `database`.
- **[M1-5] Normalize cache interface** — replace the go-redis-leaking interface with `Get/Set/Delete/Increment` value semantics; memory + redis + noop drivers; `app.Redis()` escape hatch when redis is enabled. AC: rate limiter and cache users compile against new interface; memory driver used in tests. deps: M1-1. size: M. labels: `runtime`, `cache`.
- **[M1-6] Optional Mongo log sink** — Zap stays; Mongo becomes a selectable sink/module, not a runtime dependency; default sink stdout/stderr. AC: app boots and logs with Mongo absent. deps: M1-1. size: S. labels: `runtime`, `logging`.
- **[M1-7] Preserve observability + security tests** — carry over metrics, tracing, probes, request-id/timeout, security headers, trusted-proxy tests into the runtime package. AC: parity test suite green. deps: M1-2. size: M. labels: `runtime`, `tests`.
- **[M1-8] XSS HTML sanitization middleware** — extract/preserve the template's request-input XSS middleware (HTML-tag sanitization) into the runtime default stack after body-size limit and before handlers; document it as a fundamental security default (headers alone are not enough). AC: markup/script payloads in request fields are sanitized before handlers see them; middleware order tested; docs + example coverage. deps: M1-7. size: S. labels: `runtime`, `http`, `security`, `tests`.

**M1 exit gate:** a minimal example app boots through the runtime with no example-domain code in the runtime, on SQLite, Postgres, and MySQL, all extracted tests green.

### M2 — Migrations (Atlas-backed, Django `makemigrations`)

- **[M2-0] ADR-012: Migrations = Atlas GORM provider** — confirm `ariga.io/atlas-provider-gorm` (Apache 2.0) covers the needed workflow on SQLite + Postgres + MySQL via **Program Mode** (models across feature-packages), and verify which capabilities sit in the open core vs the paid Atlas Pro/cloud tier (versioned `migrate diff` is open; confirm nothing you depend on — lint checks, drift monitoring — is gated). Record the decision or the fallback (hand-rolled DSL). **Go/no-go gate for M2.** deps: M1-4. size: S. labels: `adr`, `migrations`.
- **[M2-1] `gombit db makemigrations`** — wrap the Atlas provider loader + `atlas migrate diff` to generate a versioned migration from current GORM models; wire the loader to enumerate feature-package models. AC: changing a model and running `gombit db makemigrations` writes a correct versioned migration on SQLite + Postgres + MySQL. deps: M2-0. size: L. labels: `migrations`, `cli`.
- **[M2-2] `gombit db migrate` / `rollback` / `status`** — apply/roll back/report versioned migrations (Atlas apply + a `framework_migrations`-style revision record: version/name/batch/applied_at, no checksum per D4). AC: up/down/status reflected and correct on all supported DBs. deps: M2-1. size: M. labels: `migrations`, `cli`.
- **[M2-3] `gombit db seed` / `reset`** — seeders and a dev reset (drop+migrate+seed). AC: seed and reset work on all supported DBs. deps: M2-2. size: S. labels: `migrations`, `cli`.
- **[M2-4] Multi-DB conformance CI** — matrix job runs the DB conformance suite (CRUD, tx, migrate up/down, timestamps, nullable, unique, index, decimal, pagination) on SQLite + Postgres + MySQL, using Atlas-generated migrations. AC: matrix green. deps: M2-3. size: M. labels: `ci`, `database`.

### M3 — Contract pipeline

- **[M3-1] Huma DTO + validation conventions** — request/response struct conventions, validation tags → D10 error envelope with `fields`. AC: invalid request returns structured field errors. deps: M0-3, M1-3. size: M. labels: `contract`, `http`.
- **[M3-2] Response envelope + error mapping** — `{data, meta}` / `{error{code,message,fields,request_id}}`; error categories (draft §41) mapped centrally. AC: envelope covered by tests; category→status mapping table tested. deps: M3-1. size: M. labels: `contract`.
- **[M3-3] OpenAPI emission + `gombit openapi generate`** — serve `/openapi.json` and a FastAPI-style interactive docs UI at `/docs` by default (local/dev; production may disable/protect); CLI writes the spec to disk. AC: spec validates; matches live routes; `/docs` try-it-out works; raw Gin routes absent from spec/docs. deps: M3-1. size: S. labels: `cli`, `contract`.
- **[M3-4] TS types + client generation + `gombit client generate`** — `openapi-typescript` + `openapi-fetch` wrapper; typed errors map to the D10 envelope. AC: generated client compiles against a sample spec. deps: M3-3, D5. size: M. labels: `cli`, `frontend`, `contract`.
- **[M3-5] Contract drift check in CI** — regenerate spec + client; fail if the working tree changes. AC: intentional server change without regen fails CI. deps: M3-4. size: S. labels: `ci`, `contract`.

### M4 — CLI + generators

- **[M4-1] `gombit new`** — adopt Cobra (D13 / ADR-014) as the `gombit` command tree; migrate existing `db` subcommands onto Cobra; interactive + non-interactive scaffold; DB/cache/auth/UI flags; feature-package layout (§3.2); `gombit.yaml`, `.env.example` splitting server vs `VITE_*` public values. AC: root help lists command families; `gombit db …` still works via Cobra; `gombit new demo --database sqlite` produces a compiling app. deps: M1 exit, M2, M3. size: L. labels: `cli`, `generator`.
- **[M4-2] `gombit dev`** — Go reload (air/watchexec), Vite, `/api` proxy, OpenAPI watch→regenerate, service table (includes `/openapi.json` and `/docs`). AC: one command runs backend+frontend with HMR and live contract regen; service table prints API docs URL. deps: M4-1. size: L. labels: `cli`, `devx`.
- **[M4-3] `gombit make resource` (AST-safe)** — generates model, Huma handler (thin over GORM), routes, migration, and frontend pages/forms/table; registers routes via `go/ast`; idempotent; `--dry-run`/`--force`; `--service`/`--repo` opt-in (C6). AC: generated resource works backend→frontend with no manual type duplication; re-run doesn't clobber edits. deps: M4-1, M3-4. size: L. labels: `cli`, `generator`.
- **[M4-4] Introspection: `gombit routes`, `gombit doctor`, `gombit config show`** — routes table; doctor checks (Go/Node, config, DB/Redis connectivity, migration status, ports, insecure prod settings). AC: doctor flags a deliberately-broken config. deps: M4-1. size: M. labels: `cli`, `devx`.
- **[M4-5] Generator golden tests** — for each generator: run against a fixture, diff against golden, compile backend, typecheck frontend, verify idempotency. AC: golden suite green in CI. deps: M4-3. size: M. labels: `tests`, `generator`.
- **[M4-6] `gombit createsuperuser`** — interactive (and flag-driven) command that creates an admin user against the users/groups/permissions core. AC: creates an admin user on a fresh DB; refuses duplicates; hashes password per the framework hasher. deps: M2-2, M5-2. size: S. labels: `cli`, `auth`.
- **[M4-7] Management-command extensibility (`gombit make command`)** — Django-style: a feature-package registers custom `gombit <command>`s via Cobra `AddCommand` (D13); generator scaffolds one. AC: a generated command is discoverable and runnable via `gombit`. deps: M4-1. size: M. labels: `cli`, `generator`.

**Django → `gombit` command map** (target surface; issues above cover the v0.1 subset):

| Django `manage.py` | `gombit` | Milestone |
|---|---|---|
| `startproject` | `gombit new` | M4-1 |
| `startapp` | `gombit make app` | M4-3 (variant) |
| — | `gombit make resource` | M4-3 |
| `makemigrations` | `gombit db makemigrations` | M2-1 |
| `migrate` | `gombit db migrate` | M2-2 |
| `createsuperuser` | `gombit createsuperuser` | M4-6 |
| `runserver` | `gombit dev` | M4-2 |
| `show_urls` | `gombit routes` | M4-4 |
| `check` | `gombit doctor` | M4-4 |
| custom commands | `gombit make command` | M4-7 |
| `collectstatic` | folded into `gombit build --embed` | M5-5 |
| `shell` | *skipped for v0.1* (poor fit in Go) | — |

**M4 exit gate:** `gombit new` → `gombit dev` → `gombit make resource` → working authenticated CRUD app, backend-to-frontend, no hand-written contract, on SQLite + Postgres + MySQL.

### M5 — Frontend + auth polish

- **[M5-1] Vite React skeleton (minimal preset)** — router, providers, generated client wiring, error→form mapping (React Hook Form). AC: skeleton builds and talks to the API. deps: M3-4. size: M. labels: `frontend`.
- **[M5-2] Bearer auth integration** — in-memory access token, refresh rotation, protected routes. AC: login→access protected route→refresh→logout E2E. deps: M5-1. size: M. labels: `frontend`, `auth`.
- **[M5-3] Cookie/session + CSRF preset (`--auth cookie`)** — HttpOnly/Secure/SameSite cookies, CSRF for state-changing requests, threat-model doc. AC: CSRF + cookie-attribute security tests pass. deps: M5-2. size: L. labels: `auth`, `security`, `greenfield`.
- **[M5-4] MUI preset (`--ui mui`)** — port the monorepo's MUI CRUD patterns as an opt-in preset. AC: `--ui mui` scaffolds MUI screens. deps: M5-1. size: M. labels: `frontend`, `preset`.
- **[M5-5] Optional `go:embed` build (`gombit build --embed`)** — single-binary with SPA fallback. AC: embedded binary serves API + static + index fallback. deps: M4-2. size: M. labels: `build`.

### M6-Admin — Django-style admin (POST-v0.1 flagship)

The single biggest differentiator — **no Go web framework has a real Django-style admin** (not Gin/Echo/Fiber/Encore). This is the headline of the first release *after* v0.1, not part of the v0.1 loop. Architecture follows the §3.3 law: the admin is a **runtime** surface, not generated pages. Accepted decision: [ADR-013](adr/013-runtime-generic-admin.md).

- **[ADMIN-0] ADR-013: runtime generic admin over an introspection API** — decide the model-registry/introspection contract (models, fields, relations, permissions a feature-package registers) and confirm the admin is a framework-owned React app driven by that metadata endpoint — **not** `--admin` scaffolded pages. Prerequisite: session auth (C3). deps: M5-3. size: M. labels: `adr`, `admin`.
- **[ADMIN-1] Model registry + introspection endpoint** — explicit `admin.Register(Model, opts)`; framework serves the metadata (no deep runtime reflection — principle 6.2). AC: registered models expose fields/permissions over the endpoint. deps: ADMIN-0. size: L. labels: `admin`, `runtime`.
- **[ADMIN-2] Generic React admin app** — list/detail/create/edit/delete with filter/search/pagination, rendered from the metadata; permission-aware. AC: registering a model makes a working admin screen appear with zero per-model frontend code. deps: ADMIN-1, M5-1. size: L. labels: `admin`, `frontend`.
- **[ADMIN-3] Admin auth + authorization** — session-gated admin, groups/permissions enforced. AC: non-permitted users are refused; superuser (M4-6) has full access. deps: ADMIN-2, M5-3. size: M. labels: `admin`, `auth`, `security`. — **done**

### REL — Release polish (POST-v0.1, packaging only)

Everything needed to hand Gombit to someone who has never seen it: discoverable README, an install path, a tutorial, community health files, and a release pipeline. **No runtime capability** — this section adds packaging and onboarding, and pulls in no M6 battery. Milestone: `post-v0.1`. Implementation plan: [plans/REL-release-polish.md](plans/REL-release-polish.md).

- **[REL-1] README rewrite** — badges (CI, Release, Go, license, pkg.go.dev), positioning, feature table, quickstart, architecture diagram, comparison table; doc link list moves to `docs/README.md`. AC: quickstart runs verbatim in a clean dir; every badge resolves. deps: REL-4. size: M. labels: `devx`.
- **[REL-2] Contributor and community health files** — CONTRIBUTING covering setup, the DB matrix, golden tests, and drift; plus `CODE_OF_CONDUCT.md` and `SECURITY.md`. AC: GitHub community-standards checklist is satisfied. size: S. labels: `devx`, `security`.
- **[REL-3] Issue templates** — YAML forms for bug report, feature request, and question, plus `config.yml` disabling blank issues. AC: each form opens and applies its labels; no new labels beyond §6. size: S. labels: `devx`.
- **[REL-4] `gombit version` + release workflow** — `cli.Version` stamped by ldflags with a `debug.ReadBuildInfo` fallback, `version` subcommand and `--version`; `release.yml` publishing checksummed archives for 5 targets on a `v*` tag or manual bump. AC: `gombit version --short` on a release binary prints the tag; `SHA256SUMS.txt` verifies. size: L. labels: `cli`, `ci`, `build`.
- **[REL-5] Installation guide** — `docs/installation.md`: prerequisites (incl. the cgo/SQLite constraint), `go install`, release archives with checksum verification, from source, per-OS and WSL2 notes, troubleshooting. AC: followed successfully on a clean machine; `gombit doctor` passes. deps: REL-4. size: M. labels: `devx`.
- **[REL-6] Tutorial** — `docs/tutorial.md` building a tasks app across the whole v0.1 loop: scaffold, model, migration, resource, contract, client, frontend, auth, admin, management command, embedded build. AC: every command runs in order from an empty dir. deps: REL-5. size: L. labels: `devx`.
- **[REL-7] Tutorial example app** — `examples/tutorial/` committed and built in CI so the tutorial cannot rot. AC: `go build ./examples/...` is a CI step. deps: REL-6. size: M. labels: `devx`, `tests`.
- **[REL-8] Docs index, changelog, release runbook** — `docs/README.md`, `CHANGELOG.md` (Keep a Changelog), `docs/releasing.md`. AC: every `docs/*.md` appears exactly once in the index. deps: REL-6. size: S. labels: `devx`.
- **[REL-9] Scaffolded apps must resolve a published framework version** — `scaffold/templates/go.mod.tmpl` hardcoded `require github.com/gombit-dev/gombit v0.0.0`, a version that does not exist, so `go build ./...` in a fresh `gombit new` tree failed with "missing go.sum entry" unless the user added a local `replace`; `goldentest` only passed because it injects one into a temp copy. This blocked the documented quickstart. Fixed by pinning the generated `go.mod` to the scaffolding binary's own version (`ResolveFrameworkVersion`, accepting release tags and pseudo-versions, rejecting `dev` / `+dirty` / v2-without-path-suffix) and running `go mod tidy` to populate `go.sum` — a version pin alone is not enough, since `go build` will not write `go.sum` itself. `--framework-version` and `--skip-tidy` override both halves; unresolvable versions warn with the `replace` recipe instead of emitting a broken tree. Goldens unchanged. AC: after `gombit new`, `go build ./...` succeeds with no manual edits. deps: REL-4. size: M. labels: `cli`, `generator`, `devx`. — **done**

### M6 — Deferred batteries (POST-v0.1, not on the critical path)

Each is a **future epic**, explicitly out of v0.1. Do not create these as active issues until v0.1 ships: jobs/queues, events, scheduler, mail, storage, optional gRPC, multi-tenancy hooks, i18n. Park them in a "post-v0.1" project column. **The admin (M6-Admin) is the prioritized post-v0.1 flagship** and comes first among post-v0.1 work.

### BENCH — Reproducible benchmarks (POST-v0.1)

- **[BENCH-1] Reproducible framework benchmark suite + README performance results** — checked-in `benchmarks/` harness quantifying Gombit's abstraction cost (net/http → Gin → Huma+Gin → Gombit) and realistic PostgreSQL CRUD/auth performance against Gin+GORM (primary control) and Django+DRF/Rails/Laravel/NestJS (ecosystem context), plus a generated README `## Performance` section. Supersedes the M0-2 Huma/Gin spike benchmark (`internal/contractspike`) as the ongoing framework-tax measurement while preserving it untouched as historical record. AC: see issue #141 §25. Implementation plan: [plans/BENCH-1-benchmark-suite.md](plans/BENCH-1-benchmark-suite.md). deps: none (post-v0.1, no locked architecture change). size: XL. labels: `infra`, `devx`, `tests`, `ci`.

### HOST — Host / deployment contracts (POST-v0.1)

Three declarative, verifiable contracts a deployment **host** consumes to
build, health-check, and safely migrate an ordinary handwritten Gombit app —
without inferring anything from repo trivia and without the app having been
produced by Forge or bound to any host. Driven by an external consumer
(Gombit Cloud, `gombit-cloud/docs/DESIGN.md` §9/§24/§29–§31), whose design
requires these contracts to live **upstream in Gombit** (MIT), open to any host.
**Not on the v0.1 critical path; adds no M6 battery** — contracts, not runtime
services. Decision gated by [ADR-015](adr/015-host-deployment-contracts.md).
Milestone: `post-v0.1`.

- **[HOST-0] ADR-015: host/deployment contracts** — lock the shape of the three contracts (application contract, health convention, migration safety manifest) so HOST-1..3 are unblocked, honoring §3.3 (generate-vs-runtime), 6.2 (no repo-trivia inference), C1/OpenAPI (health stays raw-Gin, out of spec), D4 (no drift checksums; manifest hash is anti-tamper), and ADR-012 (no new migration engine). Confirm the contracts are host-neutral and never required to run a Gombit app. AC: ADR accepted; HOST-1..3 issues reconciled to `[HOST-*]` titles under `post-v0.1`. deps: none. size: M. labels: `adr`, `contract`.
- **[HOST-1] Machine-readable application contract** — emit a stable, versioned (`contract_version`) JSON description of an app — framework version, build command/artifact, runtime ports + health paths, database driver/required, migrations path — **projected from declared config** (`gombit.yaml`, resolved framework version per REL-9), never from tree-walking or README/dir heuristics (§10, 6.2). Provisional surface `gombit contract app`. Not an HTTP route; not in OpenAPI. AC: `gombit contract app` on a sample app emits valid JSON whose values come from declared config; unknown framework version fails loudly; schema + `contract_version` covered by tests; docs + example. deps: HOST-0, REL-9, M4-1. size: M. labels: `contract`, `cli`.
- **[HOST-2] Runtime health convention (`/livez` + `/readyz`)** — promote the existing raw-Gin `/livez`/`/readyz` routes to a documented, stable operational contract and make `/readyz` **meaningful**: `200 {"data":{"status":"ready"}}` only when the configured datastore is reachable and `OnStart` hooks have completed, else `503` with a D10 `not_ready` error. `/livez` stays a cheap dependency-free `200`. Stays out of OpenAPI (operational, like `/metrics`). Supersedes Cloud DESIGN.md §24 `/healthz` — the host consumes `/livez`+`/readyz`. AC: `/readyz` returns 503 before readiness and 200 after, verified on SQLite + Postgres + MySQL; readiness check is not per-request-heavy; `docs/health.md` documents the exact bodies/codes. deps: HOST-0, M1 (lifecycle/hooks). size: M. labels: `runtime`, `http`, `lifecycle`.
- **[HOST-3] Migration safety manifest + verifier** — define a versioned (`manifest_version`) manifest classifying a migration's operations into a closed set (`create_table`, `add_column`, `create_index`, `drop_column`, `drop_table`, `drop_index`, `rename_*`, `alter_column`, `other`) with destructive kinds flagged `safety: "data_loss"` / `requires_confirmation: true`, plus a **verifier** that reads the Atlas-generated SQL (ADR-012, no new engine) and proves the declared classification matches — a manifest claiming `non_destructive` over a `DROP COLUMN` is a verification failure (§31, L8). `sql_sha256` binds the manifest to the reviewed SQL (anti-tamper, not D4 drift). Provisional surface `gombit db verify` / emission at `makemigrations`. The **approval gate is host policy, not Gombit runtime** (Cloud §32). Does not touch `framework_migrations` (D4). AC: verifier classifies the closed op set correctly; the two adversarial cases (hash mismatch; declared-safe-but-SQL-drops-column) fail verification; covered by tests on the DB matrix where SQL differs; docs + example. deps: HOST-0, M2-2. size: L. labels: `migrations`, `security`, `contract`.

---

## 5. Agent working agreement (definition of done)

Put this in `CONTRIBUTING.md`; reference from every issue. A PR is **not** done unless:

1. **Tests:** new behavior has unit tests; runtime changes keep the extracted suite green; DB-touching changes pass the SQLite + Postgres + MySQL matrix.
2. **Docs + example:** every stable feature ships docs and appears in an example app.
3. **Extraction discipline:** do not rewrite code that passes its tests. Refactor and move; preserve contracts (draft §2.3). If a "small extraction" turns into a rewrite, stop and open a discussion issue.
4. **Generator safety:** Go source is modified via `go/ast`/`go/format` only — never regex. Generators are idempotent, support `--dry-run`/`--force`, print created/modified files, and never silently overwrite user-owned files (§3.3).
5. **Security invariants:** no secrets in generated frontend source; `VITE_*` treated as public; production config validation must loudly fail the cases in draft Appendix C.
6. **Contract integrity:** any API change regenerates OpenAPI + TS client in the same PR; CI drift check must pass (M3-5).
7. **Scope guard:** if an issue starts pulling in an M6 battery, split it out. v0.1 is the one CRUD loop, nothing more.
8. **Every PR links its issue** and states which acceptance criteria it satisfies.

---

## 6. Suggested GitHub labels & milestones

Milestones: `M0 spike`, `M1 runtime`, `M2 migrations`, `M3 contract`, `M4 cli`, `M5 frontend-auth`, `M6 admin`, `post-v0.1`.
Labels: `infra`, `runtime`, `config`, `lifecycle`, `http`, `database`, `cache`, `logging`, `migrations`, `contract`, `cli`, `generator`, `devx`, `frontend`, `auth`, `security`, `build`, `preset`, `admin`, `tests`, `ci`, `adr`, `spike`, `greenfield`, `good-first-issue`.

`good-first-issue` candidates for agents to warm up on: M0-1, M1-6, M2-3, M3-3, M4-4.

---

## 7. Critical-path summary

```
M0-1 (github.com/gombit-dev/gombit) → M0-2/M0-3 (Huma GATE) → M1 (extraction)
                                                  ↓
                              M2-0 (Atlas GATE) → M2 (migrations)
                                                  ↓
                                         M3 (contract pipeline)
                                                  ↓
                                       M4 (cli + generators) → v0.1 GATE
                                                  ↓
                                    M5 (frontend + auth, incl. session mode) → v0.1 release
                                                  ↓
                                       M6-Admin (post-v0.1 flagship; needs session auth)
```

Two go/no-go gates invalidate downstream work if they fail: **M0-2** (Huma escape hatch → reshapes M3) and **M2-0** (Atlas licensing/coverage → falls back to a hand-rolled migration DSL). Everything after the v0.1 release is post-v0.1; the **admin is the prioritized flagship** among that work and depends on session auth landing in M5-3.
