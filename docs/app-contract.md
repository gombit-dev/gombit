# Application contract

The **application contract** is a stable, machine-readable description of a
Gombit app that a deployment host (a container platform, CI, or
[Gombit Cloud](adr/015-host-deployment-contracts.md)) reads to learn how to
**build, health-check, and migrate** the app — without inferring anything from
the source tree. It is the HOST-1 contract from
[ADR-015](adr/015-host-deployment-contracts.md).

```bash
gombit contract app          # print the contract to stdout
gombit contract app --out app.json
gombit contract app --dir ./path/to/project
```

## Output

```json
{
  "contract_version": 1,
  "framework": { "name": "gombit", "version": "v0.5.0" },
  "build": { "command": "gombit build --embed", "artifact": "bin/server" },
  "runtime": {
    "http_port": 8080,
    "health": { "live": "/livez", "ready": "/readyz" }
  },
  "database": { "required": false, "driver": "sqlite" },
  "migrations": { "path": "database/migrations" }
}
```

## Every field is *declared*, never inferred

Gombit projects the contract from configuration the app already declares — it
never guesses by scanning files, directory names, or README text (build plan
§10, principle 6.2). `--dir` selects one project directory that **both** the
config (`.env`) and `go.mod` are read from, so a monorepo cannot mix one app's
`go.mod` with another's config (process `GOMBIT_*` env still applies, as it
would at runtime):

| Field | Source |
| --- | --- |
| `contract_version` | The schema version this Gombit emits. Independent of the framework version, so a host can fail loudly on an unknown shape. |
| `framework.version` | The `github.com/gombit-dev/gombit` **require** directive in the project's `go.mod`. |
| `build.command` / `artifact` | The documented production build (`gombit build --embed`, `bin/server`). |
| `runtime.http_port` | The port of `config.HTTP.Addr` (`gombit.yaml` / `GOMBIT_HTTP_ADDR`). |
| `runtime.health` | The fixed `/livez` + `/readyz` probes (HOST-2, [health.md](health.md)). |
| `database.driver` | `config.Database.Driver`. |
| `database.required` | The declared `config.Database.Required` (`GOMBIT_DATABASE_REQUIRED`, default `true`) — whether a host must provision a database for this app. It is a real config value, not a proxy for auth; an app that needs no database sets it `false`. |
| `migrations.path` | The versioned-migrations directory (`database/migrations`). |

## Failure is loud

`gombit contract app` does not emit a half-true contract:

- If `go.mod` does not require `github.com/gombit-dev/gombit`, it is not a
  Gombit app — error.
- If a **replace** directive points the framework at a local checkout, the
  version is unresolvable for a host — error, with guidance to build against a
  published release. Pin a real release before generating a contract a host
  will consume.

## Stability

`contract_version` is bumped only on a breaking shape change, documented in the
CHANGELOG. See [ADR-015](adr/015-host-deployment-contracts.md) for the decision
and how this relates to the health convention (HOST-2) and the migration safety
manifest (HOST-3).
