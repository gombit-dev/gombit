# ADR-019: Deletion is the database's deletion

- Status: Accepted
- Issue: [#312](https://github.com/gombit-dev/gombit/issues/312) (SCHEMA-5, epic #277)
- Related: #220 (admin hard delete), MODEL-5 (#305, `on_delete` declaration)

## Context

A relation declares what deleting its parent does: `on_delete=restrict`,
`cascade`, or `set_null` becomes the foreign key's `ON DELETE` clause. GORM's
`gorm.Model` embeds `DeletedAt`, and a model that has it is *soft*-deleted: GORM
runs `UPDATE ... SET deleted_at = now()` instead of `DELETE`. The database sees
no delete, so no foreign key fires. `RESTRICT` protects nothing, `CASCADE`
leaves the children, `SET NULL` leaves them pointing at a parent the API reports
as gone. The schema then states a rule the runtime does not follow.

Emulating the three policies on top of soft delete would move them from the
database into application code: recursive cascades, restore semantics, and the
races between a check and the write, with any other writer (a script, another
service) free to break them.

## Decision

Gombit's deletion semantics are the database's deletion semantics.

- Deleting a row is a physical `DELETE`. `RESTRICT` / `NO ACTION` refuse it,
  `CASCADE` removes the referencing rows, `SET NULL` clears their key, in one
  statement enforced by the database.
- `gombit make resource` generates models without soft delete: an explicit
  `ID` (auto-increment `uint`, or an application-assigned `uuid.UUID`),
  `CreatedAt`, and `UpdatedAt`, and no `DeletedAt`. A GORM `Delete` on such a
  model is a real `DELETE`.
- `database.Delete(ctx, db, value, conds...)` is the framework's delete. It
  issues a real `DELETE` even for a model that embeds `gorm.DeletedAt`, and a
  refusal wraps `database.ErrReferenced`, which `database.MapDeleteError` maps
  to a D10 `409 conflict`. The admin data plane deletes through it.
- Gombit does not emulate foreign keys on soft-deleted rows and has no restore.
  A future archival feature would be designed on its own terms, not by
  changing what `DELETE` means.

## Consequences

- One source of truth: the generated model, the SQL schema, and runtime
  deletion agree, on SQLite, PostgreSQL, and MySQL (the conformance suite
  covers each policy and a `gorm.Model` parent).
- There is no app-side pre-check to race: the constraint is the check.
- Existing models that embed `gorm.Model` keep compiling. App code that calls
  GORM's `db.Delete` on them still soft-deletes and still bypasses the foreign
  keys; `database.Delete` does not. Moving such a model to hard delete is a
  model edit plus a migration (see docs/database.md).
- Deleted rows are gone. Apps that need history keep it explicitly (an audit
  table, an archived flag with its own rules), not through `DELETE`.
