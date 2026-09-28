---
name: create-feature
description: Implements a Gombit backlog issue or new framework capability as a scoped, tested change. Use when the user asks to create a feature, implement an issue, add a resource, scaffold a feature-package, or land a milestone item from docs/GOMBIT_BUILD_PLAN.md.
---

# Create Feature

Implement one Gombit unit of work. Read `AGENTS.md` first. `AGENTS.md` and the ADRs in `docs/adr/` win on conflicts (`docs/GOMBIT_BUILD_PLAN.md` is the historical v0.1 plan); the design doc is rationale only.

## When not to use

- Defect / failing test / regression → `bugfix`
- Reviewing an existing diff or PR → `code-review`
- Inventing work that is not a §4 backlog issue (flag it; do not silently add scope)

## Workflow

### 1. Orient

1. Identify the GitHub issue (`[ID] Title`, e.g. `[M1-2] ...`). If none exists, stop and ask — do not invent issues.
2. Read that issue's acceptance criteria, deps, size, and labels from build plan §4.
3. Confirm every "Depends on" issue is done. If a dependency is open, stop.
4. Check the working tree. This repo may still be **pre-code** (docs only). Do not assume `go.mod`, `internal/`, or a generated-app layout exists until `ls` / `git log` say so.
5. If the issue is an extraction from `golang-rest-api-template` or `crud-template-monorepo`, locate the existing code and **move/refactor** it. Do not rewrite passing tests.

### 2. Place the change

Decide **runtime vs generated** before writing files (build plan §3.3):

> Behavior lives in the versioned runtime. Generated code is a thin, one-time scaffold the user owns. The framework never rewrites user-owned files.

- If the behavior can live in the runtime, put it there.
- Generated application code uses the feature-package layout in [references/layout.md](references/layout.md). Never Laravel-style `app/controllers` / `app/models`.
- `service.go` / `repo.go` only when the issue or `--service` / `--repo` asks for them. Default is a thin Huma handler over GORM.
- Generated Go filenames are `snake_case`; exported types are PascalCase (D7).
- Default API prefix is `/api/v1` (D8).

Load [references/conventions.md](references/conventions.md) for Huma, envelope, auth, migrations, and frontend contract rules.

### 3. Implement

- Keep the change inside the issue's milestone. If an M6 battery (jobs, events, scheduler, mail, storage, gRPC, multi-tenancy, i18n) appears, stop and split it out.
- Do not re-litigate locked decisions (AGENTS.md / build plan §1–§3).
- Public API behavior goes through Huma-typed handlers. Use raw `*gin.Engine` via `app.Router()` only for webhooks, SSE, or other out-of-contract routes — and test that escape hatch.
- Validation lives on Huma input structs. Validation failures must render the D10 error envelope with `fields`.
- Expose `*gin.Engine` and `*gorm.DB`; do not wrap them in a second framework.
- Interfaces belong at real boundaries (cache, clock, mail, storage), not around GORM.
- Generators must be idempotent and additive, support `--dry-run` / `--force`, print created/modified files, and edit Go via `go/ast` + `go/format` only — never regex.
- Config reads stay in the typed config boundary. No new `os.Getenv` in low-level packages.
- Frontend: Vite + React + TypeScript. Access tokens stay **in memory**, never `localStorage`. Treat every `VITE_*` value as public.

### 4. Verify (definition of done)

A feature is not done unless all of these hold:

1. New behavior has tests. Prefer table-driven Go tests (`t.Run`) with useful failure messages. DB-touching changes pass SQLite, PostgreSQL, and MySQL.
2. Runtime extractions keep the extracted suite green.
3. Any API change regenerates OpenAPI 3.1 + the TypeScript client (`openapi-typescript` + `openapi-fetch`) in the same PR.
4. Stable features ship docs and appear in an example app.
5. No secrets in generated frontend source. Production config must fail loudly for draft Appendix C cases (short JWT secret, insecure cookies, wildcard CORS + credentials, debug Gin in prod, etc.).
6. The PR links its issue and states which acceptance criteria it satisfies.

Until `go.mod` exists, verification is: the change matches the issue AC, does not invent runtime APIs, and does not add scope beyond §4.

### 5. Hand-off

- One issue → one PR where practical.
- Conventional commit prefix is fine (`feat:`, `docs:`, `chore:`).
- After implementing, run the `code-review` skill against the diff before calling the work done. That skill is an adversarial contract review (`# APPROVE` / `# COMMENT` / `# REQUEST CHANGES`), not a checklist dump.
