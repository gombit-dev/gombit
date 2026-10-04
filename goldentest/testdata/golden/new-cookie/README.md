# demo

Scaffolded by `gombit new`. Feature-package layout is build plan §3.2.

## Run

```sh
# gombit new already wrote a gitignored .env with a generated JWT secret.
# Do not copy .env.example over it — that file uses the public development
# placeholder, which production rejects. Recreate .env only if it is missing.
go run ./cmd/server
```

The app-owned CLI embeds the framework Cobra tree and registers feature
commands explicitly (no reflection):

```sh
go run ./cmd/gombit --help
go run ./cmd/gombit make command greet
go run ./cmd/gombit greet
```

The server listens on `GOMBIT_HTTP_ADDR` (default `:8080`). Public API
routes are under `/api/v1`. Interactive docs are at `/docs` when
`GOMBIT_DOCS_ENABLED` is on. The live OpenAPI 3.1 document is
`/openapi.json`.

For local development, one command runs the API, Vite HMR, and live
TypeScript client regeneration:

```sh
gombit dev
```

`gombit dev` prints a service table (Backend, Frontend, OpenAPI, API docs, Admin).
Backend reload uses `air` or `watchexec` when installed; otherwise it falls
back to `go run ./cmd/server`. The frontend uses `pnpm` when available,
otherwise `npm`. Node.js is required for Vite HMR. The frontend is a Vite +
React + TypeScript skeleton (router, generated client, React Hook Form,
HttpOnly cookie session + CSRF login — see
[`docs/auth-cookie.md`](https://github.com/gombit-dev/gombit/blob/main/docs/auth-cookie.md)
for the threat model).

Split deploy is the default. For a single binary that serves API + static +
SPA fallback:

```sh
gombit build --embed
./bin/server
```

`go run ./cmd/server` still works without a Vite `dist`. See
[`docs/build.md`](https://github.com/gombit-dev/gombit/blob/main/docs/build.md).

This module requires [`github.com/gombit-dev/gombit`](https://github.com/gombit-dev/gombit).
After scaffolding, pin a released version:

```sh
go get github.com/gombit-dev/gombit@latest
go mod tidy
```

The default module path is `github.com/example/demo` (override with
`--module`).

## Recorded choices (`gombit.yaml`)

| Key | Value | Notes |
| --- | --- | --- |
| database | `sqlite` | sqlite, postgres, or mysql |
| cache | `memory` | memory, redis, or noop |
| auth | `cookie` | Bearer JWT (v0.1 default) or `cookie` (HttpOnly session + CSRF, see `docs/auth-cookie.md`) |
| ui | `minimal` | minimal/headless default; `--ui mui` scaffolds the MUI CRUD preset (AppBar, Table, TextField) |

Runtime still reads `GOMBIT_*` environment variables, not `gombit.yaml`.

## Layout

- `cmd/server` — `config.Load`, `framework.New`, explicit `product.Register`, optional embed via `internal/web`
- `cmd/gombit` — framework Cobra tree plus `product.RegisterCommands(root)`
- `internal/product` — model, Huma handlers, routes (no `service.go` / `repo.go`); `RegisterCommands` for CLI hooks
- `internal/platform` — database open + AutoMigrate for the framework tables (auth, storage claims) and the example product
- `internal/web` — `go:embed` hook (`static/.keep` placeholder; `gombit build --embed` collectstatics here)
- `database/migrations`, `database/seeds` — Atlas SQL (see `gombit db`)
- `frontend/` — Vite + React + TypeScript minimal skeleton (`gombit dev`)
- `storage/` — files stored by the local driver (`GOMBIT_STORAGE_LOCAL_ROOT`; created on the first write, gitignored). Set `GOMBIT_STORAGE_DRIVER=s3` for an S3-compatible store; see `.env.example`

## Migrations

```sh
gombit db makemigrations create_products \
  --model github.com/example/demo/internal/product.Product
gombit db migrate
gombit db status
```

Example product handlers use GORM AutoMigrate on startup so the list/create
API works before the first Atlas migration exists. Prefer `gombit db` for
schema you intend to keep.

## Add a resource

```sh
gombit make resource Book title:string:required
```

That writes `internal/book/` (model, thin Huma handler, routes), React
list/form pages, and registers `book.Register(app)` in
`cmd/server/main.go` via `go/ast`. Re-running is idempotent and will not
clobber edits unless you pass `--force`. `--service` / `--repo` are opt-in.

Frontend pages import types from `frontend/src/api/generated` and map D10
`error.fields` into React Hook Form. Run `gombit client generate` or
`gombit dev` after the API is up.

If Atlas is not installed, the GORM model is still ready for:

```sh
gombit db makemigrations create_books --model github.com/example/demo/internal/book.Book
```

## Add a management command

```sh
gombit make command greet
```

That writes `internal/commands/greet.go`, appends
`cli.AddCommand(root, NewGreetCommand())` in `internal/commands/commands.go`,
and registers `commands.RegisterCommands(root)` in `cmd/gombit/main.go` via `go/ast`.
Re-running is idempotent. Then:

```sh
go run ./cmd/gombit greet
```

## Create an admin account

```sh
gombit createsuperuser --email admin@example.com --password correct-horse-battery-staple
```

Requires `GOMBIT_JWT_SECRET` to be set (auth on, Bearer or cookie mode;
`.env` already has one). Prompts interactively when `--email` / `--password` are omitted and
stdin is a TTY. Refuses duplicate emails and hashes the password with the
same bcrypt hasher as `/auth/register`. See
[`docs/cli.md`](https://github.com/gombit-dev/gombit/blob/main/docs/cli.md#gombit-createsuperuser).

