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
| `time` | yes | `time_of_day` | `time` | `types.TimeOfDay`, JSON `HH:MM:SS` |
| `duration` | yes | `duration` | `duration` | `types.Duration`, JSON Go duration (`1h30m0s`), column bigint nanoseconds |
| `uuid` | yes | `uuid` | `uuid` | `uuid.UUID` |
| `json` | yes | `json` | `json` | `types.JSON` (required), `types.NullJSON` (optional) |
| `email` | yes | `email` | `string` | `string`, OpenAPI `format: email` |
| `url` | yes | `url` | `string` | `string`, OpenAPI `format: uri` |
| `slug` | yes | `slug` | `string` | `string`, pattern `^[-a-zA-Z0-9_]+$` |
| `ip` | yes | `ip` | `string` | `string`, OpenAPI `format: ip` |
| `enum` | yes | `enum(a,b)` or `enum(draft=Draft)` | `string` | `string`; the label is display-only |
| `file` | yes | `file` | none yet (left out of the admin) | `types.File`, the object key; reads return a file object |
| `image` | yes | `image` | none yet (left out of the admin) | `types.Image`, as `file` with image types only |
| `relation` | yes | `belongs_to`, `has_many`, `many_to_many`, `one_to_one` | `relation` | |

`time` on the command line is a **datetime** (`time.Time`), kept as a
compatibility alias. `date` is a calendar date (`types.Date`, JSON
`YYYY-MM-DD`). `datetime` is a timestamp. `time_of_day` is a clock
(`types.TimeOfDay`, JSON `HH:MM:SS`) stored as `char(8)` so SQLite,
PostgreSQL, and MySQL share one sortable text form. The request pattern,
the admin write, and `types.TimeOfDay` accept the same spellings: `HH:MM`,
`HH:MM:SS`, and a clock with a numeric offset (`15:04:05+07:00`). All three
are stored as `HH:MM:SS`. This is not OpenAPI format `time`, which rejects
`HH:MM`. `duration` is a Go duration string
(`1h30m`, `300ms`, `0s`) stored as a signed bigint of nanoseconds, which
sorts the same way on every supported driver. Zero duration is `0s`.
Midnight is `00:00:00`. An optional field of either kind is a pointer, and
a blank form submits null, because both formats reject `""`.

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
An optional one is `*string` when the format or the slug pattern rejects
`""`, the same way an optional date is. A user regex becomes a pointer only
when it does not match `""`; `^[a-z]*$` stays `string`. A format that accepts
`""`, such as `uri-reference`, keeps `""`. A `*string` with no `omitempty`
stays nullable, because Huma sets that from the pointer. A `*string` with
`omitempty` and no `nullable:"true"` does not: Huma clears nullability.
`gombit generate` adds `omitempty` when a default is set, and adds
`nullable:"true"` when `""` is illegal, so email, URL, slug, and IP stay
nullable. A `uri-reference` with a default is `omitempty` and not nullable:
JSON null is rejected and `""` is stored. `gombit generate` copies the
column's Go type. A plain `string` is not a pointer, and Huma's format check
rejects `""` for every format it knows except `uri-reference`,
`iri-reference`, `uri-template`, `json-pointer`, and `regex`. The model
stores `format:"email"`, `format:"uri"`, `format:"ip"`, or the slug
`pattern`. The form uses `type="email"` and `type="url"` and submits null
for a blank optional pointer. A slug checks `^[-a-zA-Z0-9_]+$`. Admin keeps
the `string` widget and rejects a value that fails the same format or
pattern. The generated create body rejects `""` when the format or pattern
does. Null and an omitted field apply the default. The form submits null, so
a blank input hits that default. Admin rewrites `""` to null on a pointer
column whose format or pattern rejects it, then uses the same default on
create. A later PATCH of `""` stores null, because the default applies only
on create. On a plain string, `""` is rejected when the format or pattern
rejects it. Email and slug are searchable. URL and IP are exact values, so
they are sortable and not searchable.

## Storage-backed fields

`file` and `image` store a file in `App.Storage()` (see [storage.md](storage.md)),
never in a SQL column. The column holds the object key:

```sh
gombit make resource Document title:string:required attachment:file:required cover:image
```

```go
Attachment types.File   `gorm:"size:512;not null;uniqueIndex" storage:"prefix=document/attachment/"`
Cover      *types.Image `gorm:"size:512;uniqueIndex" storage:"prefix=document/cover/"`
```

**The policy.** The `storage` tag sets what `gombit generate` puts in the
field's upload policy:

| Setting | Meaning | Default |
| --- | --- | --- |
| `prefix=` | The key prefix the field owns | `<package>/<column>/` |
| `max_bytes=` | Largest file | 10 MiB |
| `types=` | Accepted media types, comma-separated (`image/*` style patterns allowed) | any (`*/*`) for `file`; PNG, JPEG, GIF, WebP for `image` |

The types are detected from the file's bytes. `image` never accepts SVG, which
can carry script. An unknown setting is an error, so a typo does not loosen a
limit.

**The API.** The generated handler has one extra operation per file field, and
file fields change the create and read shapes:

- `POST /<resources>/uploads/<field>` takes `{size, content_type, filename}`.
  It checks the declared size and type against the policy, then returns
  `{key, upload}`: a signed, single-use upload request, 15 minutes by default.
  See [direct uploads](storage.md#direct-uploads).
- The client sends the bytes with that request, straight to storage.
- The create body carries the `key`: a string, or `null` for an optional file
  with none. The create is refused unless:
  - the key is under the field's prefix (otherwise 422);
  - the upload exists and passes the policy, detected by its bytes (otherwise
    422, and a refused file is deleted);
  - no other record holds it (otherwise 409).
- Reads return a file object: `key`, `filename`, `size`, `content_type`, and
  `url`. The URL is permanent for keys under the public prefix and signed for
  15 minutes otherwise. `missing` is `true` when the store no longer has the
  file.

Every read stats each file, so a list page makes one storage request per file
per row.

**Ownership.** The column's unique index means one record per file, and the
field's prefix is what it owns. That is the contract
`storage.DeleteOwned` and `storage.Sweep` rely on: a file is never deleted
from under another record. Uploads granted but never attached are removed by
`storage.Sweep(ctx, store, "<prefix>", age, filefield.ReferencedBy(db, &Model{}, "<column>"))`.

**Who may upload.** Uploading is as open as create. A `BeforeUpload(ctx, field)`
method on the resource's hooks decides who may have a grant; the seeded
`hooks.go` of a resource with file fields has one. On an app reachable by
anyone, restrict it (to signed-in users, say), and run the sweep below: each
grant lets its holder store up to the field's `max_bytes`, and an upload
that is never attached stays stored until swept.

**Frontend and admin.**
- The generated form has a file input per field. On submit it uploads the file
  and sends its key.
- The list links each file to its URL.
- The admin leaves file columns out until it has a file widget (STORAGE-8).
  Meanwhile, a required file column cannot be set there.

[`examples/storage`](../examples/storage/internal/document/document.go) has a
`Document` resource with both kinds.

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
| `max_length=` | `string`, `email`, `url`, `slug`, `ip` | GORM `size` (otherwise 255), request `maxLength`, the form, and the admin write. Each one counts Unicode code points |
| `regex=` | `string`, `text` | unanchored request `pattern` and the form's `new RegExp(..., "u")` check, including `text`. The pattern must compile in Go RE2. Escapes are an allowlist (`\d` `\D` `\w` `\W`, `\n` `\r` `\t` `\f` `\v`, `\0` for NUL, two-digit `\xNN`, and escaped syntax characters). `\b` and `\B` are rejected: JavaScript finds a word edge between the two surrogates of a non-BMP character. `\a`, octal, `\x{HHHH}`, `\s`, `\p`, inline flags, and POSIX classes are rejected. A `]` that opens a class is rejected, because RE2 treats it as a member and JavaScript closes an empty class. An unescaped `]` outside a class is rejected (`\]` is the literal). A quantifier may not have a leading zero (`{01}`, `{00}`), and it may not follow `^` or `$`. A `-` inside a class is a range only between single characters; `\d` or `\w` on either side is rejected. A hyphen that is first or last stays a literal. The form rewrites `.` to `[^\n]` under the `u` flag, so both sides match one code point and every character except newline. The form does not set an HTML `pattern` attribute, because that attribute anchors the match. Not a SQL check |
| `default=` | `string`, `text`, `email`, `url`, `slug`, `ip`, `int`, `int64`, `uint`, `decimal`, `bool`, `duration`, `time_of_day`, `enum` (not `float`, `date`, `datetime`, `uuid`, or `json`) | Applied when the create body omits the field or sends null, and when the admin create omits the field or sends null. The generated request rejects `""` before the mapper when the format or pattern rejects it; that body does not treat `""` as omitted. Admin rewrites `""` to null on a pointer string whose format or pattern rejects it, then applies the default. An explicit `0`, `false`, or a legal `""` is stored. A decimal default is a string matching the decimal schema and the column precision and scale. A duration default is a Go duration (`30m`). A time-of-day default is `HH:MM:SS`. The form submits null for a blank optional pointer, and admin create starts from the default. Enum values are stored on the model's `validate` tag so `gombit generate` can emit them. A label that differs from the stored value is a parallel `label=` list |

Values are case-sensitive. A CLI `regex` cannot contain a comma, because
modifiers are comma-separated. The model stores the same facts in a `validate`
tag separated by semicolons (`min=0;max=150`), which `gombit generate` and
admin meta read. `references=` stays unsupported.

```
age:int:required,min=0,max=150
status:enum(draft=Draft,published=Published):default=draft
length:duration:default=30m
opens:time_of_day
```

`enum(draft,published)` still stores each token as both the value and the
label. `enum(draft=Draft,published=Published)` stores `draft` and shows
`Draft`. The label cannot contain a comma. A stored value or a label
cannot contain `{` or `}`, because the generated form writes the label as
JSX text. The create body and the list
filter use the stored value. The generated form and the admin select show
the label. The model's `validate` tag keeps `enum=draft,published` and,
when a label differs, `label=Draft,Published`.
