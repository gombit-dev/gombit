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

Resourcegen and admin read this catalog. They do not keep their own copies of
the overlapping names (`int` / `integer`, `bool` / `boolean`, `time` /
`datetime`).
