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
| `MapLoadError` | `gorm.ErrRecordNotFound` → D10 `not_found` (404); `*database.ValidationError` or a data exception → `validation_error` (422) | `internal` |
| `MapPersistError` | `*database.ValidationError` → `validation_error` (422, with its fields), including a decimal the column would not store exactly (below); unique / duplicate → `conflict` (409); foreign-key or NOT NULL violation, or a data exception → `validation_error` (422) | `internal` |
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

`IsUniqueViolation`, `IsForeignKeyViolation`, `IsNotNullViolation`, and
`IsDataException` are the shared detectors behind those helpers; auth
registration uses `IsUniqueViolation` too. A data exception is one of the
codes a value the client sends can cause, which the client fixes by sending
another value: PostgreSQL's string too long (22001), numeric out of range
(22003), invalid datetime (22007, 22008), NUL byte or invalid UTF-8 (22021,
22P05), and MySQL's out-of-range (1264), truncated (1265), incorrect string
(1366), too long (1406) and incorrect date or time literal (1292). A code does
not say what caused it, so the same code raised by server-side SQL (an
expression overflowing a column, `n + 1` past an `int4`) is a 422 too; the
rest of PostgreSQL's class 22 (division by zero, an invalid cast) stays a 500,
and the database logger keeps logging data exceptions at error level. The
422 carries no `fields`: the driver names a column, if at all, not the field
the API names. The write checks below refuse most of these before the SQL
runs, with the field named, so the driver's refusal is the backstop. See
[`docs/contract.md`](contract.md#application-errors-41-categories).

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
  edit leaves it as stored, whether the request omits it or sends the stored
  value back: `database.StoredValues` records the loaded row, and
  `database.ScopeEdit` scopes the update to leave a column out of its `SET`
  if it still holds the zero after the model's hooks, so a hook that repairs
  it is written. A zero instant written over a real value is refused, in any
  column. The scope covers the update of that row (the pointer passed to
  `ScopeEdit`) only. (`StoredZeroColumns` lists a row's zero columns;
  `KeepStoredZeros` is the deprecated form of the scope.) In your own code a
  `Save` of such a row writes the zero back and is a 422 unless you scope it
  the same way; so is `Updates(&row)` on PostgreSQL, which reads the value back in
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

### Text

`database.Open` also refuses, before the SQL runs on every create and update,
a string a text column cannot store the same way on every driver (issue
#444), with a `*database.ValidationError` naming the field (a 422 through
`MapPersistError`). What a statement writes is decided as for the time range
check, and every string column the Go value is written to as is gets
checked: a `string` or named string type, `*string`, `sql.NullString`. A
field whose value is converted on the way (a GORM `serializer`, a type with
its own `driver.Valuer`) writes something else and is not checked, nor is a
column declared binary (`blob`, `bytea`).

- A NUL byte or invalid UTF-8 (`database.TextProblem`): PostgreSQL refused
  them with a 500; SQLite and MySQL stored a NUL byte.
- More than the column holds, from its declared `type:`, else its `size:`.
  `size:n`, `varchar(n)` and `char(n)` hold n characters (PostgreSQL and
  MySQL refused more, or silently cut trailing spaces past n; SQLite stored
  them). `text` holds `database.TextMaxBytes` (65,535) bytes, MySQL's
  `TEXT`, on every driver (MySQL refused more with a 500); `tinytext` and
  `mediumtext` hold MySQL's capacity too. A string column with neither is
  unlimited text, except that MySQL makes a primary key, indexed, unique or
  defaulted one `varchar(191)`: the check enforces 191 characters for such a
  column on every driver, though PostgreSQL and SQLite would store more. A
  generated `string` field is `size:255` (or its `max_length`), a `text`
  field `type:text`.

NULL (a nil pointer, an invalid `NullString`) and an expression are left to
the database. The check allocates only when a value is refused, or when
`Updates` is given a struct of a type other than the model, which is parsed
to match its fields to the model's columns.

**Rows stored before the check.** A row may already hold text the check now
refuses: more than a `text` or a 191-character column holds (PostgreSQL,
SQLite), more than a `size:n` column holds or invalid UTF-8 (SQLite), a NUL
byte (MySQL, SQLite). An edit scoped to the row
(`database.ScopeEdit` with the row's `database.StoredValues`, taken when it
was loaded) does not judge text the row still stores, compared after the
model's hooks run; text the edit or a hook changed is checked. The admin's
PATCH does this, so such a row stays editable there, including from the
admin's form, which sends every field back; setting the column to other
refused text is a 422. A partial update (`Update`, `Updates` of a map or
struct) does not write the column. A generic `Save` of the row writes the
stored value back and is a 422 on that field: shorten or clean the value, or
scope the save. Invalid UTF-8 does not survive a round trip through JSON: the
API returns it with U+FFFD in place of each invalid byte, so a form that sends
the field back writes that replacement (a change, which is valid text), while
a PATCH that omits the field keeps the stored bytes.

The list helpers apply the same rule to what a client sends: `FilterEq` on a
string column and `Search` refuse a NUL byte or invalid UTF-8 with a 422
(keyed on the column, and on `search`), which PostgreSQL refused to compare
with a 500, and `FilterEq` on a uint column refuses a value over
`math.MaxInt64`, which PostgreSQL's bigint cannot bind; for the same reason a
generated `get`, and the product handler `gombit new` scaffolds, answer an id
over `math.MaxInt64` with a 404. `Search`
therefore takes a context and returns an error, like `FilterEq`; `gombit
generate` emits the new call. The admin data plane refuses them in its
filters, search, many-to-many id lists and writes alike, the last unless the
value is the one the row already stores (sent back unchanged by its form);
its write check also covers a string type with its own `driver.Valuer`,
which the database's text check does not see as text.

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
