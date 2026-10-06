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

| Helper | Maps | Anything else |
| --- | --- | --- |
| `MapLoadError` | `gorm.ErrRecordNotFound` → D10 `not_found` (404) | `internal` |
| `MapPersistError` | `*database.ValidationError` → `validation_error` (422, with its fields); unique / duplicate → `conflict` (409); foreign-key or NOT NULL violation → `validation_error` (422) | `internal` |
| `MapDeleteError` | `database.ErrReferenced` or a foreign-key violation → `conflict` (409) | `internal` |

`IsUniqueViolation`, `IsForeignKeyViolation`, and `IsNotNullViolation` are the
shared detectors behind those helpers; auth registration uses
`IsUniqueViolation` too. See [`docs/contract.md`](contract.md#application-errors-41-categories).

### Timestamp and date range

`database.Open` and `OpenConn` refuse, before the SQL runs on every create and
update, a `time.Time`, `sql.NullTime` or `types.Date` value the statement writes
that is outside what all three drivers can store and return:
`1000-01-02T00:00:00Z`..`9999-12-30T23:59:59Z` for a timestamp and
`1000-01-01`..`9999-12-31` for a date (`types.TimeBounds`, `types.DateBounds`).
MySQL stores no earlier year, and a timestamp outside years 0..9999 in the time
zone it is read into cannot be encoded as JSON; PostgreSQL would store year 0
as 1 BC and then fail every read of the row. The timestamp bounds keep a day's
margin for that time-zone conversion. The failure is a
`*database.ValidationError` naming the field, so `MapPersistError` answers it
with a 422 on the generated API, the admin data plane, and your own code alike.

What a statement writes is what GORM writes:

- `Create`, `Save`, `Updates` with a map or a struct (of any type, matched to
  the model's columns by name), `Update`, `UpdateColumn(s)`, and the literal
  assignments of an upsert's `ON CONFLICT DO UPDATE`, filtered by
  `Select` / `Omit` the way GORM filters them (on a create an auto
  `CreatedAt` / `UpdatedAt` is written whatever `Select` says). An update's
  model is not checked unless it is what is written (`Save`, `Updates(&row)`),
  so updating other columns of a row that already holds an out-of-range value
  is not refused.
- A column GORM's field permissions keep out of the statement
  (`gorm:"->"`, `<-:create` on an update, `<-:update` on a create) is not
  written, and not checked.
- Every `time.Time`, `sql.NullTime`, `types.Date` and named time type
  (`gorm.DeletedAt`, `type Stamp time.Time`) column is checked, a caller-set
  `CreatedAt` / `UpdatedAt` / `DeletedAt` included (a hook, a seeder, a map
  naming the column, `UpdateColumns`). A struct update that runs hooks writes
  now over `UpdatedAt`, so that one is not.
- The zero instant `0001-01-01T00:00:00Z` is refused wherever a statement
  writes it, on every driver, whatever `Location` the value carries: a zero
  non-pointer field on create, a zero value in a map or `Update`, a non-nil
  pointer to it or a valid `sql.NullTime` holding it, and a zero struct field
  an update writes. GORM decides that: an update writes a zero struct field
  only when `Select` puts its column in GORM's select map (a field or column
  name, `table.col`, a quoted name, `*`, `table.*`), and `Updates` skips it
  otherwise. GORM's zero is the Go zero value, `time.Time{}`; a zero time
  carrying a `Location` (`.Local()`, a pgx read, a parse with an explicit
  offset such as `+00:00`) is written, and refused. Use a pointer (or
  `sql.NullTime`) for an optional time.
- An unset auto timestamp or `default` column is not a value to refuse. On a
  create GORM fills a zero `CreatedAt` / `UpdatedAt` and the database a zero
  defaulted column, `Select` or not. Under `Select("*")` (`Save`) an update
  leaves such a column out of its `SET` when it holds the zero instant, so
  `Save` of a struct that does not carry `CreatedAt` keeps the row's, and
  when no row matches, `Save`'s insert fallback fills it as any create does.
  NULL (a nil pointer, an invalid `sql.NullTime`) is a value and is written.
  Naming the column without `*` (`Select("CreatedAt")`) writes the zero,
  and is refused; under a select that includes `*` the column is left out.
- A row stored before this check with the zero instant in a non-pointer
  column (an unset field on SQLite or PostgreSQL, `'0000-00-00'` on MySQL)
  can still have its other columns changed by `Update` and a partial
  `Updates`. The admin's PATCH keeps such a column, mapped or not, when the
  request does not set it: `database.StoredZeroColumns` lists the loaded
  row's zero columns, and `database.KeepStoredZeros` scopes the update to
  leave one out of its `SET` if it still holds the zero after the model's
  hooks, so a hook that repairs it is written. A zero the request sets is
  refused, in any column. The scope covers the update of that row (the
  pointer passed to `KeepStoredZeros`) only. In your own code a `Save` of
  such a row writes the zero back and is a 422 unless you scope it the same
  way; so is `Updates(&row)` on PostgreSQL, which reads the value back in
  `Local` (GORM then writes it), while on SQLite and MySQL GORM skips it. To
  clean the rows up, make the field a pointer and
  `UPDATE … SET col = NULL WHERE col = '0001-01-01 00:00:00'`, or give the
  column a real value.
- A string written to such a column is read as the drivers read it (RFC 3339,
  ISO without a zone, `YYYY-MM-DD hh:mm:ss` with an offset such as `+00`,
  `+0000`, `-03:00` or a zone name, `YYYY-MM-DD`); one that does not parse is
  refused, since PostgreSQL accepts forms (`'infinity'`, `'… BC'`) no Go time
  can be read back from. An expression (`gorm.Expr`) and NULL are left to the
  database.

The check costs an ordinary write nothing: it allocates only when a value is
out of range.

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

Set `Config.Database.MaxOpenConns`, `MaxIdleConns`, or `ConnMaxLifetime` (the
`GOMBIT_DATABASE_*` variables in [`docs/config.md`](config.md#environment)) to
override these defaults.

The default SQLite DSN writes `gombit.db` in the current working directory.
`gombit doctor` flags a SQLite path whose directory is missing or not writable
(the `insecure` row).

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
- relation deletion (`relation_deletion`): `ON DELETE` `RESTRICT` / `CASCADE` /
  `SET NULL` through `database.Delete`

`TestDatabaseCheck` runs `gombit db check`'s database-schema layer on each
driver: a migrated database matches its migrations, and a column added outside
a migration is reported.

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
