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

`email`, `url`, `slug`, and `ip` are Go `string` columns (`varchar(255)`, or
`max_length` when that constraint is set). A required field stays `string`.
An optional one is `*string`: format and pattern reject `""`, so a blank is
null, the same way an optional date is. The model stores `format:"email"`,
`format:"uri"`, `format:"ip"`, or the slug `pattern`. `gombit generate`
copies that onto the request, where Huma checks it. The form uses
`type="email"` and `type="url"` and submits null for a blank optional value.
A slug checks `^[-a-zA-Z0-9_]+$`. Admin keeps the `string` widget and rejects
a value that fails the same format or pattern. Email and slug are searchable.
URL and IP are exact values, so they are sortable and not searchable.

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

## Constraints

Modifiers after the type keep their meaning: `required`, `nullable`, `unique`,
`index`, `filterable`, `sortable`, `searchable`, `aggregatable`. These add
bounds and a default:

| Modifier | Column types | Where it lands |
| --- | --- | --- |
| `min=`, `max=` | `int`, `int64`, `uint` | GORM `check`, request `minimum` / `maximum`, form `min` / `max`, admin meta and the admin write. The token must be an integer in ±(2^53−1). The request and the form compare it as a number, and past that range a number comparison accepts integers the SQL check rejects. The column name cannot be reserved in SQLite, PostgreSQL, or MySQL (`order`, `group`, `select`): the check is unquoted, and no one quote character is valid SQL on all three |
| `min=`, `max=` | `decimal` | GORM `check`, and a create-body check that compares decimal magnitudes. The token must match the decimal schema (`^-?[0-9]+(\.[0-9]+)?$`) and fit the column: fractional digits ≤ scale, integer digits ≤ precision−scale. A bare `decimal` is `decimal(19,4)`. The request does not advertise `minimum` / `maximum`. The same reserved-name rule as an integer check applies |
| `max_length=` | `string` | GORM `size` (otherwise 255), request `maxLength`, the form, and the admin write. Each one counts Unicode code points |
| `regex=` | `string`, `text` | unanchored request `pattern` and the form's `new RegExp(..., "u")` check, including `text`. The pattern must compile in Go RE2. Escapes are an allowlist (`\d` `\D` `\w` `\W`, `\n` `\r` `\t` `\f` `\v`, `\0` for NUL, two-digit `\xNN`, and escaped syntax characters). `\b` and `\B` are rejected: JavaScript finds a word edge between the two surrogates of a non-BMP character. `\a`, octal, `\x{HHHH}`, `\s`, `\p`, inline flags, and POSIX classes are rejected. A `]` that opens a class is rejected, because RE2 treats it as a member and JavaScript closes an empty class. An unescaped `]` outside a class is rejected (`\]` is the literal). A quantifier may not have a leading zero (`{01}`, `{00}`), and it may not follow `^` or `$`. A `-` inside a class is a range only between single characters; `\d` or `\w` on either side is rejected. A hyphen that is first or last stays a literal. The form rewrites `.` to `[^\n]` under the `u` flag, so both sides match one code point and every character except newline. The form does not set an HTML `pattern` attribute, because that attribute anchors the match. Not a SQL check |
| `default=` | scalars and `enum` | Applied when the create body or the admin create omits the field. An explicit `0`, `false`, or `""` is stored. A decimal default is a string matching the decimal schema and the column precision and scale. The form and admin create start from that value. Enum values are stored on the model's `validate` tag so `gombit generate` can emit them |

Values are case-sensitive. A CLI `regex` cannot contain a comma, because
modifiers are comma-separated. The model stores the same facts in a `validate`
tag separated by semicolons (`min=0;max=150`), which `gombit generate` and
admin meta read. `references=` stays unsupported.

```
age:int:required,min=0,max=150
status:enum(draft,published):default=draft
```
