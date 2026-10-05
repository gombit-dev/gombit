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
| `MapPersistError` | `*database.ValidationError` → `validation_error` (422, with its fields), including a decimal the column would not store exactly (below); unique / duplicate → `conflict` (409); foreign-key or NOT NULL violation → `validation_error` (422) | `internal` |
| `MapDeleteError` | `database.ErrReferenced` or a foreign-key violation → `conflict` (409) | `internal` |

Before every create and update, `database.Open` also checks each value the
statement assigns to a decimal column (a `types.Decimal` / `decimal.Decimal`
field): a value with more digits than the column holds, before or after the
point, or one that is not a decimal number at all, is a
`*database.ValidationError` naming the field, not a rounded, truncated, or
garbage write.

- **What is checked** is the assignment set, the columns the statement writes:
  a create's rows (or map) and an upsert's explicit `ON CONFLICT DO UPDATE`
  values; an update's map (`Updates(map)`, `Update(column, value)`) or the
  struct it writes (`Save`, `Updates(struct)`, any struct type, by column
  name), honoring `Select` and `Omit`. An update that does not write a decimal
  column is never refused for the row's old value, and a model with no decimal
  field costs nothing.
- **Every value shape** is read: decimals, strings, numbers, `json.Number`,
  pointers (a `*string` / `*float64` PATCH field), named types, and
  `driver.Valuer`s such as `sql.NullString`. A string must be a plain decimal
  (`[sign] digits [. digits] [e [sign] digits]`, `types.IsPlainDecimalSpelling`),
  the grammar all three databases read, optionally padded with the ASCII
  whitespace they skip; anything else, Unicode spaces (`"1.5\u00a0"`) or a
  sign after a leading point (`".-5"`, which Go's decimal parser accepts)
  included, is not a decimal number. A null is
  nothing to check, and a SQL expression (`gorm.Expr`) or an upsert's column
  reference is left to the database.
- **The column's limits** come from the type GORM emits for the model field
  (`GormDBDataType`, else the dialect's `DataTypeOf`), the same type
  `AutoMigrate` and the Atlas provider create, so the model's declared type
  must match the migrated column. A decimal type without `(p,s)` is
  `DECIMAL(10,0)` on MySQL and an unbounded `numeric` on PostgreSQL; a
  `precision:`/`scale:` tag does not reach the column for `types.Decimal`; an
  `unsigned` column refuses a negative value; a text column stores the bytes
  it is given as written, so a string bound for one must already be the
  decimal's canonical spelling (`"1.5"`, not `" 1.5 "`, `"1.50"`, or
  `"15e-1"`), a plain number (integer or float) is refused, and the spelling
  must fit the column's
  length (`varchar(n)`); any other type (`real`,
  `double precision`, `money`, `bigint` cents) fails every write with an
  error rather than an unchecked one. Declare such a field `decimal(p,s)`.
- **On SQLite**, which stores decimals through float64, a value with more than
  15 digits (`database.SQLiteDecimalDigits`), or outside about 1e±307, is
  refused the same way.
- **On every driver**, a value whose exponent is beyond ±1000, or that would
  spell more than `types.MaxDecimalDigits` (1000) digits, is refused before
  anything formats or compares it. This is one rule (`types.DecimalShapeOf`)
  shared by the write guard and `types.Decimal`'s parsing and `Scan`, so a
  value the guard approves always loads again, and text longer than any
  bounded value could spell is refused before it is even parsed. A decimal's exponent is unbounded, and
  formatting or comparing one rescales it to that exponent, zero included:
  `"0e1000000000"` costs as much as `"1e1000000000"`. `types.Decimal` refuses
  such a value when it is unmarshalled (`UnmarshalJSON`, `UnmarshalText`,
  `NewDecimalFromString`) or scanned, and the admin refuses it before comparing
  it to a bound.

The check runs on the API and admin write paths alike.

`IsUniqueViolation`, `IsForeignKeyViolation`, and `IsNotNullViolation` are the
shared detectors behind those helpers; auth registration uses
`IsUniqueViolation` too. See [`docs/contract.md`](contract.md#application-errors-41-categories).

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
- decimal round-trip, and precision: a `decimal(19,4)` value round-trips
  exactly, a value over the scale or not a decimal at all is refused on every
  driver (on create, upsert, and the update paths, whatever Go value carries
  it), one over 15 digits is refused on SQLite, and an undeclared-precision
  decimal is MySQL's `DECIMAL(10,0)`
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
