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

The layout mirrors what the generator emits:

```text
examples/tutorial/
├── main.go                    # ≈ cmd/server/main.go in a generated app
└── internal/task/
    ├── task.go                # GORM model
    ├── handler.go             # Huma-typed handlers, D10 envelope
    ├── routes.go              # explicit huma.Register calls
    └── admin.go               # admin.Register (ADR-013)
```

Differences from a generated app, all for self-containment:

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
