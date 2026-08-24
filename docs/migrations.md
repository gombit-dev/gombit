# Migrations

M2 introduces `gombit db` migration commands as a thin wrapper around Atlas
versioned migrations and `ariga.io/atlas-provider-gorm` Program Mode. Gombit
does not define its own migration DSL.

## The bootstrap migration

`gombit new` seeds `database/migrations/<timestamp>_bootstrap.sql` (and the
`models.json` registry entry for it — see [Generate A
Migration](#generate-a-migration)) covering every model
`internal/platform/database.go`'s `AutoMigrate` call registers — the
framework's own auth tables (`users`, `permissions`, `groups`,
`refresh_tokens`, their join tables) plus the `product/` example — when Atlas
is on `PATH` and `go mod tidy` succeeded. For `--database sqlite` (the
default), `gombit new` also **applies** it immediately, so
`gombit db status` shows it already applied before you've run `db migrate`
yourself. `--database postgres`/`mysql` get a placeholder DSN until you edit
`.env` with real credentials, so those are seeded but left for you to apply
with `gombit db migrate` once configured — same as always. If Atlas isn't
installed at all, `gombit new` prints the equivalent
`gombit db makemigrations bootstrap --model ...` command to run once it is.

This exists because `AutoMigrate` also runs at every app startup
(`app.OnStart`), creating those tables directly through GORM — including the
very first `gombit dev`, which the tutorial has you start before you ever
touch migrations yourself. Without the bootstrap migration **applied** before
that first run, `AutoMigrate` creates the tables live first, and applying the
migration afterward fails with `table users already exists`. Seeding it
without applying it isn't enough on its own: Atlas's tracked history has to
actually match the live database, not just have a migration file on disk
describing what it should eventually look like.

## Generate A Migration

Run the command from the application module root:

```sh
gombit db makemigrations create_products \
  --driver sqlite \
  --model github.com/acme/shop/internal/product.Product
```

The Atlas Community Edition CLI must be installed and available on `PATH`, or
supplied with `--atlas-bin`:

```sh
curl -sSf https://atlasgo.sh | sh -s -- --community
```

You only need to name what's new. `gombit db makemigrations` persists every
model it's ever seen for a given `--dir` in `database/migrations/models.json`
and merges new `--model` flags into that set — it is not the entire desired
schema by itself, just what this invocation is adding. Adding a second
feature later only needs its own model:

```sh
gombit db makemigrations create_accounts \
  --driver postgres \
  --model github.com/acme/shop/internal/account.Account
```

`product.Product` from the first migration is carried forward automatically;
this does not generate a `DROP TABLE product`. Commit `models.json` alongside
the SQL files it describes — like `atlas.sum`, it's part of the migration
history, not build output. Atlas itself ignores it (only `*.sql` and
`atlas.sum` are meaningful to `atlas migrate diff`/`apply`), and `gombit db
status`/`migrate` skip it the same way they already skip `atlas.sum`.

To retire a model — the table is genuinely going away — say so explicitly
instead of just omitting it, so Atlas proposes the drop on purpose:

```sh
gombit db makemigrations drop_legacy_widget \
  --driver postgres \
  --forget-model github.com/acme/shop/internal/widget.Widget
```

`--forget-model` removes the entry from `models.json` and from this
invocation's desired schema, so the generated migration is the intentional
`DROP TABLE` — the only way to get one, now that the registry means a model
is never dropped by accident just because a later call didn't repeat it.

The command writes a temporary Atlas Program Mode loader under `.gombit`,
passes all supplied model types to `gormschema.New(driver).Load(...)`, writes
the generated SQL schema to a temporary `schema.sql`, and then runs:

```sh
atlas migrate diff <name> --env gombit --config file://<generated atlas.hcl>
```

The temporary loader is removed after Atlas exits. Migration files are written
to `database/migrations` by default; override that with `--dir`.

`gombit db makemigrations` depends only on Atlas Community Edition features:
the generated config points `src` at the temporary schema file and uses
`atlas migrate diff`. It does not depend on Atlas Cloud, drift monitoring,
external schema data sources, or migration linting.

Gombit does not add a separate `--dry-run` flag in M2-1. Atlas owns the diff
preview behavior: if there is no model/schema change, `atlas migrate diff`
exits without writing a new migration.

Migration names may contain letters, numbers, underscores, and hyphens, and
must not start with a hyphen.

## Apply / Status / Rollback

M2-2 adds apply, status, and rollback. These commands read the configured
database from `config.Load()` (`GOMBIT_DATABASE_DRIVER` / `GOMBIT_DATABASE_DSN`)
and the migration directory (default `database/migrations`).

```sh
gombit db migrate [--dir database/migrations] [--atlas-bin atlas]
gombit db status  [--dir database/migrations] [--atlas-bin atlas]
gombit db rollback [--dir database/migrations]
```

### Apply

`gombit db migrate`:

1. Ensures the Gombit revision table `framework_migrations` exists.
2. Runs Atlas Community Edition
   `atlas migrate apply --url ... --dir file://... --allow-dirty`
   (PostgreSQL also passes `--revisions-schema public` so
   `atlas_schema_revisions` is a table in `public`, matching Gombit's ledger
   sync; Atlas's default dedicated revisions schema is not used).
   `--allow-dirty` is required because Gombit creates `framework_migrations`
   before apply, and real apps already have schema tables.
3. Records into `framework_migrations` only the pending versions that appear in
   `atlas_schema_revisions` after apply (`version`, `name`, `batch`,
   `applied_at`; no checksum; D4). This keeps the Gombit ledger aligned with
   Atlas when the two previously diverged.

If nothing is pending relative to `framework_migrations`, migrate prints
`No pending migrations.` and does not invoke Atlas apply.

Unrecognized `*.sql` filenames in the migration directory are skipped with a
warning on stderr.

### Status

`gombit db status` prints Gombit applied/pending rows from the migration
directory plus `framework_migrations`, then runs `atlas migrate status` for
Atlas bookkeeping.

### Rollback

Rollback is Gombit-owned and does **not** wrap `atlas migrate down` (that
command is outside Atlas Community Edition).

`gombit db rollback` rolls back the **latest batch** only:

1. Loads the highest `batch` from `framework_migrations`.
2. Requires a companion down file for every version in that batch (missing
   downs fail before any SQL runs; migrate does not require downs).
3. Executes those down files in reverse version order.
4. Deletes matching rows from `framework_migrations` and
   `atlas_schema_revisions` so a later `gombit db migrate` can re-apply.

On SQLite and PostgreSQL, downs and revision deletes run in one transaction:
a mid-batch failure aborts the transaction and leaves revision rows unchanged.
MySQL DDL often auto-commits, so a mid-batch failure can leave the schema
partially rolled back while revision rows remain; the error lists completed
downs and revision rows are only removed after every down succeeds.

### Down files

Atlas writes up migrations such as:

```text
database/migrations/20260101000000_create_products.sql
```

Gombit-owned down SQL lives in a subdirectory so Atlas never scans it (Atlas
panics if `.down.sql` files sit beside versioned up migrations):

```text
database/migrations/downs/20260101000000_create_products.down.sql
```

If any down file in the latest batch is missing, rollback fails before
executing any down SQL. Migrate does not require downs.

## Seed / Reset

M2-3 adds seeders and a destructive development reset. Both commands read the
configured database from `config.Load()` (`GOMBIT_DATABASE_DRIVER` /
`GOMBIT_DATABASE_DSN`).

```sh
gombit db seed  [--seeds database/seeds]
gombit db reset [--dir database/migrations] [--seeds database/seeds] [--atlas-bin atlas] [--force]
```

### Seed

`gombit db seed` executes every top-level `*.sql` file in the seed directory in
lexical order. Nested subdirectories and non-`.sql` files are skipped with a
warning on stderr. A missing or empty seed directory prints `No seed files.` and
exits successfully.

Each seed file may contain multiple SQL statements separated by `;`. Gombit
splits on semicolons outside single/double quotes and `--` / `/* */` comments,
then executes statements in order (so multi-`INSERT` files work without relying
on driver multi-statement support). **Known limit (v0.1):** the splitter does
not treat MySQL backtick identifiers (`` `ident` ``) or PostgreSQL dollar-quoted
strings (`$tag$...$tag$`) as quoted regions — a `;` inside those forms can
mis-split. Prefer one statement per file, or avoid semicolons inside backticks /
dollar-quotes, until the splitter is extended.

Seed files are application-owned SQL. Keep them idempotent if you plan to run
`seed` more than once against the same database; Gombit does not wrap seeds in a
cross-driver transaction.

Example layout (flat directory only):

```text
database/seeds/01_demo.sql
database/seeds/02_more_data.sql
```

### Reset

`gombit db reset` is drop + migrate + seed:

1. Wipes the configured database using the driver strategy below (including
   Gombit/Atlas revision tables when they live in the wiped scope).
2. Runs `gombit db migrate`.
3. Runs `gombit db seed`.

Driver wipe strategy:

| Driver | Wipe |
| --- | --- |
| SQLite | `DROP` every non-`sqlite_*` table/view from `sqlite_master` (file is not deleted) |
| PostgreSQL | Resets schema `public` only (`DROP SCHEMA public CASCADE` then recreate + grants). Non-`public` schemas are left untouched. |
| MySQL | Disable FK checks, drop every base table/view in the current database, re-enable checks |

Reset refuses to run when `GOMBIT_ENV=production` unless `--force` is set.
Seed alone is allowed in production; apps own seed idempotency.

## Revision metadata

| Column | Notes |
| --- | --- |
| `version` | Atlas migration version prefix |
| `name` | Migration name suffix |
| `batch` | Incremented once per successful migrate that applies files |
| `applied_at` | UTC timestamp when the batch was recorded |

Atlas may still maintain `atlas.sum` and `atlas_schema_revisions` for apply
integrity. That is separate from D4: Gombit does not store checksums in
`framework_migrations`.

## Drivers

`--driver` (makemigrations) accepts the supported v0.1 database drivers:

| Driver | Atlas dev database |
| --- | --- |
| `sqlite` | `sqlite://file?mode=memory&_fk=1` |
| `postgres` | `docker://postgres/15/dev?search_path=public` |
| `mysql` | `docker://mysql/8/dev` |

SQLite runs without Docker. PostgreSQL and MySQL use Atlas dev-database Docker
URLs, so Docker must be available when generating those migrations.

Apply/status convert the configured GORM DSN into an Atlas `--url` for the
same three drivers. Libpq keyword/value DSNs with a slash-prefixed `host`
become a unix-socket URI (`postgres://user@/dbname?host=/path/to/sockets`);
IPv6 hosts are bracketed (`[::1]:5432`). SQLite `file:///abs/path` maps to
`sqlite:///abs/path` (three slashes), not four.

## Model Registration

M2-1 keeps model enumeration explicit: pass feature package models from
`internal/<feature>` with `--model` flags, one per `gombit db makemigrations`
call for whatever's new. `database/migrations/models.json` is the persisted
registry of every model named so far (see [Generate A
Migration](#generate-a-migration)) — you don't repeat earlier ones.

The model spec format is:

```text
<go import path>.<exported model type>
```

For example:

```text
github.com/acme/shop/internal/product.Product
```

The repository example model can be passed with:

```sh
gombit db makemigrations create_products \
  --driver sqlite \
  --model github.com/gombit-dev/gombit/examples/migrations/internal/product.Product
```

This explicit list is the Program Mode equivalent of importing each feature
package and passing concrete model values to Atlas. It avoids runtime
reflection discovery and keeps the loader reviewable.

## Multi-DB Conformance

M2-4 gates official SQLite / PostgreSQL / MySQL support with a conformance
matrix that applies Atlas-generated migrations and exercises CRUD, transactions,
timestamps, nullable/unique/index columns, decimal, pagination, and migrate
up/down. See [`docs/database.md`](database.md#conformance-m2-4).
