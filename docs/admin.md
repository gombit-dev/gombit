# Admin (ADMIN-1 through ADMIN-3)

Gombit's Django-style admin is a **runtime generic admin** over an explicit
model registry, a Huma introspection + data-plane API, and a
framework-owned React SPA under `/admin/`
([ADR-013](adr/013-runtime-generic-admin.md)). Feature packages call
`admin.Register` only — there is **zero** per-model frontend code.

| Issue | What it ships |
| --- | --- |
| ADMIN-0 (#33) | ADR-013. Done. |
| ADMIN-1 (#34) | `admin.Register` + `GET /api/v1/admin/meta` + generic `/api/v1/admin/resources/{slug}` |
| ADMIN-2 (#35) | Framework-owned SPA under `/admin/` |
| ADMIN-3 (#36) | Session-gated direct/group permissions with a superuser bypass |

## When routes mount

`framework.New` mounts admin Huma routes **and** the `/admin/` SPA **only**
when cookie session auth is on (`cfg.Auth.Mode == cookie` /
`AuthModeCookie`) and a database is attached. JWT-only apps do not get
`/api/v1/admin/…` or `/admin/` (they 404; the SPA paths are absent from
OpenAPI). Dual Bearer-API + cookie-admin in one process is not introduced
here.

Session is required for the API. Regular users receive permission keys
directly or through `auth.Group`; `auth.User.IsSuperuser` bypasses every
permission check (`gombit createsuperuser`). `/auth/register` never sets
that flag. The SPA itself is public HTML (anonymous `GET /admin/` returns
the shell and redirects to `/admin/login`).

| Caller | Admin API result |
| --- | --- |
| Anonymous | D10 `authentication` (401) |
| Authenticated without a required permission | D10 `authorization` (403) |
| Superuser, enabled action | Allowed without a permission row |
| Any authenticated user, disabled action | D10 `authorization` (403) |
| Any authenticated user, unknown slug or id | D10 `not_found` (404) |

`GET /admin/meta` returns only models for which the user has the registered
view key. A regular user with no visible model receives 403, preventing
catalog probing. Each model includes current-user `can.view/create/update/delete`;
each value requires both the permission grant and an enabled action.

Seed a group after the application has migrated the auth models:

```go
permission, _ := auth.EnsurePermission(ctx, db, "admin.widgets.view", "View widgets")
viewers, _ := auth.EnsureGroup(ctx, db, "viewers")
_ = auth.GrantPermissionToGroup(ctx, db, &viewers, &permission)
_ = auth.AddUserToGroup(ctx, db, &user, &viewers)
```

Generated applications include `auth.User`, `auth.RefreshToken`,
`auth.Group`, and `auth.Permission` in their complete `AutoMigrate`/Atlas
desired-schema list. Production `gombit createsuperuser` still does not run
`AutoMigrate`; migrate the application before invoking it.

CSRF on POST/PATCH/DELETE is the existing M5-3 global middleware
(`X-CSRF-Token`). See [auth-cookie.md](auth-cookie.md).

## Admin SPA (`/admin/`)

The UI lives in `internal/adminui` and is versioned with the Gombit module.
`framework.New` embeds the production `dist/` (`//go:embed all:dist`) and
registers explicit Gin GET/HEAD routes for `/admin` and `/admin/*filepath`
so they win over Huma and over an application `WithEmbeddedFrontend`
NoRoute. Those routes stay **out of OpenAPI**. If `dist/` has no
`index.html`, the SPA is not mounted (same placeholder rule as M5-5).

Open http://127.0.0.1:8082/admin/ after `go run ./examples/admin` (cookie
mode + seeded superuser). Screens are driven only by:

- `GET /api/v1/admin/meta` and `GET /api/v1/admin/meta/{slug}`
- `GET/POST /api/v1/admin/resources/{slug}` and
  `GET/PATCH/DELETE /api/v1/admin/resources/{slug}/{id}`
- cookie auth: `GET /api/v1/auth/csrf`, `POST /api/v1/auth/login`,
  `POST /api/v1/auth/logout`, `GET /api/v1/me`

Anonymous `/admin` redirects to `/admin/login`. After login the catalog
lists authorized `data.models` (a superuser can receive an empty catalog
when nothing is registered; models with `actions.list === false` are shown
but not linked). List/detail/create/edit/delete honor both `actions.*` and
the current user's `can.*`. Switching models remounts the list route
(`key={slug}`) so page, search, ordering, and filters start empty.
Create and edit remount on slug/id (`key={`${slug}-${id || "new"}`}`) so
overlapping field names do not leak onto the next model or row. Edit
requires a GET of the current row (`actions.detail` + `can.view`); if
detail is disabled, the edit screen is hidden rather than PATCHing empty
boolean defaults (`false`) over stored `true` values.
Field widgets cover the closed field types; `belongs_to` is a single-select
picker (storing the foreign key); `has_many` is a read-only list of the related
children; `many_to_many` is a
multi-select. The relation pickers are searchable Autocompletes backed by the
related model's list endpoint, showing its label field. When the related model
supports search, typing issues a debounced server-side `search` (surfacing rows
beyond the first page) and the local filter is off; otherwise it filters the
loaded page client-side. A model registered without a `Search` defaults it to
its text columns (an explicit empty `Search` opts out), so pickers search out of
the box.
`datetime-local` values are converted to RFC3339 before POST/PATCH.
Empty optional (non-boolean) inputs — string, text, date, datetime, json,
number, relation — are sent as JSON `null` so a partial PATCH can clear
them. Booleans and numeric `0` are always included.
401 (including session expiry on list/detail/edit) returns to login;
catalog 403 shows a forbidden page; other catalog errors show the D10
message. Silent `POST /auth/refresh` after a 401 **awaits** CSRF bootstrap
so a reload with an expired access cookie does not send refresh without
`X-CSRF-Token` and drop a still-valid refresh cookie.

The SPA does **not** bake `/api/v1` at Vite build time. Gin injects
`config.API.Prefix` when serving `index.html` (placeholder
`__GOMBIT_API_PREFIX__`) and `GET /admin/config.json`. The client
prefixes `/auth/*` and `/admin/*` with that value (default `/api/v1`).
Resource IDs (and slugs) are `encodeURIComponent`'d as a single path
segment so a string PK such as `foo/bar` or `../widgets/1` is not split
or normalized by `new URL()`.

The SPA may use MUI internally. That is **not** a C4 violation: `--ui mui`
remains the generated **application** preset. `gombit new` trees do not
gain `@mui` from this package. `gombit make resource` does not grow
`--admin`. Cookie-mode `gombit dev` prints the Admin SPA URL and Vite
proxies `/admin` to the Go origin so `http://127.0.0.1:5173/admin/` reaches
the framework admin instead of the generated application SPA.

Rebuild: see [`internal/adminui/README.md`](../internal/adminui/README.md).
Commit `dist/` with source changes so consumers `go get` a working embed.

## Registration

Feature packages register models explicitly — typically from
`internal/<feature>/admin.go` or `routes.go`. The framework never walks
GORM models, AutoMigrate lists, or packages.

```go
func RegisterAdmin(app *framework.App) error {
    return admin.Register(app, Product{}, admin.Options{
        Slug:     "products",
        Singular: "Product",
        Plural:   "Products",
        Fields: []admin.Field{
            {Name: "id", Type: admin.TypeInteger, ReadOnly: true},
            {Name: "name", Type: admin.TypeString, Required: true},
            {
                Name: "category_id",
                Type: admin.TypeRelation,
                Related: &admin.Relation{
                    Slug:       "categories",
                    Kind:       admin.RelBelongsTo,
                    LabelField: "name",
                },
            },
        },
        List:     []string{"name", "category_id"},
        Search:   []string{"name"},
        Filter:   []string{"category_id"},
        Ordering: []string{"name", "created_at"},
        Actions: admin.Actions{
            List: true, Detail: true, Create: true, Update: true, Delete: true,
        },
        Permissions: admin.Permissions{
            View:   "admin.products.view",
            Create: "admin.products.create",
            Update: "admin.products.update",
            Delete: "admin.products.delete",
        },
    })
}
```

`Options` is the source of truth. Missing or duplicate `Slug` is an error at
`Register`. After `Register` returns, handlers read stored values plus a
constructor for `T` (`Register[T any]`). They do **not** reflect over
arbitrary Go types.

### Fields, PK, and names

- **`Field.Name`** is the JSON object key in meta and in data-plane rows.
  For v1, `Name` is also the GORM/SQL column unless `Field.Column` is set
  (use that when the Go exported name or GORM column differs).
- **`Options.PK`** is the JSON/field name of the primary key. Empty means
  derive the GORM primary key **at Register** and store it. The PK field
  must appear in `Fields`.
- **Empty `Fields`** derives a default from the struct once, inside
  `Register`, via `admin.FieldsFrom(T)`. That helper may use `reflect`
  **only at registration time**. Do not call it from request handlers.
- **`created_at` / `updated_at`** are implicit GORM timestamp columns.
  They may appear in `List` and `Ordering` even when omitted from
  `Fields`. When the model has those columns, list and detail row JSON
  include the values. Search and filter still require an explicit field.
  If the model has no such GORM column, `Register` errors.
- Zero `Actions` defaults to all enabled. Empty `Permissions` default to
  `admin.{slug}.{view,create,update,delete}`. Admin handlers enforce the
  stored keys, including custom keys, and echo them in meta.

Closed field types: `string`, `text`, `integer`, `float`, `decimal`,
`boolean`, `datetime`, `date`, `uuid`, `json`, `relation`.

Relation `kind` is `belongs_to`, `has_many`, or `many_to_many`. **`belongs_to`**
is stored as the foreign key on create/update; auto-derivation (`FieldsFrom`)
renders the FK column as a relation field (target `slug` = the related table,
label = the field name of its `name` column when present) so the SPA shows a
picker instead of a
bare integer, and the picker submits the selected primary key. **`has_many` is
read-only**: auto-derivation emits it, and when it maps to a real GORM has_many
association the list/detail responses preload it and return the related
children's primary keys (the SPA shows them as read-only chips); a `has_many`
field declared without a matching association is meta-only and reads empty.
Writes to a `has_many` field are rejected, and `Register` rejects `has_many`
(and any field with no SQL column) in Search, Filter, or Ordering.

**`many_to_many`** round-trips as a list of related primary keys (#223): the
list/detail responses read the related ids (the association is preloaded), and
create/update sync the join table to the submitted id list. An empty list
clears the relation; omitting the field on a PATCH leaves it unchanged. Every
submitted id must reference an existing related row — a missing id is a 422, so
you cannot point the join table at a row that does not exist. Auto-derivation
(`FieldsFrom`) emits a `many_to_many` relation field for each association (with
the related table as the default target `slug`) instead of dropping it; register
an explicit `admin.Field` to set the `slug`/`label_field` when they differ. The
framework admin SPA renders it as a multi-select backed by the related model's
list endpoint.

`Register` does not AutoMigrate. The application still owns schema
(Atlas / `AutoMigrate` in examples).

Generated apps are **not** auto-registered. Prefer an explicit
`product.RegisterAdmin` in the app; this repo does not scaffold that call
so JWT goldens stay still.

## HTTP (Huma on `app.API()`, D10)

Paths honor `config.API.Prefix` (default `/api/v1`) and appear in OpenAPI.

| Method | Path | Body |
| --- | --- | --- |
| `GET` | `/api/v1/admin/meta` | `{ "data": { "models": [ ... ] }, "meta"?: { "auth": { "mode": "cookie", "bootstrap": "permissions" } } }` |
| `GET` | `/api/v1/admin/meta/{slug}` | `{ "data": { /* one model */ } }` — 404 `not_found` if unknown |
| `GET` | `/api/v1/admin/resources/{slug}` | list; `meta` is `contract.PageMeta` (`page`, `per_page`, `total`) |
| `POST` | `/api/v1/admin/resources/{slug}` | create writable fields |
| `GET` | `/api/v1/admin/resources/{slug}/{id}` | detail |
| `PATCH` | `/api/v1/admin/resources/{slug}/{id}` | update writable fields |
| `DELETE` | `/api/v1/admin/resources/{slug}/{id}` | `{ "data": { "ok": true } }` |

`data.models` is required on the catalog (empty array when nothing is
registered). Raw `*gin.Engine` is **not** used for these endpoints. It
**is** used for `/admin/` static files and SPA fallback, which must not
appear in OpenAPI.

List query parameters:

- `page`, `per_page` (default page 1, per_page 20, max 100; same
  `contract.ClampPage` as generated list handlers)
- `search` (OR `LIKE` across `Options.Search`, ASCII case-insensitive on
  every supported driver — SQLite's built-in `LOWER()` folds ASCII only, so
  full Unicode case-folding parity with Django's `icontains` is not
  guaranteed for accented/non-Latin search terms)
- `ordering` (a field from `Options.Ordering`; prefix `-` for DESC)
- one query key per `Options.Filter` field

Row JSON is keyed by registered field names, plus implicit `created_at` /
`updated_at` when the GORM model has them and they were omitted from
`Fields`. Create/update accept only writable (non-readonly) fields.
`PATCH` is partial: omitted keys are left unchanged. A present JSON `null`
(or `""` for string/text) clears an optional field — pointer dests become
SQL NULL, non-pointer strings become `""`, `time.Time` becomes zero,
`json.RawMessage` / `[]byte` become nil. Required fields with `null` still
422 (`is required`). Unknown keys, readonly keys, and type failures render
D10 `validation_error` with `fields`. JSON object/array values populate
`json.RawMessage`, `[]byte`, and nested structs (`json.Unmarshal` /
`json.Unmarshaler`). UUID strings populate `uuid.UUID`
(`encoding.TextUnmarshaler` / `sql.Scanner`). `FieldsFrom` infers
`uuid.UUID` as `uuid`, not `json`.

The application's public CRUD API stays the feature's own typed Huma
routes. Admin does not replace them.

## Example

[`examples/admin`](../examples/admin) — cookie mode + SQLite +
`admin.Register` of `widget.Widget`, plus the `/admin/` SPA. It seeds a
superuser with `auth.Service.CreateSuperuser` (the same path as
`gombit createsuperuser`) and a `viewers` group with only
`admin.widgets.view`. Curl for meta and one CRUD cycle is in that README.

## Out of scope

- Full users/groups management screens in the admin SPA
- `--admin` generator / golden template changes / copying the SPA into
  generated `frontend/`
- M6 batteries (jobs, events, scheduler, mail, storage, gRPC, multi-tenancy, i18n)
- `localStorage` tokens
