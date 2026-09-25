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
| `float` | no | `float` | `float` | |
| `decimal` | yes | `decimal` | `decimal` | `types.Decimal` |
| `boolean` | yes | `bool`, `boolean` | `boolean` | `bool` |
| `date` | no | `date` | `date` | |
| `datetime` | yes | `time`, `datetime` | `datetime` | `time.Time` |
| `time` | no | | | clock time, not the `time` token |
| `duration` | no | `duration` | | |
| `uuid` | no | `uuid` | `uuid` | |
| `json` | no | `json` | `json` | |
| `email`, `url`, `slug`, `ip` | no | same as the kind | `string` | |
| `enum` | yes | `enum(a,b)` | `string` | `string` |
| `relation` | yes | `belongs_to`, `has_many`, `many_to_many` | `relation` | |

`time` on the command line is a **datetime** (`time.Time`). The clock-time
kind exists in the vocabulary so a later issue can add it without a second
type list. Admin introspection already stores `float`, `date`, `uuid`, and
`json`; the generator does not emit those kinds yet.

`integer64` and `unsigned` share the admin `integer` widget. The meta payload
does not change.

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
| `min=`, `max=` | `int`, `int64`, `uint` | GORM `check`, request `minimum` / `maximum`, form `min` / `max`, admin meta and the admin write |
| `min=`, `max=` | `decimal` | GORM `check`, and a create-body check that compares decimal magnitudes. The decimal schema is a string, so the request does not advertise `minimum` / `maximum` |
| `max_length=` | `string` | GORM `size` (otherwise 255), request `maxLength`, form `maxLength` |
| `regex=` | `string`, `text` | unanchored request `pattern` and the form's `new RegExp` check, including `text`. The pattern must compile in Go RE2 and in JavaScript and must not use a construct whose match set differs (`\p`, `\P`, inline flags, POSIX classes). The form does not set an HTML `pattern` attribute, because that attribute anchors the match. Not a SQL check |
| `default=` | scalars and `enum` | Applied when the create body or the admin create omits the field. An explicit `0`, `false`, or `""` is stored. A decimal default is a string, matching the decimal schema. The form and admin create start from that value. Enum values are stored on the model's `validate` tag so `gombit generate` can emit them |

Values are case-sensitive. A CLI `regex` cannot contain a comma, because
modifiers are comma-separated. The model stores the same facts in a `validate`
tag separated by semicolons (`min=0;max=150`), which `gombit generate` and
admin meta read. `references=` stays unsupported.

```
age:int:required,min=0,max=150
status:enum(draft,published):default=draft
```
