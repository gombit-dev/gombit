# Application contract

The **application contract** is a stable, machine-readable description of a
Gombit app that a deployment host (a container platform, CI, or
[Gombit Cloud](adr/015-host-deployment-contracts.md)) reads to learn how to
**build, health-check, and migrate** the app, and which **processes** to run
from the build — without inferring anything from the source tree. It is the
HOST-1 contract from [ADR-015](adr/015-host-deployment-contracts.md), with the
processes added by HOST-4.

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
  "database": { "required": true, "driver": "sqlite" },
  "migrations": { "path": "database/migrations" },
  "processes": [
    { "name": "web", "args": [], "http": true, "stop_timeout_seconds": 22 },
    { "name": "worker", "args": ["worker"], "http": false, "stop_timeout_seconds": 58 }
  ],
  "redis": { "required": true }
}
```

That is an app on the `redis` jobs driver at the default shutdown settings. An
app on `sync` or `memory` lists the `web` process alone.

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
| `processes` | `web` always; `worker` when `config.Jobs.Driver` (`GOMBIT_JOBS_DRIVER`) is `redis`. See [Processes](#processes). |
| `processes[].stop_timeout_seconds` | The web process: `framework.WebKillAfter` of the shutdown settings (`GOMBIT_SHUTDOWN_TIMEOUT`, `GOMBIT_SHUTDOWN_DRAIN_DELAY`). The worker: `framework.WorkerKillAfterFor` of them and of the worker's (`GOMBIT_JOBS_WORKER_SHUTDOWN_TIMEOUT`). Rounded up to a second. |
| `redis.required` | Whether `config.Cache.Driver` or `config.Jobs.Driver` is `redis`. |

## Processes

One build runs as each process the contract lists, the artifact with that
process's `args`:

- **`web`** (`args: []`, `http: true`) serves `runtime.http_port` and the
  health probes. Route traffic to it only once `/readyz` passes.
- **`worker`** (`args: ["worker"]`, `http: false`) runs the app's
  [jobs worker](jobs.md#running-the-worker). It serves nothing. Run as many as
  the queues need.

A host starts each process with the same environment it projected the
contract with, and **no other arguments**: the worker reads its queues,
concurrency, lease and shutdown timeout from the `GOMBIT_JOBS_WORKER_*`
settings, the same settings its stop timeout is computed from.

To stop a process, send it SIGTERM (or SIGINT) and wait up to its
`stop_timeout_seconds` before killing it. That covers a graceful shutdown at
the declared settings: for the web process, the drain delay, the HTTP drain
and the stop hooks; for the worker, its in-flight jobs, their acks and
releases, and the stop hooks. A host that kills a process sooner cuts off a
request, a stop hook, or a job's ack, and that job then waits out its lease.
A shutdown timeout or drain delay the app sets in code
(`framework.WithShutdownTimeout`, `WithShutdownDrainDelay`) is not in the
contract: declare in configuration what a host must wait for.

When `redis.required` is true, the host provides a Redis server the app reaches
through `GOMBIT_REDIS_*`, or refuses the app.

### Projecting for a host's environment

The contract describes the configuration it was projected with, and the
process environment wins over `.env`, as it does at runtime. A host that
injects its own configuration, a jobs driver or a shutdown timeout per
environment say, runs `gombit contract app` with the environment it will start
the processes with, so the contract lists the processes and timeouts that
environment gets. A contract projected from the app's `.env` alone describes
the app as its own configuration declares it.

## Failure is loud

`gombit contract app` does not emit a half-true contract:

- If `go.mod` does not require `github.com/gombit-dev/gombit`, it is not a
  Gombit app — error.
- If a **replace** directive points the framework at a local checkout, or at
  another module (a fork, whose version is not a framework release), the
  version is unresolvable for a host — error, with guidance to build against a
  published release. Pin a real release before generating a contract a host
  will consume. A replace by the framework module itself at another version
  reports that version.
- The replace that counts is the one the go command applies: a replace of the
  required version wins over one of every version, and a replace of another
  version is ignored. A `go.mod` the go command refuses (the framework
  required twice, conflicting replaces) is an error. `gombit upgrade baseline`
  reads `go.mod` the same way.

## Stability

`contract_version` is bumped only on a breaking shape change, documented in the
CHANGELOG. A field can be added within a version: a host that doesn't know it
ignores it and behaves as before. `processes` and `redis` were added to version
1 after v0.10.0, so a contract from an older gombit lacks them: a host that
reads them treats their absence as a contract that predates them, not as an
app with no processes. See [ADR-015](adr/015-host-deployment-contracts.md)
for the decision and how this relates to the health convention (HOST-2) and
the migration safety manifest (HOST-3).
