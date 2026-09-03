# Tutorial: build a task app with Gombit

You'll build a small task tracker end to end: a typed API, a migration, a
generated TypeScript client, a React page, cookie login, and the same model
running in the admin. That's the whole v0.1 loop.

**Time:** about 45 minutes.
**Prerequisites:** [installation.md](installation.md) — Go 1.25+, Node 22+,
Atlas, and a C toolchain (SQLite is cgo-only).

Every chapter ends with a **✅ Checkpoint** telling you what "it worked" looks
like. If a checkpoint fails, stop there — later chapters build on it.

The finished application is committed at
[`examples/tutorial/`](../examples/tutorial) and compiled by CI, so it can't
silently rot.

| # | Chapter |
| --- | --- |
| 1 | [Scaffold the project](#1-scaffold-the-project) |
| 2 | [Run it](#2-run-it) |
| 3 | [Your first model and migration](#3-your-first-model-and-migration) |
| 4 | [Generate the resource](#4-generate-the-resource) |
| 5 | [The contract](#5-the-contract) |
| 6 | [The typed client](#6-the-typed-client) |
| 7 | [The frontend](#7-the-frontend) |
| 8 | [Auth](#8-auth) |
| 9 | [The admin](#9-the-admin) |
| 10 | [A management command](#10-a-management-command) |
| 11 | [Ship it](#11-ship-it) |
| 12 | [Where next](#12-where-next) |

---

## 1. Scaffold the project

```bash
gombit new tasks --database sqlite --auth cookie --ui mui
cd tasks
```

Four choices, recorded in `gombit.yaml`:

| Flag | Value | Why |
| --- | --- | --- |
| `--database` | `sqlite` | zero setup; swap to `postgres` or `mysql` later via config |
| `--auth` | `cookie` | **the admin requires session cookies** (ADR-013). The API default is Bearer JWT (`--auth jwt`) |
| `--ui` | `mui` | scaffolds Material UI CRUD screens. `minimal` is the headless default |
| `--module` | derived | defaults to `github.com/example/tasks`; pass `--module` to set your own |

The generated tree:

```text
tasks/
├── cmd/
│   ├── server/main.go        # your application entrypoint
│   └── gombit/main.go        # your CLI, with app-registered commands
├── config/README.md
├── database/
│   ├── migrations/           # versioned SQL, written by gombit db makemigrations
│   └── seeds/
├── frontend/                 # Vite + React + TypeScript
│   └── src/api/generated/    # generated client — never hand-edit
├── internal/
│   ├── platform/database.go  # AutoMigrate model list
│   ├── product/              # example feature package
│   └── web/embed.go          # go:embed target for --embed builds
├── go.mod
└── gombit.yaml
```

The law here: **features live in `internal/<feature>/`** with their model,
handler, and routes together. No `app/controllers`, no `app/models`. `product/`
is a worked example — delete it once you've read it.

> `gombit new` writes a **separate Go module**. It is not part of the framework
> repository, and nothing is committed to Gombit when you run it.

**✅ Checkpoint**

```bash
go build ./...
```

Compiles with no output. `gombit new` pinned `go.mod` to its own framework
version and ran `go mod tidy`, so there is nothing to wire up by hand.

> Building the CLI from source instead of installing it? Then it reports `dev`,
> which the module proxy can't resolve, and `gombit new` tells you to add a
> `replace` — see
> [installation.md](installation.md#how-generated-apps-find-the-framework).

---

## 2. Run it

```bash
gombit dev
```

`gombit dev` runs the Go API and the Vite dev server together, proxies `/api`,
`/openapi.json`, `/docs`, and `/admin` to the backend, and regenerates the TypeScript
client whenever the live spec changes. It prints a service table with the URLs.

Open:

- <http://127.0.0.1:5173> — the React app
- <http://127.0.0.1:8080/docs> — interactive API docs
- <http://127.0.0.1:8080/openapi.json> — the OpenAPI 3.1 document

`/docs` is on when `GOMBIT_DOCS_ENABLED` is set, which is the local default.
Turn it off in production.

**✅ Checkpoint**

```bash
curl -s http://127.0.0.1:8080/livez
```

Returns a success response. Leave `gombit dev` running in its own terminal for
the rest of the tutorial.

---

## 3. Your first model and migration

Create `internal/task/task.go`:

```go
package task

import "gorm.io/gorm"

// Task is the feature-package GORM model.
type Task struct {
	gorm.Model
	Title string `gorm:"size:255;not null"`
	Done  bool
}
```

`gorm.Model` supplies `ID`, `CreatedAt`, `UpdatedAt`, and a soft-delete
`DeletedAt`.

Now generate the SQL. `makemigrations` takes the model type explicitly — it
doesn't scan your project:

```bash
gombit db makemigrations create_tasks --model github.com/example/tasks/internal/task.Task
```

Gombit wraps [Atlas](https://atlasgo.io/) in Program Mode: it loads your GORM
models, diffs them against the current schema, and writes **versioned SQL** you
can read and edit. There is no hand-rolled migration DSL, and no
`AutoMigrate`-in-production.

Read the file it wrote under `database/migrations/` before applying it. Then:

```bash
gombit db migrate
gombit db status
```

`status` lists applied and pending revisions. `gombit db rollback` undoes the
last batch.

`gombit new` already seeded *and applied* one migration ahead of yours: a
`bootstrap` migration for the framework's own auth tables (see
[migrations.md](migrations.md#the-bootstrap-migration)) — check
`gombit db status` right now and you'll see it there already, in batch 1,
before you've touched `db migrate` yourself. Your `create_tasks` migration
lands in its own batch.

**✅ Checkpoint** — `gombit db status` shows `bootstrap` (batch 1) and
`create_tasks` (batch 2) as applied.

> If you get `atlas: executable file not found in $PATH`, install Atlas
> Community Edition (see [installation.md](installation.md)). The GORM model is
> still loader-ready without it.

---

## 4. Generate the resource

Now let the generator build the API around that model:

```bash
gombit make resource Task title:string:required done:bool
```

```text
gombit make resource: resourcegen: refuse to overwrite internal/task/task.go
without --force (not generated by gombit)
```

**That refusal is the feature.** You hand-wrote `task.go` in chapter 3, and the
generator will not silently overwrite a file it didn't create. Since the
generated model is identical to what you wrote, let it through:

```bash
gombit make resource Task title:string:required done:bool --force
```

```text
modify cmd/server/main.go
modify frontend/src/resources.tsx
create frontend/src/task/form.tsx
create frontend/src/task/list.tsx
modify internal/platform/database.go
create internal/task/handler.go
create internal/task/routes.go
modify internal/task/task.go
```

The field grammar is
`name:type[:required][,unique][,index][,filterable][,sortable][,searchable]`,
over `string`, `text`, `int`, `int64`, `bool`, and `uint`. The
`filterable` / `sortable` / `searchable` modifiers opt a field into the list
handler's declared query surface — see the
[list query](contract.md#list-query-filter--sort--search) section.

Two properties of every Gombit generator matter here:

- **Go source is modified through `go/ast`, never regex.** `Register(app)` is
  inserted into `cmd/server/main.go` next to the existing `product.Register`
  call, and the AutoMigrate list is updated the same way.
- **They are idempotent and additive.** You just saw the clobber protection;
  re-running is safe, and `--dry-run` previews any run without writing.

`--service` and `--repo` add pass-through service/repository layers. The
default is a thin handler straight over GORM — add the layers when you have a
reason, not by default.

`routes.go` shows what was mounted:

```go
huma.Register(api, huma.Operation{
	OperationID: "list-tasks",
	Method:      http.MethodGet,
	Path:        prefix + "/tasks",
	Summary:     "List tasks",
	Tags:        []string{"Tasks"},
}, h.list)
```

Routes are registered **explicitly**. Gombit never discovers feature packages
by reflection.

**✅ Checkpoint**

```bash
curl -s http://127.0.0.1:8080/api/v1/tasks
```

```json
{"data":[],"meta":{"page":1,"per_page":20,"total":0}}
```

That shape is the D10 envelope: success is `{"data": ..., "meta"?: ...}` and
nothing else — the response body carries no extra keys.

---

## 5. The contract

Handlers are Huma-typed, and **the handler signature is the source of truth for
the API contract**. OpenAPI is emitted from the code, never hand-written.

From the generated `handler.go`:

```go
type createTaskInput struct {
	Body struct {
		Title string `json:"title" minLength:"1" maxLength:"255" doc:"Title"`
		Done  bool   `json:"done" doc:"Done"`
	}
}

type createTaskOutput struct {
	Body contract.Data[taskData]
}
```

`minLength` / `maxLength` land in the OpenAPI schema **and** are enforced at
request time. Validation failures come back in the D10 error envelope with
per-field detail:

```json
{
  "error": {
    "code": "validation_error",
    "message": "The request contains invalid fields.",
    "fields": {
      "done": ["expected required property done to be present"],
      "title": ["expected length >= 1"]
    },
    "request_id": "5db935cd-7c74-4ffe-a4de-0fa817451f54"
  }
}
```

`request_id` is on every error, which is what makes a production report
traceable to a log line.

Errors are constructed with `contract` helpers rather than raw status codes.
Generated get/create handlers classify GORM errors through
`database.MapLoadError` / `database.MapPersistError`: a missing row is D10
`not_found`, a unique violation is `conflict`, and any other driver failure
is `internal`.

```go
return nil, contract.WithContext(ctx, contract.NotFound("task not found"))
```

Inspect what you've mounted. Bare `gombit routes` only prints framework
routes (`/livez`, `/docs`, and so on) — pass `--url` to see the feature
routes `gombit dev` is actually serving:

```bash
gombit routes --url http://127.0.0.1:8080
gombit openapi generate --out openapi.json
```

Need something Huma can't express? `app.Router()` returns the raw
`*gin.Engine`. It's a tested, first-class escape hatch — not a workaround.

**✅ Checkpoint** — `/tasks` appears in the `/docs` UI with `title` marked
required.

---

## 6. The typed client

```bash
gombit client generate
```

This reads the OpenAPI document and writes a TypeScript client into
`frontend/src/api/generated/`. **Never hand-edit that directory** — it is
overwritten.

The important command is the drift check:

```bash
gombit client check --url http://127.0.0.1:8080/openapi.json
```

It fails if the committed client no longer matches the live contract. `gombit`
is a separately compiled binary, so outside the framework's own repository it
has no Go-level access to your API — `--url` fetches the live `/openapi.json`
from a running app (`gombit dev` or your deployed API) instead. Wire it into
your own CI against a running instance of your app: it's what stops a backend
change from silently breaking the frontend. `gombit dev` regenerates the
client automatically while it's running.

**✅ Checkpoint** — `frontend/src/api/generated/schema.ts` contains a task
type with `title` and `done`.

---

## 7. The frontend

`gombit make resource` already scaffolded `frontend/src/task/list.tsx` and
`form.tsx`, wired into `frontend/src/resources.tsx`. With `--ui mui` they're
Material UI screens; with `minimal`, headless equivalents.

Two things worth reading in that generated code:

**Types come from the generated client**, so a backend rename becomes a
TypeScript error rather than a runtime surprise.

**Server validation maps into the form.** The `fields` object from the D10
error envelope is fed into React Hook Form, so field errors from Go render next
to the right inputs — one validation source of truth, not two. Required
fields also get a client-side rule with its own message (`"Title is
required"`), so an empty submit shows text immediately, before round-tripping
to the server at all.

> **No secrets in frontend source.** Anything under `VITE_*` is compiled into
> the bundle and is public. Treat it that way.

You scaffolded with `--auth cookie`, so `RequireAuth` guards every route
except `/login` — visiting `/tasks` right now redirects there. The next
chapter sets up a login; come back and finish this checkpoint after it.

**✅ Checkpoint** — once you can log in (chapter 8), the task list renders at
<http://127.0.0.1:5173>, and submitting an empty title shows a "Title is
required" inline error next to the field.

---

## 8. Auth

You scaffolded with `--auth cookie`, so sessions are HttpOnly cookies plus CSRF
double-submit. (The API default, `--auth jwt`, keeps the access token **in
memory — never `localStorage`**.)

`gombit new` already wrote a random `GOMBIT_JWT_SECRET` into `.env` (cookie
mode signs its CSRF token with the same secret), and every `gombit` command
run from this directory — including the `gombit dev` you started in chapter
2 — loads `.env` automatically. Create an admin account:

```bash
gombit createsuperuser --email admin@example.com
```

The secret is required: without it Bearer auth stays unmounted and nobody
could log in. `createsuperuser` prompts for the password when you leave
`--password` off — better than putting it in your shell history.

> `.env` is gitignored and never read outside this directory. A real
> deployment has no `.env` file and sets `GOMBIT_JWT_SECRET` through its own
> environment instead — see [Ship it](#11-ship-it).

Under cookie auth, **every state-changing request needs a CSRF token**. The
full flow:

```bash
# 1. Get a CSRF token (and its cookie).
curl -s -c jar.txt http://127.0.0.1:8080/api/v1/auth/csrf
CSRF=$(grep -i csrf jar.txt | awk '{print $7}')

# 2. Log in.
curl -s -b jar.txt -c jar.txt -X POST http://127.0.0.1:8080/api/v1/auth/login \
  -H 'Content-Type: application/json' -H "X-CSRF-Token: $CSRF" \
  -d '{"email":"admin@example.com","password":"correct-horse-battery-staple"}'

# 3. Now writes are accepted.
CSRF=$(grep -i csrf jar.txt | awk '{print $7}')
curl -s -b jar.txt -X POST http://127.0.0.1:8080/api/v1/tasks \
  -H 'Content-Type: application/json' -H "X-CSRF-Token: $CSRF" \
  -d '{"title":"Write the tutorial","done":false}'
```

```json
{"data":{"id":1,"title":"Write the tutorial","done":false}}
```

Skip the token and you get a clean refusal rather than a mutation:

```json
{"error":{"code":"authorization","message":"csrf token missing or invalid","request_id":"..."}}
```

The browser app handles all of this for you — `RequireAuth` guards routes and
the client attaches the header. The threat model is written up in
[auth-cookie.md](auth-cookie.md).

**✅ Checkpoint** — you can log in from the UI, and a `POST` without the CSRF
header is rejected.

---

## 9. The admin

This is the part no other Go framework gives you: a **runtime** admin over an
explicit registry. Not generated pages you maintain, and not request-time
reflection over your models.

Create `internal/task/admin.go`:

```go
package task

import (
	"github.com/gombit-dev/gombit/admin"
	"github.com/gombit-dev/gombit/framework"
)

func RegisterAdmin(app *framework.App) error {
	return admin.Register(app, Task{}, admin.Options{
		Slug:     "tasks",
		Singular: "Task",
		Plural:   "Tasks",
		Fields: []admin.Field{
			{Name: "id", Type: admin.TypeInteger, ReadOnly: true},
			{Name: "title", Type: admin.TypeString, Required: true},
			{Name: "done", Type: admin.TypeBoolean},
		},
		List:     []string{"title", "done"},
		Search:   []string{"title"},
		Ordering: []string{"title", "created_at"},
		Actions: admin.Actions{
			List: true, Detail: true, Create: true, Update: true, Delete: true,
		},
	})
}
```

Call it from `cmd/server/main.go`, next to `task.Register(app)`:

```go
if err := task.RegisterAdmin(app); err != nil {
	log.Fatal(err)
}
```

Registration is explicit and typed — `Options` is concrete data, resolved once
at startup. Leave `Fields` empty and they're derived from the struct at
registration time, still once, never per request.

Now visit **<http://127.0.0.1:5173/admin/>** (Vite proxies `/admin` to the Go
server) or **<http://127.0.0.1:8080/admin/>** and log in as your superuser. The
SPA is framework-owned and served from the binary; you don't build or maintain
it.

Behind it are two API surfaces:

```bash
# What models exist, and what can be done with them.
curl -s -b jar.txt http://127.0.0.1:8080/api/v1/admin/meta
```

```json
{"data":{"models":[{"slug":"tasks","singular":"Task","plural":"Tasks","pk":"id",
"fields":[{"name":"id","type":"integer","required":false,"readonly":true},
{"name":"title","type":"string","required":true,"readonly":false},
{"name":"done","type":"boolean","required":false,"readonly":false}],
"list":["title","done"],"search":["title"],
"permissions":{"view":"admin.tasks.view","create":"admin.tasks.create","...":"..."}}]}}
```

```bash
# The generic data plane.
curl -s -b jar.txt 'http://127.0.0.1:8080/api/v1/admin/resources/tasks?per_page=5'
```

```json
{"data":[{"id":1,"title":"Write the tutorial","done":false,
"created_at":"2026-08-18T16:40:26Z","updated_at":"2026-08-18T16:40:26Z"}],
"meta":{"page":1,"per_page":5,"total":1}}
```

### Permissions

Each resource gets permission keys defaulting to `admin.{slug}.{action}` —
`admin.tasks.view`, `admin.tasks.create`, and so on. They're enforced on every
admin request, granted **directly or through groups**, and **superusers bypass
all of them**.

To give someone read-only access, grant just `admin.tasks.view` — via a group
if more than one person needs it:

```go
permission, _ := auth.EnsurePermission(ctx, db, "admin.tasks.view", "View tasks")
group, _ := auth.EnsureGroup(ctx, db, "viewers")
_ = auth.GrantPermissionToGroup(ctx, db, &group, &permission)
_ = auth.AddUserToGroup(ctx, db, &user, &group)
```

Unauthenticated requests to the admin API get `401`; authenticated-but-
unpermitted get `403`.

**✅ Checkpoint** — your task is listed at `/admin/`, and a non-superuser
without `admin.tasks.view` gets `403`.

Details: [admin.md](admin.md) and [ADR-013](adr/013-runtime-generic-admin.md).

---

## 10. A management command

Django's `manage.py` equivalent:

```bash
gombit make command backfill_done --package task
```

`--package task` writes into your existing feature package
(`internal/task/`) instead of the default `internal/commands/`, and
registers the command on **your** CLI at `cmd/gombit`, again via `go/ast`.
The command name is normalized to kebab-case for the CLI regardless of how
you typed it, so `backfill_done` becomes the runnable `backfill-done`. Run
it:

```bash
go run ./cmd/gombit backfill-done
```

Your binary keeps the whole framework tree — `db`, `routes`, `doctor`,
`createsuperuser` — plus your own commands.

**✅ Checkpoint** — `go run ./cmd/gombit --help` lists `backfill-done`.

---

## 11. Ship it

```bash
gombit build --embed
```

`--embed` runs the frontend build and compiles the assets into the Go binary
with `go:embed`, giving you **one artifact** that serves the API, the SPA, and
the admin. Without the flag you get a plain backend build and deploy the
frontend separately — both are supported.

Configuration is environment-driven and typed:

```bash
gombit config show
```

Secrets — database passwords, `Cache.Redis.Password`, `Auth.JWTSecret` — are
redacted in that output.

Before deploying, check the environment:

```bash
gombit doctor
```

Production checklist:

| Setting | Value |
| --- | --- |
| `GOMBIT_ENV` | `production` |
| `GOMBIT_JWT_SECRET` | strong, unique, not in source control |
| `GOMBIT_DOCS_ENABLED` | off |
| Cookie `Secure` | `true` (config validation requires it in production) |
| Database | run `gombit db migrate`; never AutoMigrate |

**✅ Checkpoint** — the built binary serves the API, the SPA, and `/admin/` on
one port with no `node` on the box.

---

## 12. Where next

You've used every subsystem in v0.1. The reference docs go deeper:

| Topic | Doc |
| --- | --- |
| All commands and flags | [cli.md](cli.md) |
| Configuration | [config.md](config.md) |
| Lifecycle hooks | [lifecycle.md](lifecycle.md) |
| Raw Gin routes | [router.md](router.md) |
| DTO and validation conventions | [contract.md](contract.md) |
| OpenAPI and `/docs` | [openapi.md](openapi.md) |
| Client generation and drift | [client.md](client.md) |
| Migrations | [migrations.md](migrations.md) |
| Databases | [database.md](database.md) |
| Bearer auth | [auth.md](auth.md) |
| Cookie auth and CSRF | [auth-cookie.md](auth-cookie.md) |
| Admin | [admin.md](admin.md) |
| Single-binary builds | [build.md](build.md) |
| Everything | [docs index](README.md) |

Architecture rationale lives in the [ADRs](adr/). Scope and roadmap live in
[GOMBIT_BUILD_PLAN.md](GOMBIT_BUILD_PLAN.md) — including the post-v0.1
batteries (jobs, events, scheduler, mail, storage, gRPC, multi-tenancy, i18n)
that are deliberately **not** here yet.

Something wrong or unclear in this tutorial? That's a docs bug —
[open an issue](https://github.com/gombit-dev/gombit/issues/new/choose).
