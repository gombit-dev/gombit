# Logical fields

Gombit has one field vocabulary: `package field` (`field/kind.go`). A **kind**
is the domain type. The resource grammar and the admin meta API are
projections of that kind, not separate lists.

| Kind | Generated today | CLI tokens | Admin meta `type` | Go type |
| --- | --- | --- | --- | --- |
| `string` | yes | `string` | `string` | `string` |
| `text` | yes | `text` | `text` | `string` |
| `integer` | yes | `int`, `integer` | `integer` | `int` |
| `integer64` | yes | `int64`, `integer64` | `integer` | `int64` |
| `unsigned` | yes | `uint`, `unsigned` | `integer` | `uint` |
| `float` | yes | `float`, `float64` | `float` | `float64` |
| `decimal` | yes | `decimal` | `decimal` | `types.Decimal` |
| `boolean` | yes | `bool`, `boolean` | `boolean` | `bool` |
| `date` | yes | `date` | `date` | `types.Date` |
| `datetime` | yes | `time`, `datetime` | `datetime` | `time.Time` |
| `time` | no | | | clock time, not the `time` token |
| `duration` | no | `duration` | | |
| `uuid` | yes | `uuid` | `uuid` | `uuid.UUID` |
| `json` | yes | `json` | `json` | `types.JSON` (required), `types.NullJSON` (optional) |
| `email` | yes | `email` | `string` | `string`, OpenAPI `format: email` |
| `url` | yes | `url` | `string` | `string`, OpenAPI `format: uri` |
| `slug` | yes | `slug` | `string` | `string`, pattern `^[-a-zA-Z0-9_]+$` |
| `ip` | yes | `ip` | `string` | `string`, OpenAPI `format: ip` |
| `enum` | yes | `enum(a,b)` | `string` | `string` |
| `relation` | yes | `belongs_to`, `has_many`, `many_to_many` | `relation` | |

`time` on the command line is a **datetime** (`time.Time`), kept as a
compatibility alias. `date` is a calendar date (`types.Date`, JSON
`YYYY-MM-DD`). `datetime` is a timestamp. The clock-time kind and `duration`
stay in the vocabulary without a generator token until a later issue.

`uuid` is stored as `char(36)` so SQLite, PostgreSQL, and MySQL share one
column type. `json` is text holding a JSON object or array. A required column
is `types.JSON`, whose contract rejects null. An optional column is
`types.NullJSON`, which also accepts null. `float` is `float64` (sortable,
not aggregatable). Optional `date` and `uuid` are pointers; their OpenAPI
schema is nullable and a blank form submits null.

`integer64` and `unsigned` share the admin `integer` widget. The meta payload
does not change.

`email`, `url`, `slug`, and `ip` are Go `string` columns (`varchar(255)`).
The model stores `format:"email"`, `format:"uri"`, `format:"ip"`, or the slug
`pattern`. `gombit generate` copies that onto the request, where Huma checks
it. The form uses `type="email"` and `type="url"`; a slug checks the same
pattern. Admin keeps the `string` widget.

## Adding a kind

1. Add a `Kind` constant and one `catalog` entry in `field/kind.go` (Go type,
   CLI tokens, admin wire string, filter/search/sort/aggregate flags).
2. Set `GeneratorReady` only when `gombit make resource` emits the kind.
   Until then the token is rejected as not generated yet.
3. A new admin wire string needs an admin widget in the same change. Reusing
   an existing wire (the way `email` reuses `string`) does not.
4. Add a row to the table above.

A relation cardinality is not a new kind. Add a `RelationKind`, a `relationCaps`
row, and that token on the `Relation` catalog entry. Resourcegen still switches
on the cardinality when it emits the association. Filter, search, sort, and
aggregate flags on the catalog entry are the policy both the CLI grammar and
`gombit generate` run.

Resourcegen and admin read this catalog. They do not keep their own copies of
the overlapping names (`int` / `integer`, `bool` / `boolean`, `time` /
`datetime`).
