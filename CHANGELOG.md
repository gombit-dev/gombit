# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Gombit is pre-1.0: minor versions may contain breaking changes. Pin an exact
version.

## [Unreleased]

### Added

- `gombit db makemigrations --rename-table old:new` renames a table with a
  native, data-preserving `ALTER TABLE ... RENAME TO` on SQLite, PostgreSQL,
  and MySQL. With `--forget-model` / `--model` it swaps the renamed model in the
  registry, and `--rename` in the same run names the new table. The plan
  suggests the exact command for a dropped table that looks renamed, and
  `--forget-model` no longer acknowledges that drop. Indexes, foreign keys, and
  checks re-created under GORM's new names with the same definition are safe
  `rename_*` steps, so the follow-up migration needs no `--allow`. Rename
  migrations (table and column) now write their inverse to `downs/`, so
  `gombit db rollback` can undo them. `--rename` also accepts
  `table.old=table.new`
  ([#310](https://github.com/gombit-dev/gombit/issues/310)).

- `gombit db plan` classifies the schema change the models imply before a
  migration is written: `destructive` (dropped table or column, narrowed type),
  `unsafe` (fails on a populated table: a NOT NULL column with no default,
  nullable to NOT NULL, a new unique index, foreign key, or check), `review`
  (delete-rule changes, widened types, SQLite table rebuilds), or `safe`. A
  dropped column next to a same-typed added column comes with the `--rename`
  command that keeps the data. It exits non-zero on an unacknowledged
  destructive or unsafe step, and `--json` prints the steps
  ([#309](https://github.com/gombit-dev/gombit/issues/309)).

### Changed

- **Breaking (workflow):** `gombit db makemigrations` runs the same plan once a
  migration exists and refuses to write a destructive or unsafe change until
  each step is acknowledged with `--allow <id|code>`. `--forget-model`
  acknowledges the drop of the forgotten model's table (by GORM's default
  name). `gombit make resource` hits the same check
  when an unrelated model has a pending destructive change. The refusal prints
  the `gombit db makemigrations` command that writes the refused migration,
  including the new model and every `--allow`
  ([#309](https://github.com/gombit-dev/gombit/issues/309)).

## [0.3.0] — 2026-09-26

### Added

- One logical field vocabulary (`package field`) shared by `gombit make resource`,
  `gombit generate`, and the admin, so a new kind has one extension path. Existing
  tokens (`int`, `bool`, `time`, …) and admin meta strings stay valid; `time`
  remains a `datetime` alias. See [docs/fields.md](docs/fields.md) ([#392](https://github.com/gombit-dev/gombit/pull/392)).
- `make resource` generates `uuid`, `json`, `date`, `float` / `float64`, and
  `datetime` fields ([#393](https://github.com/gombit-dev/gombit/pull/393)), and `email`, `url`, `slug`, and `ip` fields that
  stay Go `string` with an OpenAPI `format` or pattern the API validates
  ([#395](https://github.com/gombit-dev/gombit/pull/395)).
- `time_of_day` (`types.TimeOfDay`, `HH:MM:SS`) and `duration` (`types.Duration`,
  stored as nanoseconds) fields, and enum labels:
  `enum(draft=Draft,published=Published)` keeps the stored value separate from the
  form and admin label ([#398](https://github.com/gombit-dev/gombit/pull/398)).
- Field constraints `default=`, `min=` / `max=`, `max_length=`, and `regex=`
  ([#394](https://github.com/gombit-dev/gombit/pull/394)). Each lands in a
  different place (see [docs/fields.md](docs/fields.md#constraints)): `min=` /
  `max=` become a SQL `CHECK` plus request and form bounds; `max_length=` sets
  the column size and request `maxLength`; `regex=` is a request `pattern` and a
  form check, not a SQL check; `default=` is applied when a create body omits
  the field or sends null, not as a column `DEFAULT`.
- `one_to_one` relations (a unique foreign key), `nullable` and
  `on_delete=restrict|cascade|set_null` on `belongs_to` / `one_to_one`, and
  self-referential relations when the foreign key is nullable ([#396](https://github.com/gombit-dev/gombit/pull/396)).
- `gombit make resource --id uuid` scaffolds an application-assigned `uuid.UUID`
  primary key instead of embedding `gorm.Model`; the default stays `uint`. A
  `belongs_to` foreign key follows the target model's primary-key type
  ([#397](https://github.com/gombit-dev/gombit/pull/397)).

## [0.2.1] — 2026-09-24

### Added

- `gombit db makemigrations <name> --rename table.old:new` emits a native,
  data-preserving `ALTER TABLE … RENAME COLUMN` instead of a drop + add rebuild
  that lost the renamed column's data ([#379](https://github.com/gombit-dev/gombit/pull/379)).

### Fixed

- `gombit db makemigrations --forget-model` refuses a model still listed in
  `AutoMigrate` (which would re-create it) and errors on an untracked model
  instead of a silent no-op. `gombit make resource` fails closed before writing when Atlas is missing,
  so its output no longer depends on `PATH`; `--skip-migrations` opts out
  ([#378](https://github.com/gombit-dev/gombit/pull/378)).

## [0.2.0] — 2026-09-20

### Changed

- **Breaking (generated resource layout):** `gombit make resource` is now
  model-first (RESGEN-1, ADR-016,
  [#352](https://github.com/gombit-dev/gombit/issues/352)). It scaffolds the
  human-owned model (with `gombit:"..."` field policy) and a `.gombit-resource`
  marker, then runs `gombit generate` to derive the generator-owned
  `dto.gen.go` + `handler.gen.go` (which owns `Register`) and seed a human-owned
  `hooks.go` — instead of a human-owned `handler.go`/`routes.go`. Customization
  moves from editing the handler to the model, its field policy, and hooks;
  editing the `*.gen.go` is unsupported (regeneration overwrites them). Scalar,
  relation, and list-query HTTP contracts (routes, DTO shape, filter/sort/search/
  aggregate surface, validation) are preserved.
  - `enum(...)` values are stored on the model's `validate` tag (the GORM
    schema only keeps a varchar) and `gombit generate` emits them as a Huma
    `enum` on the create body.
  - `make resource` is now **preflighted and atomic**: it plans both phases and
    validates the whole operation (including compiling the pending model in a Go
    build overlay, so the real tree is untouched) before writing anything, then
    applies the scaffold and generated files as one transaction. It refuses to
    re-scaffold over a resource still on the legacy handler layout.
  - Existing apps keep compiling; convert old resources with the
    [migration guide](docs/migration-model-first-resources.md).
- New `gombit generate` command regenerates the model-first `*.gen.go` from the
  app's real models, with `gombit generate --check` as a drift gate.
- **The per-handler request timeout is now opt-in** (PERF-12,
  [#270](https://github.com/gombit-dev/gombit/issues/270); ADR-017).
  `HTTP.RequestTimeout` now defaults to `0`, which imposes no cooperative
  per-handler context deadline. The deadline lives in the `request_context`
  middleware (#268 folded it in — there is no separate timeout layer), which
  always runs; a `0` value only skips the deadline setup, a no-op that adds
  nothing to the request path (≈4 allocs/op saved on every request). **Breaking for apps that
  relied on the implicit 60s default:** without setting
  `GOMBIT_HTTP_REQUEST_TIMEOUT` a long-running DB query is no longer cancelled
  and a slow handler keeps running after the connection's `WriteTimeout`. Set
  `GOMBIT_HTTP_REQUEST_TIMEOUT` to any positive duration to restore it;
  `gombit new` scaffolds `60s` explicitly, so new projects keep the deadline.
  The `http.Server` `ReadTimeout`/`WriteTimeout`/`IdleTimeout` remain a
  connection-level safety net regardless — when the per-handler deadline is
  disabled they fall back to `60s` instead of becoming unbounded, so this is not
  a DoS regression. (A value of `0` no longer zeroes the server timeouts.) See
  [docs/adr/017-request-timeout-opt-in.md](docs/adr/017-request-timeout-opt-in.md).
- **Request-input HTML sanitization is now opt-in** (PERF-13,
  [#271](https://github.com/gombit-dev/gombit/issues/271); ADR-018). The default
  `framework.App` no longer strips HTML from request input — it does not read,
  decode, or rewrite the body/query for sanitization, saving ~4–5 allocs/op on
  every write request. XSS is handled where it belongs, on **output** (React JSX
  text escaping in the generated frontend and the React admin SPA; a JSON
  response is not an HTML sink; the response CSP is a backstop). The default
  middleware no longer mutates request **values**, so `{"description":"x < y"}`
  and `{"note":"<b>bold</b>"}` reach handlers intact (value fidelity, not
  byte-for-byte — the response envelope re-encodes JSON; a handler needing the
  exact bytes reads the raw body via `WithRawBodyPaths`). The 8MiB JSON
  body-size bound the sanitizer used to provide incidentally is preserved as a
  separate, always-on `request_body_limit` layer, so raw Gin routes keep their
  413-before-handler memory bound whether or not sanitization is enabled.
  **Breaking for apps that relied on ingress stripping:** set
  `Security.SanitizeInput` (`GOMBIT_SECURITY_SANITIZE_INPUT=true`) to restore
  the legacy middleware unchanged (including the `password` exemption and
  `WithRawBodyPaths` handling), or call the newly exported
  `framework.SanitizeHTML(s)` to strip a single field from a handler. See
  [docs/security.md](docs/security.md#input-sanitization-opt-in) and
  [docs/adr/018-input-sanitization-opt-in.md](docs/adr/018-input-sanitization-opt-in.md).

### Fixed

- Generated DTOs can no longer drift from the model they map ([#218](https://github.com/gombit-dev/gombit/issues/218)): the
  model-first layout derives them from the model on every `gombit generate`
  ([#372](https://github.com/gombit-dev/gombit/pull/372); regression test in [#376](https://github.com/gombit-dev/gombit/pull/376)).

## [0.1.14] — 2026-09-13

### Added

- `gombit db hash` recomputes `atlas.sum` after a hand-edited migration, so
  recovering from a checksum mismatch no longer needs the raw Atlas CLI
  ([#297](https://github.com/gombit-dev/gombit/pull/297)).

### Changed

- **Security headers are now scoped by response kind** (PERF-9,
  [#267](https://github.com/gombit-dev/gombit/issues/267)). JSON/API responses
  get the strict, minimal policy `Content-Security-Policy: default-src 'none';
  frame-ancestors 'none'` plus `X-Content-Type-Options` (and HSTS in
  production) — no `X-Frame-Options` or `Referrer-Policy`, since
  `frame-ancestors 'none'` already subsumes the former and a `default-src
  'none'` document needs neither. HTML responses (the embedded SPA, the admin
  SPA, and Huma's `/docs`) keep the full browser policy including
  `Referrer-Policy` and `X-Frame-Options: DENY`. This holds the common API
  response under Go's 8-header swiss-map threshold, removing ~5 allocs/op of
  header-map growth from every API response. The dead IE8-only
  `X-Download-Options: noopen` header is no longer set on any response. If you
  relied on `X-Frame-Options`/`Referrer-Policy` or the old `default-src 'self'`
  CSP on JSON responses, note the new API policy. See
  [docs/security.md](docs/security.md).
- **Breaking (minimum Go):** the framework now requires **Go 1.26** (`go.mod`
  `go 1.26.0`), raised by `golang.org/x/crypto` v0.56.0
  ([#294](https://github.com/gombit-dev/gombit/pull/294)). Scaffolded apps
  (`gombit new`) now pin `go 1.26.0`, and the badges/prerequisites in the README,
  installation guide, and tutorial move to Go 1.26+. Migration URL generation
  gained a fix required by the newer toolchain: an in-memory SQLite DSN
  (`:memory:`) now maps to Atlas's canonical `sqlite://file?mode=memory&…` dev
  URL instead of `sqlite://:memory:?…`, whose empty-host `:memory:` authority
  `net/url` (and therefore Atlas) rejects as an invalid port on Go 1.26+.
- The admin data plane now hard-deletes rows so the database's foreign keys
  enforce integrity: a `RESTRICT` reference fails as `409 conflict`, and
  `CASCADE` / `SET NULL` actually run. Admin soft-delete was a side effect of
  `gorm.Model`, with no restore path ([#296](https://github.com/gombit-dev/gombit/pull/296)).

### Fixed

- Cookie-mode CSRF no longer breaks after a second tab opens or the cookie
  expires: `GET /auth/csrf` reuses a valid cookie, and the admin SPA and
  generated clients read the token at request time and recover from a `403`
  ([#295](https://github.com/gombit-dev/gombit/pull/295)).

## [0.1.13] — 2026-09-05

### Added

- Migration safety manifest + verifier (HOST-3,
  [#284](https://github.com/gombit-dev/gombit/issues/284); ADR-015):
  `gombit db verify` classifies each migration's SQL into a closed operation set,
  flags data-loss operations (drop column/table, destructive alter,
  delete/truncate) as `requires_confirmation`, and (`--write`) emits a
  `<version>_<name>.manifest.json` bound to the SQL by `sql_sha256`. Verifying an
  existing manifest fails on a hash mismatch (SQL changed after review) or a
  mis-declared safety — a host re-derives the classification and never trusts the
  declared field (§31). Gombit classifies and verifies; the approval gate is the
  host's policy. New importable `manifest` package. See
  [docs/migration-safety.md](docs/migration-safety.md).
- Application contract (HOST-1,
  [#282](https://github.com/gombit-dev/gombit/issues/282); ADR-015):
  `gombit contract app` emits a stable, versioned, machine-readable description
  of an app — framework version, build command/artifact, runtime port + health
  paths, database driver/requirement, migrations path — for a deployment host to
  consume. Every field is projected from declared config and `go.mod`, never
  inferred from the source tree; a missing or local-path-replaced framework
  version fails loudly (a version-to-version replace resolves to its target).
  Adds a declared `Config.Database.Required` (`GOMBIT_DATABASE_REQUIRED`, default
  `true`) so `database.required` is a real config value, not a proxy for auth.
  See [docs/app-contract.md](docs/app-contract.md).
- Runtime health contract (HOST-2,
  [#283](https://github.com/gombit-dev/gombit/issues/283); ADR-015): `/livez`
  and `/readyz` are now a documented, stable host contract
  ([docs/health.md](docs/health.md)). `framework.WithShutdownDrainDelay` keeps
  the server accepting after `/readyz` starts returning 503 on shutdown, so a
  host can deregister the instance before connections are refused.

### Changed

- **Breaking (probe contract):** `GET /readyz` now reflects real readiness
  (HOST-2, [#283](https://github.com/gombit-dev/gombit/issues/283)). Its success
  body is `{"data":{"status":"ready"}}` — `data.status` changed from `"ok"` to
  `"ready"` — and it now returns `503` with a D10 `not_ready` envelope while
  draining or when an attached datastore is unreachable. Previously it always
  returned `200 {"data":{"status":"ok"}}`. `/livez` is unchanged.
- Faster JSON content-type check in the XSS sanitizer ([#274](https://github.com/gombit-dev/gombit/pull/274)).

## [0.1.12] — 2026-09-03

### Added

- Declared server-side numeric aggregates on generated list handlers: mark an
  `int` / `int64` / `uint` / `decimal` field `aggregatable` and request
  `?aggregate=sum:total,avg:total`; results land in `meta.aggregates`
  ([#273](https://github.com/gombit-dev/gombit/pull/273)).

### Fixed

- The XSS sanitizer keeps the text after an unclosed skip tag ([#261](https://github.com/gombit-dev/gombit/pull/261)).

## [0.1.11] — 2026-09-03

### Added

- Declared server-side list filtering, sorting, and search on generated list
  handlers via the `filterable`, `sortable`, and `searchable` field modifiers,
  queried as `?<field>=`, `?ordering=`, and `?search=` (the admin data plane's
  query spelling). A
  `belongs_to` foreign key is filterable by default ([#263](https://github.com/gombit-dev/gombit/pull/263)).

### Fixed

- `gombit db rollback` runs down files statement by statement ([#257](https://github.com/gombit-dev/gombit/pull/257)).
- `gombit make resource` rejects non-ASCII resource names before writing
  anything ([#262](https://github.com/gombit-dev/gombit/pull/262)).

## [0.1.10] — 2026-09-01

### Fixed

- The memory cache reclaims expired entries that are never read again
  ([#253](https://github.com/gombit-dev/gombit/pull/253)).
- The embedded frontend serves `HEAD` like `GET` on SPA routes and `index.html`,
  so `HEAD`-based health checks pass ([#254](https://github.com/gombit-dev/gombit/pull/254)).
- Admin search is case-insensitive on SQLite, PostgreSQL, and MySQL alike
  ([#252](https://github.com/gombit-dev/gombit/pull/252)).

## [0.1.9] — 2026-08-30

### Changed

- Hot-path performance: lock-free metrics middleware ([#244](https://github.com/gombit-dev/gombit/pull/244)), correlation
  IDs without `crypto/rand` ([#245](https://github.com/gombit-dev/gombit/pull/245)), no XSS decode/re-encode for bodies
  without markup ([#246](https://github.com/gombit-dev/gombit/pull/246)), and no request-timeout re-wrap under a tighter
  deadline ([#247](https://github.com/gombit-dev/gombit/pull/247)).

## [0.1.8] — 2026-08-30

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
- Admin `belongs_to` picker
  ([#223](https://github.com/gombit-dev/gombit/issues/223)): auto-derivation
  renders a foreign-key column as a relation field (target `slug` = the related
  table; `label_field` = the field name of its `name` column), and the SPA shows
  a single-select picker backed by the related model's list endpoint that stores
  the selected primary key — instead of a bare integer input. Preserves numeric
  vs uuid/string keys; an empty selection clears an optional FK. `has_many` stays
  read-only.

## [0.1.7] — 2026-08-29

### Added

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

## [0.1.6] — 2026-08-29

### Added

- `framework.WithCSRFExemptPaths` — opt specific request paths out of
  cookie-mode CSRF enforcement, for non-browser endpoints (webhooks,
  server-to-server callbacks) that cannot echo a double-submit token and
  authenticate themselves instead (e.g. HMAC signature verification). Safe
  methods still bootstrap the cookie; exempt handlers must verify the caller.
  See [`docs/auth-cookie.md`](docs/auth-cookie.md)
  ([#226](https://github.com/gombit-dev/gombit/issues/226)).

## [0.1.5] — 2026-08-28

### Changed

- Fewer per-request middleware allocations on the runtime stack ([#211](https://github.com/gombit-dev/gombit/pull/211)).

### Fixed

- Admin create, update, and delete report foreign-key and NOT NULL violations as
  client errors instead of `500` ([#216](https://github.com/gombit-dev/gombit/pull/216)).
- Admin create no longer reports a provided but invalid required field as "is
  required" ([#213](https://github.com/gombit-dev/gombit/pull/213)).
- The metrics middleware bounds the HTTP method label, closing an
  unbounded-cardinality memory DoS ([#214](https://github.com/gombit-dev/gombit/pull/214)).

## [0.1.4] — 2026-08-28

### Added

- The BENCH-1 benchmark suite and workflows. No framework changes.

## [0.1.3] — 2026-08-25

### Fixed

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

## [0.1.2] — 2026-08-20

### Changed

- **Breaking (module path):** the module moved from
  `github.com/LAA-Software-Engineering/gombit` to `github.com/gombit-dev/gombit`.
  Update imports and `go.mod`.

## [0.1.1] — 2026-08-19

### Fixed

- SQLite apps apply a seeded bootstrap migration, so `AutoMigrate` and Atlas
  never diverge ([#101](https://github.com/gombit-dev/gombit/pull/101), [#104](https://github.com/gombit-dev/gombit/pull/104)).
- The migration model registry persists, so adding a model never drops an older
  one ([#99](https://github.com/gombit-dev/gombit/pull/99)).
- The admin data plane's row type has a valid OpenAPI schema name ([#103](https://github.com/gombit-dev/gombit/pull/103)).
- Tutorial fixes: `.env` loads automatically, the generated JWT secret comment
  matches its value, generated forms show a required-field message, and
  `gombit client check` works outside this repository ([#98](https://github.com/gombit-dev/gombit/pull/98)).

## [0.1.0] — 2026-08-18

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

### Changed

- README rewritten: badges, positioning, feature list, quickstart, architecture
  diagram, and a comparison table, with the doc link list moved to
  `docs/README.md`.
- CONTRIBUTING expanded with setup, the database test matrix, golden-test
  regeneration, and the contract drift check.
- CI now builds `./examples/...` so committed examples cannot rot.

### Fixed

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

### Notes

- SQLite requires cgo (`mattn/go-sqlite3`). Official release binaries are built
  with cgo enabled on native runners; `go install` needs a C compiler if you use
  SQLite. PostgreSQL and MySQL are pure Go. See
  [installation.md](docs/installation.md).
- Post-v0.1 batteries — jobs, events, scheduler, mail, storage, gRPC,
  multi-tenancy, i18n — are **not** included. See
  [the build plan](docs/GOMBIT_BUILD_PLAN.md).

[Unreleased]: https://github.com/gombit-dev/gombit/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/gombit-dev/gombit/compare/v0.2.1...v0.3.0
[0.2.1]: https://github.com/gombit-dev/gombit/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/gombit-dev/gombit/compare/v0.1.14...v0.2.0
[0.1.14]: https://github.com/gombit-dev/gombit/compare/v0.1.13...v0.1.14
[0.1.13]: https://github.com/gombit-dev/gombit/compare/v0.1.12...v0.1.13
[0.1.12]: https://github.com/gombit-dev/gombit/compare/v0.1.11...v0.1.12
[0.1.11]: https://github.com/gombit-dev/gombit/compare/v0.1.10...v0.1.11
[0.1.10]: https://github.com/gombit-dev/gombit/compare/v0.1.9...v0.1.10
[0.1.9]: https://github.com/gombit-dev/gombit/compare/v0.1.8...v0.1.9
[0.1.8]: https://github.com/gombit-dev/gombit/compare/v0.1.7...v0.1.8
[0.1.7]: https://github.com/gombit-dev/gombit/compare/v0.1.6...v0.1.7
[0.1.6]: https://github.com/gombit-dev/gombit/compare/v0.1.5...v0.1.6
[0.1.5]: https://github.com/gombit-dev/gombit/compare/v0.1.4...v0.1.5
[0.1.4]: https://github.com/gombit-dev/gombit/compare/v0.1.3...v0.1.4
[0.1.3]: https://github.com/gombit-dev/gombit/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/gombit-dev/gombit/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/gombit-dev/gombit/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/gombit-dev/gombit/releases/tag/v0.1.0
