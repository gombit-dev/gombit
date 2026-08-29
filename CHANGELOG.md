# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Gombit is pre-1.0: minor versions may contain breaking changes. Pin an exact
version.

## [Unreleased]

### Added

- `gombit make resource` relation fields
  ([#222](https://github.com/gombit-dev/gombit/issues/222) part b):
  `name:belongs_to:Target`, `name:has_many:Target`, and
  `name:many_to_many:Target` generate the foreign key / association on the model
  (and the `many2many:` join table), importing the target feature-package. The
  thin CRUD handler exposes `belongs_to` as its foreign key (`engine_id`);
  `has_many` / `many_to_many` are model-only — in the admin, `many_to_many` is
  editable and `has_many` is shown read-only. A `has_many` child must carry the
  parent foreign key itself. Self-referential relations (a target equal to the
  resource itself) are rejected for now — they need a nullable foreign key /
  explicit join keys — as is a `belongs_to` whose synthesized `<name>_id` foreign
  key collides with another field.
- Admin read-only `has_many` view
  ([#223](https://github.com/gombit-dev/gombit/issues/223)): a `has_many`
  association now auto-derives to a read-only relation field — list/detail
  preload it and return the related children's primary keys, and the SPA shows
  them as read-only chips (writes are rejected). Previously `has_many` was
  dropped from auto-derivation and returned nothing.
- Admin relation pickers are searchable
  ([#223](https://github.com/gombit-dev/gombit/issues/223)): the `belongs_to` and
  `many_to_many` widgets are now MUI Autocompletes. When the related model
  supports search, typing issues a debounced server-side `search` (so rows beyond
  the first page are reachable) and Autocomplete's local filter is turned off;
  otherwise it filters the loaded page client-side. An already-selected row that
  is off the current page is fetched (`client.detail`) so it shows its label, not
  a raw key. A model registered without a `Search` now defaults it to the model's
  text columns (an explicit empty `Search` opts out), so search — and the picker
  — work out of the box on the documented registration path.
- `framework.WithRawBodyPaths` — mark webhook / server-to-server paths whose
  request body must reach the handler byte-for-byte, for signature verification
  (e.g. GitHub `X-Hub-Signature-256`). The XSS sanitizer, which re-encodes JSON
  bodies, is skipped for these paths, and they are CSRF-exempt too (so a
  signature-verifying webhook needs only this one option). Fixes webhooks that
  failed HMAC checks because the body was re-encoded before the handler saw it
  ([#232](https://github.com/gombit-dev/gombit/issues/232)).
- Admin many-to-many relationships end to end
  ([#223](https://github.com/gombit-dev/gombit/issues/223)): a `many_to_many`
  relation field round-trips as a list of related primary keys — list/detail
  preload and read the ids, create/update sync the join table (with existence
  validation; a missing id is a 422; an empty list clears it), and
  auto-derivation (`FieldsFrom`) emits the relation instead of dropping the
  association. The framework admin SPA renders it as a multi-select backed by
  the related model's list endpoint.
- Admin `belongs_to` picker
  ([#223](https://github.com/gombit-dev/gombit/issues/223)): auto-derivation
  renders a foreign-key column as a relation field (target `slug` = the related
  table; `label_field` = the field name of its `name` column), and the SPA shows
  a single-select picker backed by the related model's list endpoint that stores
  the selected primary key — instead of a bare integer input. Preserves numeric
  vs uuid/string keys; an empty selection clears an optional FK. `has_many` stays
  read-only.
- `gombit make resource` field grammar now supports `decimal`, `decimal(p,s)`,
  `time`, and `enum(a,b,c)` in addition to the existing scalars. `decimal` uses
  the new framework `types.Decimal` (a `shopspring/decimal` wrapper that carries
  an OpenAPI string schema and GORM persistence), `time` maps to `time.Time`,
  and `enum` maps to a validated string column. A single Go type flows through
  the model, handler DTO, OpenAPI/TS contract, and GORM, so these types do not
  reproduce the model/DTO drift of
  [#218](https://github.com/gombit-dev/gombit/issues/218). An optional
  `time`/`decimal` field becomes a pointer so it can be left empty. Relationships
  remain future work ([#222](https://github.com/gombit-dev/gombit/issues/222) part b).
- `types.Decimal` — a fixed-point decimal for money and exact numerics, shared
  by generated models, DTOs, and the admin data plane
  ([#222](https://github.com/gombit-dev/gombit/issues/222)).
- A framework home for domain logic shared by the API and admin write paths
  ([#224](https://github.com/gombit-dev/gombit/issues/224)):
  - `database.Validator` (`Validate(ctx, tx) error`) runs via a GORM callback on
    every create/update, so an invariant enforced once cannot be bypassed by the
    other write surface. A returned `database.ValidationError` maps to a D10 422
    with field detail through `database.MapPersistError`.
  - `framework.App.Tx(ctx, fn)` — a transaction helper for multi-model writes;
    `Validate` hooks run inside it.
  - Optimistic locking on the admin update path: a model with an integer
    `version` column gets a version-guarded update that returns 409 on a stale
    write instead of silently last-write-wins.
  - See [`docs/validation.md`](docs/validation.md).
- `framework.WithCSRFExemptPaths` — opt specific request paths out of
  cookie-mode CSRF enforcement, for non-browser endpoints (webhooks,
  server-to-server callbacks) that cannot echo a double-submit token and
  authenticate themselves instead (e.g. HMAC signature verification). Safe
  methods still bootstrap the cookie; exempt handlers must verify the caller.
  See [`docs/auth-cookie.md`](docs/auth-cookie.md)
  ([#226](https://github.com/gombit-dev/gombit/issues/226)).
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

- An unsupported HTTP method on a known route now returns `405 Method Not
  Allowed` with an `Allow` header (and the D10 envelope, code
  `method_not_allowed`) instead of `404`, so clients can distinguish a missing
  resource from an unsupported method. A genuinely unknown path still returns
  the 404-style fallback ([#225](https://github.com/gombit-dev/gombit/issues/225)).
- Response bodies and generated request types no longer carry Huma's
  off-contract `$schema` key; the D10 envelope is exactly `{data, meta?}` /
  `{error}`. Regenerated `openapi.json` and the sample TS client
  ([#225](https://github.com/gombit-dev/gombit/issues/225)).
- The generated `.env` / `.env.example` now quotes `GOMBIT_DATABASE_DSN`, so a
  DSN containing `&`/`?` survives `set -a; . ./.env` instead of being truncated.
  `config.Load` strips one layer of matching quotes, so the runtime value is
  unchanged ([#225](https://github.com/gombit-dev/gombit/issues/225)).
- `gombit make resource` derives route paths (and TS client method names) with
  GORM's pluralizer (`jinzhu/inflection`), so irregular nouns agree with the
  table name: `Mouse` → `/mice`, `Person` → `/people`, `Analysis` → `/analyses`
  ([#225](https://github.com/gombit-dev/gombit/issues/225)).
- Concurrent `POST /auth/refresh` of the same still-valid token no longer
  family-revokes the winner's new session (two tabs / parallel curls)
  ([#127](https://github.com/gombit-dev/gombit/issues/127)).
- Generated README Run snippet no longer copies `.env.example` over the
  per-project `.env` JWT secret
  ([#126](https://github.com/gombit-dev/gombit/issues/126)).
- `gombit make resource` refuses `Product` / `product`, the scaffold
  feature-package every `gombit new` app already owns
  ([#125](https://github.com/gombit-dev/gombit/issues/125)).
- `gombit make command` refuses `version` and `createsuperuser`, which
  `cli.NewRoot` already registers, so generated stubs cannot shadow the
  framework commands
  ([#124](https://github.com/gombit-dev/gombit/issues/124)).
- `gombit make resource` and `gombit make command` strip trailing `//`
  comments from the `go.mod` module line, so
  `module example.com/demo // app` does not produce invalid imports
  ([#123](https://github.com/gombit-dev/gombit/issues/123)).
- `gombit make resource` refuses a second name whose plural HTTP path
  collides with an existing feature-package (`Bus` and `Buse` both become
  `/buses`) instead of registering two resources on one URL
  ([#122](https://github.com/gombit-dev/gombit/issues/122)).
- `gombit dev` Vite proxy now forwards `/admin` to the Go origin, and
  cookie-mode apps print an Admin row in the service table, so
  `http://127.0.0.1:5173/admin/` reaches the framework admin SPA instead of
  the generated application catch-all
  ([#121](https://github.com/gombit-dev/gombit/issues/121)).
- `gombit new --auth none` is rejected. v0.1 auth is `jwt` or `cookie` (C3);
  `none` was accepted and silently scaffolded a JWT app
  ([#120](https://github.com/gombit-dev/gombit/issues/120)).
- Cookie-mode generated SPAs bootstrap CSRF in `AppProviders` (not only
  on the login page) and await it before unsafe requests, so a reload on
  a gated route does not POST without `X-CSRF-Token`. `clearSession` drops
  the in-memory CSRF token
  ([#119](https://github.com/gombit-dev/gombit/issues/119)).
- XSS request sanitizer no longer truncates JSON/query strings that contain
  `<` without a complete HTML tag (e.g. `a<b` stayed `a`). Complete tags
  are still stripped ([#118](https://github.com/gombit-dev/gombit/issues/118)).
- Cookie-mode session 401s omit `WWW-Authenticate: Bearer`. The D10 body
  stays `authentication`; Bearer mode still sends `Bearer realm="api"`
  ([#117](https://github.com/gombit-dev/gombit/issues/117)).
- Minimal generated number inputs use RHF `setValueAs` so a cleared field
  submits `0` instead of JSON `null` (`valueAsNumber` → `NaN`)
  ([#116](https://github.com/gombit-dev/gombit/issues/116)).
- Admin silent `POST /auth/refresh` on 401 awaits CSRF bootstrap, so a
  reload with an expired access cookie does not 403 CSRF and drop a valid
  refresh session ([#115](https://github.com/gombit-dev/gombit/issues/115)).
- Admin resource lists remount when the model slug changes (`key={slug}`),
  so pagination, search, ordering, and filters do not carry over to the next
  model ([#114](https://github.com/gombit-dev/gombit/issues/114)).
- Unknown-email login timing pad no longer races on `Service.dummyHash`.
  `compareDummy` initializes the dummy bcrypt hash once via `sync.Once` so
  concurrent `Authenticate` misses are race-free
  ([#113](https://github.com/gombit-dev/gombit/issues/113)).
- `gombit make resource` no longer hardcodes a **Products** home link on
  generated list pages. AppLayout already exposes that nav; Books (and any
  other resource) keep a New link only
  ([#112](https://github.com/gombit-dev/gombit/issues/112)).
- Generated get/create handlers map GORM errors to D10 categories instead of
  collapsing every load failure to `not_found` and every persist failure to
  `internal`. Missing rows are 404 `not_found`; unique/duplicate keys are 409
  `conflict`; other driver errors are 500 `internal`. Shared helpers live in
  `database` (`IsUniqueViolation`, `MapLoadError`, `MapPersistError`) and are
  used by generated handlers, admin, and auth
  ([#111](https://github.com/gombit-dev/gombit/issues/111)).
- Generated list handlers honor `page` / `per_page` instead of returning every
  row while advertising `meta.per_page=20`. Scaffolded product handlers,
  `gombit make resource`, and the tutorial Task list clamp like the admin data
  plane (default page 1, per_page 20, max 100), `COUNT` `meta.total`
  separately, and `LIMIT`/`OFFSET` the payload
  ([#110](https://github.com/gombit-dev/gombit/issues/110)).
- Generated application SPA honors `GOMBIT_API_PREFIX` / `config.API.Prefix`
  at runtime (HTML `__GOMBIT_API_PREFIX__` injection + `rewriteAPIRequest`)
  for `gombit dev` and `gombit build --embed`. `gombit client generate`
  rewrites live OpenAPI path keys to `/api/v1` before `openapi-typescript`
  so scaffolded `client.GET("/api/v1/...")` still typechecks after the
  prefix changes. Split/CDN deploys must substitute the same placeholder
  in `dist/index.html` (or set `window.__GOMBIT_API_PREFIX__`); it is not
  injected automatically
  ([#109](https://github.com/gombit-dev/gombit/issues/109)).
- Admin edit forms can clear optional fields. The SPA now sends JSON `null`
  for emptied string/text/date/datetime/json/number inputs instead of
  omitting them, and the data-plane setter writes NULL/empty when a PATCH
  key is present with `null` ([#108](https://github.com/gombit-dev/gombit/issues/108)).
- Generated SPA 401 interceptor no longer clones a consumed `Request` after
  silent refresh. POST/PATCH retries buffer via `clone().arrayBuffer()` gated
  on method (Firefox's `Request.body` getter is undefined) and resend those
  bytes ([#106](https://github.com/gombit-dev/gombit/issues/106)).
- Cookie-mode generated SPAs serialize CSRF bootstrap (`csrfInFlight` plus
  skip-if-token-exists) and `await bootstrapCSRF()` before login/register,
  so React StrictMode remounts no longer mint a second pair that 403s login
  ([#107](https://github.com/gombit-dev/gombit/issues/107)).
- XSS JSON sanitization no longer `io.ReadAll`s request bodies without a
  bound: JSON sanitizer buffering is capped at 8MiB (HTTP 413, D10
  `payload_too_large`), and `http.Server.ReadTimeout` follows
  `GOMBIT_HTTP_REQUEST_TIMEOUT` ([#137](https://github.com/gombit-dev/gombit/issues/137)).
- `AtlasURL` now converts Postgres unix-socket and IPv6 libpq DSNs, and
  SQLite `file:///abs` URIs, into Atlas `--url` values that parse
  ([#135](https://github.com/gombit-dev/gombit/issues/135)).
- `RedactDSN` / `SanitizeError` now redact libpq keyword/value DSN passwords
  (`password=secret dbname=app`) without swallowing the rest of the DSN, and
  strip the password token from driver errors that do not echo the full DSN
  ([#136](https://github.com/gombit-dev/gombit/issues/136)).
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
