# Admin example (ADMIN-1 through ADMIN-3, relationships #223)

Minimal `framework.App` with `Config.Auth.Mode = config.AuthModeCookie`,
SQLite, and `admin.Register` of a `widget.Widget` model plus its relation
targets (`warehouse.Warehouse`, `part.Part`). `framework.New` mounts empty
admin Huma routes and the framework-owned SPA under `/admin/` in cookie mode;
this example registers three models from their feature packages. JWT-only apps
do not get these routes.

## Relationships (#223)

`Widget` carries all three relation kinds, and its `RegisterAdmin` leaves
`Fields` empty so `admin.FieldsFrom` derives them from the GORM model — no
per-field wiring:

- **belongs_to** — `WarehouseID` (a `*uint`, so genuinely optional) +
  `Warehouse` derive to a `warehouse_id` picker backed by the `warehouses` list,
  labelled by `name`. Leaving it unset stores `NULL`; the create still
  succeeds (a non-nullable `uint` FK would reject "no warehouse" under foreign
  key enforcement).
- **many_to_many** — `Warehouses []warehouse.Warehouse`
  (`many2many:widget_warehouses`) derives to a multi-select; a write syncs the
  join table in the same transaction as the row.
- **has_many** — `Parts []part.Part` derives to a **read-only** view of the
  children's ids (`Part` carries the `widget_id` back-reference). A write to it
  is rejected.

`Part.widget_id` is a plain integer, not a picker: `Part` cannot hold a
`Widget` association without an import cycle, which is the same constraint
`gombit make resource ... has_many:Part` documents.

See [`docs/admin.md`](../../docs/admin.md),
[`docs/cli.md`](../../docs/cli.md) (relation grammar), and
[ADR-013](../../docs/adr/013-runtime-generic-admin.md).

## Run

```sh
go run ./examples/admin
```

Admin UI: [http://127.0.0.1:8082/admin/](http://127.0.0.1:8082/admin/).
Sign in with the seeded superuser
`admin@example.com` / `correct-horse-battery-staple`. The widgets screen
appears because `widget.RegisterAdmin` called `admin.Register` — there is
no per-model React file.

The example also seeds `viewer@example.com` with the same password and adds
that user to a `viewers` group containing only `admin.widgets.view`. The
viewer can open the widget list and detail screens. The API returns 403 for
create, update, and delete, and the SPA hides those buttons. The superuser
sees every enabled action without needing permission rows.

Interactive docs: [http://127.0.0.1:8082/docs](http://127.0.0.1:8082/docs).

`/auth/register` never sets `IsSuperuser`. When the example is pointed at
a file-backed SQLite DSN, use `gombit createsuperuser` (see below).

## E2E (meta + one CRUD cycle)

A cookie jar is required. CSRF must be double-submitted on POST/PATCH/DELETE.

```sh
JAR=$(mktemp)

CSRF_BODY=$(curl -sS -c "$JAR" -b "$JAR" http://127.0.0.1:8082/api/v1/auth/csrf)
CSRF=$(python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["csrf_token"])' <<<"$CSRF_BODY")

curl -sS -c "$JAR" -b "$JAR" -X POST http://127.0.0.1:8082/api/v1/auth/login \
  -H 'Content-Type: application/json' -H "X-CSRF-Token: $CSRF" \
  -d '{"email":"admin@example.com","password":"correct-horse-battery-staple"}'

# catalog (data.models is required; empty array when nothing is registered)
curl -sS -c "$JAR" -b "$JAR" http://127.0.0.1:8082/api/v1/admin/meta
curl -sS -c "$JAR" -b "$JAR" http://127.0.0.1:8082/api/v1/admin/meta/widgets

# create / list / detail / update / delete
CREATED=$(curl -sS -c "$JAR" -b "$JAR" -X POST http://127.0.0.1:8082/api/v1/admin/resources/widgets \
  -H 'Content-Type: application/json' -H "X-CSRF-Token: $CSRF" \
  -d '{"name":"Wrench","sku":"w-1"}')
ID=$(python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["id"])' <<<"$CREATED")

curl -sS -c "$JAR" -b "$JAR" "http://127.0.0.1:8082/api/v1/admin/resources/widgets?search=Wrench&ordering=name"
curl -sS -c "$JAR" -b "$JAR" "http://127.0.0.1:8082/api/v1/admin/resources/widgets/$ID"
curl -sS -c "$JAR" -b "$JAR" -X PATCH "http://127.0.0.1:8082/api/v1/admin/resources/widgets/$ID" \
  -H 'Content-Type: application/json' -H "X-CSRF-Token: $CSRF" \
  -d '{"sku":"w-1a"}'
curl -sS -c "$JAR" -b "$JAR" -X DELETE "http://127.0.0.1:8082/api/v1/admin/resources/widgets/$ID" \
  -H "X-CSRF-Token: $CSRF"

rm -f "$JAR"
```

Anonymous calls return D10 `authentication` (401). An authenticated user
without the registered action permission gets D10 `authorization` (403).
Unknown slugs/ids are `not_found`. A disabled action is `authorization`
even for a superuser.

## Create a superuser against a file DSN

When the example is pointed at a file-backed SQLite DSN instead of the
in-memory default, use `gombit createsuperuser`:

```sh
GOMBIT_DATABASE_DRIVER=sqlite \
GOMBIT_DATABASE_DSN='file:admin-example.db?cache=shared&_fk=1' \
GOMBIT_JWT_SECRET='dev-only-example-jwt-secret-not-for-prod' \
GOMBIT_AUTH_MODE=cookie \
go run ./cmd/gombit createsuperuser --no-input \
  --email admin@example.com --password correct-horse-battery-staple
```
