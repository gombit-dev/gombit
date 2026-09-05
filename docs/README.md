# Gombit documentation

New here? [Install](installation.md), then work through the
[tutorial](tutorial.md).

## Getting started

| Doc | What it covers |
| --- | --- |
| [installation.md](installation.md) | Prerequisites, install paths, verification, troubleshooting |
| [tutorial.md](tutorial.md) | Build a task app end to end — API, migration, client, React, auth, admin |
| [cli.md](cli.md) | Every command and flag: `new`, `dev`, `build`, `make`, `db`, `openapi`, `client`, `routes`, `doctor`, `config`, `createsuperuser`, `version` |

## Runtime

| Doc | What it covers |
| --- | --- |
| [config.md](config.md) | Typed `config.Config`, environment variables, redaction |
| [lifecycle.md](lifecycle.md) | `framework.App`, `OnStart` / `OnStop` hooks, graceful shutdown |
| [health.md](health.md) | The `/livez` + `/readyz` health probes and the host readiness contract |
| [router.md](router.md) | Application-owned route registration and the raw `*gin.Engine` escape hatch |
| [logging.md](logging.md) | Runtime logging |
| [cache.md](cache.md) | Cache runtime (memory, Redis, noop) |

## Data

| Doc | What it covers |
| --- | --- |
| [database.md](database.md) | GORM setup and the SQLite / PostgreSQL / MySQL support matrix |
| [migrations.md](migrations.md) | Atlas-backed `gombit db` migrations, revisions, rollback |
| [validation.md](validation.md) | Model `Validate` hooks (API + admin), `app.Tx`, and optimistic locking |

## Contract

| Doc | What it covers |
| --- | --- |
| [contract.md](contract.md) | Huma DTO and validation conventions, the D10 response envelope |
| [openapi.md](openapi.md) | OpenAPI 3.1 emission and the `/docs` UI |
| [client.md](client.md) | TypeScript client generation and the contract drift check |

## Frontend

| Doc | What it covers |
| --- | --- |
| [frontend.md](frontend.md) | The Vite + React + TypeScript skeleton |
| [frontend-mui.md](frontend-mui.md) | The MUI CRUD preset (`--ui mui`) |
| [build.md](build.md) | Optional single-binary production builds (`gombit build --embed`) |

## Auth and admin

| Doc | What it covers |
| --- | --- |
| [auth.md](auth.md) | Bearer JWT login and refresh rotation (the API default) |
| [auth-cookie.md](auth-cookie.md) | Cookie/session auth, CSRF double-submit, threat model |
| [admin.md](admin.md) | The admin registry, introspection API, SPA, and permissions |

## Project

| Doc | What it covers |
| --- | --- |
| [GOMBIT_BUILD_PLAN.md](GOMBIT_BUILD_PLAN.md) | **Authoritative** scope, locked decisions, and the issue backlog (§4) |
| [GO_FULLSTACK_FRAMEWORK_DESIGN.md](GO_FULLSTACK_FRAMEWORK_DESIGN.md) | Long-form rationale — prose only, never a source of scope |
| [releasing.md](releasing.md) | Maintainer release runbook |
| [adr/](adr/) | Accepted architecture decision records |
| [plans/](plans/) | Per-issue implementation plans |

## Architecture decisions

| ADR | Decision |
| --- | --- |
| [011](adr/011-contract-layer-huma.md) | Huma-typed handlers are the contract source of truth |
| [012](adr/012-migrations-atlas-gorm-provider.md) | Migrations wrap Atlas + `atlas-provider-gorm` |
| [013](adr/013-runtime-generic-admin.md) | The admin is a runtime surface over an explicit registry |
| [014](adr/014-cli-cobra.md) | Cobra is the CLI framework |
| [015](adr/015-host-deployment-contracts.md) | Host/deployment contracts: application contract, health convention, migration safety manifest |

## Contributing

[CONTRIBUTING.md](../CONTRIBUTING.md) · [CODE_OF_CONDUCT.md](../CODE_OF_CONDUCT.md) · [SECURITY.md](../SECURITY.md) · [CHANGELOG.md](../CHANGELOG.md)
