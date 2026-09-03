# Contract (Huma DTOs + envelope)

Gombit's public API contract conventions: Huma-typed handlers over Gin, D10
success/error envelopes, validation tags → structured `fields`, and draft §41
application error categories mapped centrally to HTTP status codes.

ADR-011 selected Huma as the contract layer. OpenAPI emission, `/docs`, and
`gombit openapi generate` are documented in [`docs/openapi.md`](openapi.md).
The TypeScript client is documented in [`docs/client.md`](client.md).

## Registering contract routes

`framework.New` mounts a Huma API on the same `*gin.Engine` as `app.Router()`.
Register public API operations on `app.API()`:

```go
app, err := framework.New()
if err != nil {
    return err
}

prefix := app.Config().API.Prefix // default /api/v1
huma.Register(app.API(), huma.Operation{
    OperationID: "create-widget",
    Method:      http.MethodPost,
    Path:        prefix + "/widgets",
}, handler)

return framework.Run(app)
```

Use `app.Router()` only for routes that must stay outside the OpenAPI contract
(webhooks, SSE, legacy). See [`docs/router.md`](router.md).

`examples/contract` shows create, list (with `meta`), get, and not-found.

## Success envelope

```go
type createWidgetOutput struct {
    Body contract.Data[Widget] // {"data": {...}}
}

type listWidgetsOutput struct {
    // Typed meta so OpenAPI emits PageMeta fields (not empty object).
    Body contract.DataMeta[[]Widget, contract.PageMeta]
}
```

`contract.PageMeta` is the v0.1 pagination meta for collection responses.
Pass a non-nil pointer when meta should appear; a nil `Meta` is omitted.
A non-nil zero `PageMeta` still serializes (JSON `omitempty` does not drop empty structs).

Generated list handlers (`gombit new` product, `gombit make resource`, and
`examples/tutorial`) honor `page` / `per_page` query parameters the same way
the admin data plane does: default page 1, per_page 20, max 100
(`contract.ClampPage`). `meta.total` is a separate `COUNT`; `data` is the
`LIMIT`/`OFFSET` slice, so `len(data)` never exceeds `meta.per_page`.

```go
Body: contract.DataMeta[[]Widget, contract.PageMeta]{
    Data: items,
    Meta: &contract.PageMeta{Page: page, PerPage: perPage, Total: total},
},
```

```json
{
  "data": [],
  "meta": {
    "page": 1,
    "per_page": 20,
    "total": 125
  }
}
```

### List query: filter / sort / search

Beyond pagination, `gombit make resource` opts declared fields into a list-query
surface (issue #260) that uses the **same query spelling as the admin data
plane** so the two contracts do not diverge. Declaration keeps the surface a
safe, indexable subset — a field is queryable only when it opts in, mirroring how
list columns are a declared subset. Add modifiers to the field grammar:

| Modifier     | Query parameter                          | Types                                |
| ------------ | ---------------------------------------- | ------------------------------------ |
| `filterable` | `?<field>=<value>` (exact match)         | string, int, int64, uint, bool, enum |
| `sortable`   | `?ordering=<field>` (`-<field>` for DESC) | any scalar (and belongs_to FK)       |
| `searchable` | `?search=<term>` (case-insensitive LIKE) | string, text, enum                   |

A `belongs_to` foreign key is **filterable by default** — no modifier needed —
so a detail page can list a record's `has_many` children with
`GET /api/v1/invoices?customer_id=<id>`.

```bash
gombit make resource Article \
  title:string:required,searchable,sortable \
  body:text:searchable \
  status:enum(draft,published):filterable,sortable \
  author:belongs_to:Author
```

The generated handler applies filters and `?search=` before the `COUNT`, so
`meta.total` reflects the filtered set; `?ordering=` replaces the fixed `id`
order (`id` stays the default when `?ordering=` is absent) and is validated
against the declared sortable set. An undeclared ordering field or an
uncoercible filter value returns a D10 `validation_error` (422). The shared
primitives live in `database` (`FilterEq`, `Search`, `Ordering` / `ParseOrdering`)
and are the same behavior — and now the same code — the admin data plane applies.
Filter values are exact-match to start; ranges/operators come later.

## Request DTOs

Go structs are the source of truth. Prefer Huma tags — not a separate
`validate:"..."` layer and not hand-written OpenAPI files.

```go
type CreateWidgetBody struct {
    Name  string `json:"name" minLength:"1" maxLength:"80" doc:"Human-readable name"`
    Color string `json:"color,omitempty" maxLength:"30"`
}
```

Conventions:

- Nest the JSON body under an input field named `Body` (Huma binding).
- Put validation metadata on body fields (`minLength`, `maxLength`, `format`,
  `enum`, `required`, and the other Huma tags).
- Document fields with `doc` / `example` so they appear in OpenAPI.

## Validation → D10 `fields`

Invalid requests return HTTP **422** with:

```json
{
  "error": {
    "code": "validation_error",
    "message": "The request contains invalid fields.",
    "fields": {
      "name": ["expected required property name to be present"]
    },
    "request_id": "..."
  }
}
```

`contract.Install` (called from `framework.New`) replaces Huma's default RFC
9457 Problem Details errors with this envelope. `fields` keys are derived from
Huma `ErrorDetail.Location` values:

| Huma location | D10 field key |
| --- | --- |
| `body.name` | `name` |
| `body.items[0].tags` | `items[0].tags` |
| `query.limit` | `query.limit` |
| `path.widget-id` | `path.widget-id` |

When Huma omits a location for a missing required property, Gombit infers the
field name from messages like `expected required property name to be present`.

`request_id` comes from the runtime request-ID middleware
(`X-Request-Id` / `framework.GetRequestIDFromContext`).

Content-Type stays `application/json` (not `application/problem+json`).

## Application errors (§41 categories)

Services and handlers should return category errors, not ad-hoc status codes.
Constructors build a D10 `ErrorEnvelope` (`huma.StatusError`). Wrap with
`contract.WithContext` so `request_id` is filled:

```go
return nil, contract.WithContext(ctx, contract.NotFound("widget not found"))
```

| Category | D10 `error.code` | HTTP |
| --- | --- | --- |
| `validation` | `validation_error` | 422 |
| `authentication` | `authentication` | 401 |
| `authorization` | `authorization` | 403 |
| `not_found` | `not_found` | 404 |
| `conflict` | `conflict` | 409 |
| `rate_limited` | `rate_limited` | 429 |
| `dependency_unavailable` | `dependency_unavailable` | 503 |
| `internal` | `internal` | 500 |

Helpers: `Validation`, `Authentication`, `Authorization`, `NotFound`,
`Conflict`, `RateLimited`, `DependencyUnavailable`, `Internal`, plus
`New(category, message)`. Always wrap with `WithContext` in handlers so
`request_id` is set — constructors alone leave it empty.

GORM errors from generated list/get/create handlers (and the admin data
plane) go through `database.MapLoadError` / `database.MapPersistError`:

| Driver outcome | D10 `error.code` | HTTP |
| --- | --- | --- |
| Missing row (`gorm.ErrRecordNotFound`) | `not_found` | 404 |
| Unique / duplicate key | `conflict` | 409 |
| Any other database error | `internal` | 500 |

`database.IsUniqueViolation` is the portable detector (`gorm.ErrDuplicatedKey`
or a driver string containing `unique` / `duplicate` — `database.Open` does
not enable GORM `TranslateError`). Auth registration uses the same helper
and still returns `conflict` for a taken email. Do not map every `First()`
error to 404 or every `Create()` error to 500.

Gin middleware may also emit D10 errors that are not §41 categories.
`contract.PayloadTooLarge` (`error.code` `payload_too_large`, HTTP 413) is
used by XSS JSON sanitizer buffering (see [`docs/router.md`](router.md));
cookie CSRF uses `Authorization` (403). An unsupported method on a known
route yields `contract.MethodNotAllowed` (`error.code` `method_not_allowed`,
HTTP 405) with an `Allow` header listing the methods the path supports — the
router distinguishes this from a genuinely unknown path (404). Do not treat
413 or 405 as a handler-level §41 category or add them to the table above.

Response bodies carry only the D10 shape (`{data, meta?}` / `{error}`). Huma's
`$schema` link property is disabled at the adapter, so neither responses nor the
generated TS request types include a `$schema` key.

`WithFields` attaches D10 `fields` without forcing the code to
`validation_error` (for example a `conflict` with a field detail). Huma tag
validation still uses the Install path and always yields `validation_error`.
A missing JSON body is HTTP **422** `validation_error` (Huma may start as
400; Install remaps it). An unexpected non-`StatusError` from a handler is
HTTP **500** `internal` with no `fields` — not `validation_error` and not
the raw driver string.

The `validation_error` code is preserved from M3-1 / D10 (not renamed to
`validation`).

## OpenAPI

`/openapi.json` is served from the Huma API and reflects the D10
`ErrorEnvelope` schema. `/docs` is the FastAPI-style Swagger UI (on by default
in local/dev). `gombit openapi generate` writes the live spec to disk. CI
regenerates the sample spec and TypeScript client from `client.SampleApp()`
and fails on drift — see [`docs/openapi.md`](openapi.md) and
[`docs/client.md`](client.md).

## What is not here yet

- Pagination query DSL / filter/sort helpers (design §42)
- gRPC status mapping (post-v0.1)
