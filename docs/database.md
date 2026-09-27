# Database

M1-4 introduces the runtime database boundary. Gombit opens GORM directly and
keeps `*gorm.DB` reachable; it does not define a second ORM interface.

Migration generation is tracked separately in
[`docs/migrations.md`](migrations.md) and
[`docs/adr/012-migrations-atlas-gorm-provider.md`](adr/012-migrations-atlas-gorm-provider.md):
M2 wraps Atlas and `ariga.io/atlas-provider-gorm` rather than defining a
Gombit migration DSL.

## Drivers

`database.Open` supports:

| Driver | Config value |
| --- | --- |
| SQLite | `sqlite` |
| PostgreSQL | `postgres` |
| MySQL | `mysql` |

The MySQL dialector uses GORM's default `mysql.Open` so it probes
`SELECT VERSION()` and sets capability flags (`DontSupportRenameColumn`,
`DontSupportDropConstraint`, …) for MySQL 5.7 / MariaDB. It does **not**
set `SkipInitializeWithVersion`, which would leave those flags at the
MySQL 8 defaults and emit `RENAME COLUMN` / `DROP CONSTRAINT` that older
servers reject.

The opened handle embeds `*gorm.DB` and exposes driver metadata:

```go
db, err := database.Open(cfg.Database)
if err != nil {
	return err
}
defer db.Close()

fmt.Println(db.Driver())
fmt.Println(db.Capabilities().Returning)
```

To open over a `database/sql` handle you built yourself (a driver wrapped for
tracing, or, in tests, for fault injection), use `database.OpenConn`. The DB
gets the same GORM setup as from `Open` (error translation, model `Validate`
hooks); you own the handle's pool settings (`database.ConfigurePool` applies
`Open`'s), and `db.Close()` closes it:

```go
conn := sql.OpenDB(instrumented) // your driver.Connector
db, err := database.OpenConn(database.DriverPostgres, conn)
```

`framework.App` can receive an opened handle through `framework.WithDatabase`;
the caller owns opening and closing that handle. `app.Database()` returns the
metadata handle and `app.DB()` returns the raw `*gorm.DB` escape hatch.
HTTP-only apps can omit `WithDatabase`.

## Error mapping

`database.Open` (and `OpenConn`) enable `gorm.Config.TranslateError`, so each
dialector maps its unique-violation code to `gorm.ErrDuplicatedKey`; the
driver's error string stays a fallback. Callers should not inspect either
themselves:

```go
if err := db.Create(&row).Error; err != nil {
	return database.MapPersistError(ctx, err, "resource already exists", "create widget")
}
if err := db.First(&row, id).Error; err != nil {
	return database.MapLoadError(ctx, err, "widget not found", "load widget")
}
```

| Helper | `gorm.ErrRecordNotFound` | unique / duplicate | other |
| --- | --- | --- | --- |
| `MapLoadError` | D10 `not_found` | `internal` | `internal` |
| `MapPersistError` | `internal` | D10 `conflict` | `internal` |

`IsUniqueViolation` is the shared detector used by those helpers and by
auth registration. See [`docs/contract.md`](contract.md#application-errors-41-categories).

## Deleting rows

Gombit deletes rows physically: its deletion semantics are the database's
([ADR-019](adr/019-hard-delete-semantics.md)). A relation's `on_delete` becomes
the foreign key's `ON DELETE`, and deleting the parent does exactly that, in one
statement the database enforces:

| `on_delete` | Deleting a referenced parent |
| --- | --- |
| `restrict` (default) | refused; `database.ErrReferenced`, a `409 conflict` through `MapDeleteError` |
| `cascade` | the referencing rows are deleted too |
| `set_null` | the referencing rows keep their data with the key set to NULL |

`gombit make resource` generates models without soft delete (an `ID`,
`CreatedAt`, `UpdatedAt`, no `DeletedAt`), so GORM's own `Delete` on them is a
real `DELETE`. `database.Delete` is the framework's delete, and the admin uses
it:

```go
n, err := database.Delete(ctx, db, &book.Book{}, id)
if errors.Is(err, database.ErrReferenced) {
	// another row still references it (ON DELETE RESTRICT)
}
return database.MapDeleteError(ctx, err, "book is still referenced", "delete book")
```

It deletes even a model that embeds `gorm.DeletedAt` (`gorm.Model`). GORM's own
`Delete` on such a model only sets `deleted_at`, so no foreign key fires and a
live row keeps pointing at one the API reports as gone. There is no restore, and
Gombit does not emulate foreign keys on soft-deleted rows.

**Moving an existing `gorm.Model` resource to hard delete.** Replace the
embedded `gorm.Model` with `` ID uint `gorm:"primaryKey" json:"id"` ``,
`CreatedAt time.Time`, and `UpdatedAt time.Time`. First decide what happens to
the rows that are already soft-deleted: remove them
(`DELETE FROM books WHERE deleted_at IS NOT NULL`) or clear their
`deleted_at` to bring them back. Then generate the migration; it drops
`deleted_at`, a destructive step you acknowledge:

```sh
gombit db makemigrations drop_books_deleted_at --allow drop_column:books.deleted_at
```

## Capabilities

`database.Capabilities` captures driver differences that affect generated code
and migrations:

| Capability | SQLite | PostgreSQL | MySQL |
| --- | --- | --- | --- |
| Transactions | yes | yes | yes |
| Savepoints | yes | yes | yes |
| Foreign key constraints | yes | yes | yes |
| Returning | yes | yes | no |
| Upsert | yes | yes | yes |
| Advisory locks | no | yes | no |
| Concurrent index builds | no | yes | no |

## Pool Defaults

If pool settings are left at zero, `database.Open` applies driver-aware
defaults:

| Driver | Max open | Max idle | Connection max lifetime |
| --- | ---: | ---: | --- |
| SQLite | 1 | 1 | none |
| PostgreSQL | 25 | 5 | 30m |
| MySQL | 25 | 5 | 30m |

Set `Config.Database.MaxOpenConns`, `MaxIdleConns`, or `ConnMaxLifetime` to
override these defaults.

The default SQLite DSN writes `gombit.db` in the current working directory.
Production checks for unwritable SQLite paths are tracked with the later
Appendix C hardening work.

## Integration Tests

The default unit suite exercises SQLite without external services. Postgres and
MySQL open round-trips can be run with the `integration` build tag and explicit
DSN flags:

```sh
go test -tags integration ./database \
  -database.postgres-dsn 'postgres://gombit:gombit@127.0.0.1:5432/gombit?sslmode=disable' \
  -database.mysql-dsn 'gombit:gombit@tcp(127.0.0.1:3306)/gombit?parseTime=true'
```

## Conformance (M2-4)

Official multi-DB support is gated by the conformance suite under
`database/conformance`. It generates a versioned migration with
`migrations.MakeMigrations` (Atlas Community Edition), applies it with
`migrations.Migrate`, then asserts portable behavior on each driver:

- migrate up / migrate down (Gombit-owned companion downs)
- timestamps, nullable columns, unique constraints, indexes
- decimal round-trip
- CRUD, transactions, pagination (`Offset` / `Limit`)

The suite uses the `conformance` build tag so default `go test ./...` stays
offline. Install Atlas Community Edition and set `ATLAS_BINARY` (or have
`atlas` on `PATH`). Postgres and MySQL makemigrations also need Docker for
Atlas `docker://` dev databases.

SQLite (temp file DSN when `-conformance.dsn` is empty):

```sh
go test -tags conformance ./database/conformance \
  -conformance.driver sqlite -count=1
```

Postgres / MySQL:

```sh
go test -tags conformance ./database/conformance \
  -conformance.driver postgres \
  -conformance.dsn 'postgres://gombit:gombit@127.0.0.1:5432/gombit?sslmode=disable' \
  -count=1

go test -tags conformance ./database/conformance \
  -conformance.driver mysql \
  -conformance.dsn 'gombit:gombit@tcp(127.0.0.1:3306)/gombit?parseTime=true' \
  -count=1
```

CI runs the same checks as three jobs in `.github/workflows/ci.yml`
(`Conformance (sqlite|postgres|mysql)`), each with only the DB service it needs.
See also [`docs/migrations.md`](migrations.md) for the Atlas migration workflow.
