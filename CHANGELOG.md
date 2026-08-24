# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Gombit is pre-1.0: minor versions may contain breaking changes. Pin an exact
version.

## [Unreleased]

### Added

- `gombit version` (and `--version`), reporting version, commit, build date, Go
  toolchain, and platform. Release binaries are stamped via ldflags; `go install`
  builds fall back to module build info.
- Release pipeline (`.github/workflows/release.yml`): cross-compiled binaries
  for `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`, and
  `windows/amd64`, published with `SHA256SUMS.txt` on a `v*.*.*` tag or a manual
  bump.
- Documentation: [installation guide](docs/installation.md), an end-to-end
  [tutorial](docs/tutorial.md), and a [docs index](docs/README.md).
- `examples/tutorial/` — the finished tutorial application, compiled in CI.
- Issue templates for bug reports, feature requests, and questions.
- `SECURITY.md` and `CODE_OF_CONDUCT.md`.

### Fixed

- `AtlasURL` now converts Postgres unix-socket and IPv6 libpq DSNs, and
  SQLite `file:///abs` URIs, into Atlas `--url` values that parse
  ([#135](https://github.com/gombit-dev/gombit/issues/135)).
- **Scaffolded apps now build with no manual steps.** `gombit new` wrote
  `require github.com/gombit-dev/gombit v0.0.0` — a version that
  has never existed on the module proxy — so `go build ./...` in a fresh tree
  failed with *missing go.sum entry*. The generated `go.mod` is now pinned to
  the version of the binary that scaffolded it (release tag or
  pseudo-version), and `go mod tidy` runs to populate `go.sum`. New
  `--framework-version` and `--skip-tidy` flags override each half. A CLI built
  from source still reports `dev`, which is unresolvable by design: the command
  explains that and prints the `replace` recipe instead of emitting a broken
  tree.

### Changed

- README rewritten: badges, positioning, feature list, quickstart, architecture
  diagram, and a comparison table, with the doc link list moved to
  `docs/README.md`.
- CONTRIBUTING expanded with setup, the database test matrix, golden-test
  regeneration, and the contract drift check.
- CI now builds `./examples/...` so committed examples cannot rot.

## [0.1.0] — unreleased

First tagged release. Milestones M0–M5 plus ADMIN-1 through ADMIN-3.

### Added

- **Runtime (M1).** Typed `config.Config` loaded from the environment;
  `framework.App` lifecycle with `OnStart` / `OnStop` hooks and graceful
  shutdown; application-owned route registration with the raw `*gin.Engine`
  reachable via `app.Router()`; structured logging; a cache runtime with memory,
  Redis, and noop drivers.
- **Databases (M1/M2).** GORM over SQLite, PostgreSQL, and MySQL, CI-gated on
  all three (D12), with a shared conformance suite.
- **Migrations (M2).** Atlas-backed `gombit db makemigrations`, `migrate`,
  `rollback`, `status`, `seed`, and `reset`, wrapping
  `ariga.io/atlas-provider-gorm` in Program Mode ([ADR-012](docs/adr/012-migrations-atlas-gorm-provider.md)).
  Versioned SQL, no hand-rolled DSL.
- **Contract (M3).** Huma-typed handlers over Gin as the source of truth for the
  API contract ([ADR-011](docs/adr/011-contract-layer-huma.md)); OpenAPI 3.1
  emitted from code; the D10 response envelope (`{data, meta?}` /
  `{error: {code, message, fields?, request_id}}`); generated TypeScript client
  with a drift check.
- **CLI (M4).** Cobra command tree ([ADR-014](docs/adr/014-cli-cobra.md)):
  `new`, `dev`, `build --embed`, `make resource`, `make command`, `db`,
  `openapi`, `client`, `routes`, `doctor`, `config show`, `createsuperuser`.
  Generators are idempotent and additive, edit Go source through `go/ast` only,
  and support `--dry-run` / `--force`.
- **Frontend and auth (M5).** Vite + React + TypeScript skeleton with router,
  generated client, and React Hook Form; Bearer JWT login with refresh rotation
  (access token held in memory, never `localStorage`); first-class cookie/session
  auth with CSRF double-submit (`--auth cookie`); the MUI CRUD preset
  (`--ui mui`); optional single-binary builds via `gombit build --embed`.
- **Admin (ADMIN-1..3).** A runtime generic admin over an explicit registry
  ([ADR-013](docs/adr/013-runtime-generic-admin.md)): `admin.Register`,
  `GET /api/v1/admin/meta`, the generic `/api/v1/admin/resources/{slug}` data
  plane, a framework-owned SPA under `/admin/`, and direct/group permission
  enforcement with a superuser bypass.

### Notes

- SQLite requires cgo (`mattn/go-sqlite3`). Official release binaries are built
  with cgo enabled on native runners; `go install` needs a C compiler if you use
  SQLite. PostgreSQL and MySQL are pure Go. See
  [installation.md](docs/installation.md).
- Post-v0.1 batteries — jobs, events, scheduler, mail, storage, gRPC,
  multi-tenancy, i18n — are **not** included. See
  [the build plan](docs/GOMBIT_BUILD_PLAN.md).

[Unreleased]: https://github.com/gombit-dev/gombit/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/gombit-dev/gombit/releases/tag/v0.1.0
