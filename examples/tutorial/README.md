# Tutorial example

The finished application from [`docs/tutorial.md`](../../docs/tutorial.md): one
`Task` resource served through Huma, with cookie auth and the runtime admin.

```sh
go run ./examples/tutorial
```

| URL | What |
| --- | --- |
| <http://127.0.0.1:8083/api/v1/tasks> | the resource API |
| <http://127.0.0.1:8083/docs> | interactive OpenAPI docs |
| <http://127.0.0.1:8083/admin/> | the framework-owned admin SPA |

A superuser is seeded at startup: **admin@example.com** /
**correct-horse-battery-staple**.

## Why this lives here

The tutorial builds this app with `gombit new` + `gombit make resource`, which
produces a **separate Go module** — generated apps are never committed to this
repository. This copy is written against the framework module directly, so CI
compiles it on every push. If a framework change breaks what the tutorial
teaches, the build fails here first.

It is a **hand-written, self-contained** illustration — not generator output.
`make resource` is now model-first (the model is human-owned; the DTOs and CRUD
handler are generated `*.gen.go` and customization lives in `hooks.go` — see
[cli.md](../../docs/cli.md#gombit-make-resource) and the
[migration guide](../../docs/migration-model-first-resources.md)). This example
keeps a single hand-written handler for self-containment rather than committing
generated `*.gen.go`:

```text
examples/tutorial/
├── main.go                    # ≈ cmd/server/main.go in a generated app
└── internal/task/
    ├── task.go                # GORM model (the human-owned source of truth)
    ├── handler.go             # hand-written Huma handlers (a generated app has handler.gen.go)
    ├── routes.go              # explicit huma.Register calls (a generated app folds this into handler.gen.go)
    └── admin.go               # admin.Register (ADR-013)
```

Differences from a generated app, all for self-containment:

- a single hand-written `handler.go`/`routes.go` instead of the model-first
  `dto.gen.go` + `handler.gen.go` + `hooks.go`;

- an in-memory SQLite DSN instead of a file, and `AutoMigrate` in an `OnStart`
  hook instead of `gombit db migrate`;
- the superuser is seeded in process rather than by `gombit createsuperuser`;
- config is built with `config.Default()` in code instead of read from `.env`;
- no `frontend/` — the tutorial's React pages belong to the generated tree.

## Trying the API

Cookie auth means writes need a CSRF token:

```sh
curl -s -c jar.txt http://127.0.0.1:8083/api/v1/auth/csrf
CSRF=$(grep -i csrf jar.txt | awk '{print $7}')

curl -s -b jar.txt -c jar.txt -X POST http://127.0.0.1:8083/api/v1/auth/login \
  -H 'Content-Type: application/json' -H "X-CSRF-Token: $CSRF" \
  -d '{"email":"admin@example.com","password":"correct-horse-battery-staple"}'

CSRF=$(grep -i csrf jar.txt | awk '{print $7}')
curl -s -b jar.txt -X POST http://127.0.0.1:8083/api/v1/tasks \
  -H 'Content-Type: application/json' -H "X-CSRF-Token: $CSRF" \
  -d '{"title":"Write the tutorial","done":false}'
```

```json
{"data":{"id":1,"title":"Write the tutorial","done":false}}
```

Then read it back through the admin data plane:

```sh
curl -s -b jar.txt 'http://127.0.0.1:8083/api/v1/admin/resources/tasks?per_page=5'
```

See [`docs/auth-cookie.md`](../../docs/auth-cookie.md) and
[`docs/admin.md`](../../docs/admin.md).
