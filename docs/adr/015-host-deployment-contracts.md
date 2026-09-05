# ADR-015: Host / Deployment Contracts

## Status

**Accepted.** This ADR locks the shape of three framework-level contracts so
their implementation issues (HOST-1, HOST-2, HOST-3) are not blocked. No
runtime or CLI code ships with this ADR. Acceptance is the go/no-go gate for
HOST-1..3, exactly as ADR-013 (ADMIN-0) gated ADMIN-1..3.

## Context

An external consumer — **Gombit Cloud**, a proprietary managed deployment
platform in a separate repository — needs to build, deploy, and operate an
**ordinary handwritten Gombit application** without inventing private
assumptions about it. Its design (`gombit-cloud/docs/DESIGN.md` §3.1, §9, §24,
§29–§31, L4) is explicit that the contracts it consumes must live **upstream in
Gombit**, not privately in Cloud:

> These contracts must not live privately inside Cloud. (DESIGN.md §3.1)

Three such contracts do not exist in Gombit today:

1. A **machine-readable application description** (framework version, build
   command, artifact path, runtime ports/health paths, database requirement,
   migrations location) — DESIGN.md §9.
2. A **standard runtime health convention** a host can poll to gate traffic —
   DESIGN.md §24.
3. A **migration safety manifest** classifying whether a migration is
   destructive, plus a verifier that proves the classification matches the
   executable SQL — DESIGN.md §29–§31.

This is **new scope for Gombit** and it is **not on the v0.1 critical path**.
Per the working agreement (build plan §5.7) v0.1 is one CRUD loop; these
contracts are a **post-v0.1 host-integration track (`HOST`)**, deliberately
kept off M1–M5. Gombit is MIT and these contracts are generally useful to *any*
host (Docker `HEALTHCHECK`, Kubernetes probes, CI gating), not only Cloud —
consistent with DESIGN.md §69/L20 keeping shared contracts open. Gombit does
not depend on Cloud and must remain runnable and deployable without it
(DESIGN.md §71/L18).

### Locked constraints this ADR must encode, not reopen

- **Generate-vs-runtime rule (§3.3).** Behavior lives in the versioned runtime;
  generated code is a thin scaffold the user owns. The health convention is
  **runtime** behavior, not a per-app generated handler. The application
  contract is **derived** from the app's own declared config, never copied as a
  forkable file the framework later can't evolve.
- **Principle 6.2 (explicit Go, minimal magic).** No request-time reflection,
  no repo-trivia inference ("if `cmd/server` exists…", "if README says
  postgres…" — DESIGN.md §10). The application contract is projected from
  **declared** sources (`gombit.yaml`, the module's framework version), not
  guessed.
- **C1 / OpenAPI.** Public HTTP contract is Huma-typed and in OpenAPI 3.1. The
  health endpoints are **operational**, not application API: like the admin SPA
  and `/metrics`, they mount on raw Gin (`app.Router()`) and stay **out** of
  OpenAPI. They are documented as a stable operational contract instead.
- **D10 envelopes.** Health JSON bodies use the `{"data": …}` success shape
  already emitted by `/livez` and `/readyz`.
- **D4 (no migration checksums).** D4 rejected content hashes for *drift*
  detection because formatting changes the hash without changing schema. The
  safety manifest's `sql_sha256` (HOST-3) serves a **different** purpose —
  anti-tamper binding between the SQL a maintainer reviewed and the SQL a host
  applies — and does not reintroduce checksum-based drift detection. HOST-3
  must state this distinction; it does not touch `framework_migrations` (still
  version/name/batch/applied_at).
- **Atlas (ADR-012).** HOST-3 does **not** hand-roll a migration engine or DSL.
  It reads the versioned SQL that `gombit db makemigrations` already produces
  and classifies it.
- **Scope guard.** This track adds no M6 battery (jobs, events, scheduler,
  mail, storage, gRPC, multi-tenancy, i18n). It adds contracts, not runtime
  services.

Names below (`gombit.app.json`, `gombit contract app`, `gombit db verify`,
field names, the `manifest_version`) are **provisional** — documentation for
implementers. This ADR adds no code.

## Decision

Gombit gains a **`HOST` post-v0.1 track**: three declarative, verifiable
contracts a deployment host consumes. Each is derived from sources Gombit
already owns; none requires the app to have been produced by Forge or deployed
on Cloud.

### 1. Application contract (HOST-1)

Gombit emits a **stable, versioned, machine-readable** description of an
application, projected from its **declared** configuration and the framework
version it builds against — never from repository heuristics (§10, 6.2).

Shape (provisional; the schema is versioned by `contract_version`):

```json
{
  "contract_version": 1,
  "framework": { "name": "gombit", "version": "0.5.0" },
  "build":     { "command": "gombit build --embed", "artifact": "./bin/server" },
  "runtime":   { "http_port": 8080, "health": { "live": "/livez", "ready": "/readyz" } },
  "database":  { "required": true, "driver": "postgres" },
  "migrations":{ "path": "database/migrations" }
}
```

- **Source of truth is declared, not inferred.** `framework.version` comes from
  the module's resolved Gombit version (`cli.Version` / `ResolveFrameworkVersion`,
  REL-9). `database.driver`, ports, and the migrations path come from
  `gombit.yaml` / typed `config.Config`. The build command/artifact are the
  documented `gombit build --embed` defaults (build plan M5-5). Nothing is
  discovered by walking the source tree.
- **Emission.** A CLI surface — provisionally `gombit contract app`
  (family alongside `gombit openapi` / `gombit client`) — writes the JSON to
  stdout or a file. Whether a copy is also written to the repo (e.g.
  `gombit.app.json`) is a HOST-1 detail; if written, it is a **generated,
  regenerable projection**, not a hand-edited source of truth (§3.3).
- **Versioning + explicit failure.** `contract_version` is independent of the
  framework version (DESIGN.md §53). A host reading an unknown
  `contract_version` must be able to fail loudly (DESIGN.md §55); the emitter
  stamps the integer it produces.
- **No new runtime dependency.** This is a build/CLI-time projection. It does
  not add an HTTP route and is not in OpenAPI.

### 2. Runtime health convention (HOST-2)

Gombit **already** serves `GET /livez` and `GET /readyz` on raw Gin
(`framework/app.go`), returning `{"data": {"status": "ok"}}`. HOST-2 promotes
these from ad-hoc routes to a **documented, stable operational contract** and
makes readiness meaningful.

- **`/livez` — liveness.** The process is up and the HTTP server is serving.
  Stays a cheap, dependency-free `200`. A host uses it to detect a hung
  process.
- **`/readyz` — readiness.** Reports whether the app can serve traffic: the
  configured datastore is reachable **and** the app is not shutting down.
  Returns `200` with `{"data":{"status":"ready"}}` when ready; **`503`** with a
  D10 error envelope (`{"error":{"code":"not_ready", …, "request_id": …}}`) when
  not. This is the signal a host gates traffic on (DESIGN.md §23/§24; Cloud
  "health before traffic", Invariant E). Today `/readyz` always returns `200`,
  which cannot gate traffic — HOST-2 fixes that. Start hooks (`OnStart`) need no
  separate readiness flag: `RunContext` begins serving only *after* start hooks
  succeed, so a request that reaches `/readyz` is already past startup. On
  graceful shutdown the probe flips to `503` first; an optional
  `WithShutdownDrainDelay` keeps the server accepting for that window so a host
  polling `/readyz` observes the `503` and deregisters the instance *before* the
  listener closes. With no delay (the default) the flag still sheds in-flight and
  keep-alive pollers during the shutdown grace period, but a fresh probe
  connection opened after shutdown begins is connection-refused rather than
  `503`. The reason string is a **fixed** value (`shutting down` /
  `datastore unavailable`), never the raw datastore error — a connection error
  can embed the DSN and `/readyz` is unauthenticated; the cause is logged.
- **Reconciliation with DESIGN.md §24.** Cloud's design floats `/healthz` as a
  *possible* convention and explicitly leaves room for "separate future
  readiness/liveness semantics." Gombit has already chosen the k8s-style
  `/livez` + `/readyz` split. **The contract is `/livez` + `/readyz`; Cloud
  consumes those and does not require `/healthz`.** This ADR records that so
  Cloud's issue is updated rather than Gombit growing a third alias.
- **Placement.** Operational, not application API. Stays on raw Gin, out of
  OpenAPI (same rationale as `/metrics` and the admin SPA). Documented in a
  new `docs/health.md` (or a section of `docs/lifecycle.md`) as the stable
  contract, including the exact bodies and status codes.
- **Config.** Paths remain fixed (`/livez`, `/readyz`) for the contract to be
  stable; if configurability is ever needed it is additive and out of scope
  here. Readiness checks must not themselves become a heavy dependency (no
  per-request migration scans); a cached/periodic readiness state is
  acceptable and a HOST-2 implementation detail.

### 3. Migration safety manifest + verifier (HOST-3)

Gombit defines a **manifest** classifying a migration's operations and a
**verifier** that proves the classification is consistent with the executable
SQL. A host (Cloud C3) consumes the verifier result and **never blindly
trusts** a `"safety"` field (DESIGN.md §31, L8).

Manifest shape (provisional; per DESIGN.md §30):

```json
{
  "manifest_version": 1,
  "migration": { "version": "20260901120000", "name": "drop_legacy_code" },
  "sql_sha256": "…",
  "requires_confirmation": true,
  "operations": [
    { "kind": "drop_column", "resource": "customers", "column": "legacy_code", "safety": "data_loss" }
  ]
}
```

- **Derived from Atlas output, not a new engine (ADR-012).** The verifier reads
  the versioned SQL already produced by `gombit db makemigrations` and
  classifies statements into a closed operation set (first cut:
  `create_table`, `add_column`, `create_index`, `drop_column`, `drop_table`,
  `drop_index`, `rename_*`, `alter_column`, `other`). Destructive kinds
  (`drop_column`, `drop_table`, narrowing `alter_column`, …) map to
  `safety: "data_loss"` and set `requires_confirmation: true`.
- **Verify, don't trust (§31).** The verifier is the authority: it parses the
  SQL and asserts the declared `operations`/`safety` match. A manifest claiming
  `non_destructive` while the SQL contains `DROP COLUMN` is a **verification
  failure**, not an accepted deploy. `sql_sha256` binds the manifest to the
  exact reviewed SQL (anti-tamper; see the D4 note in Context).
- **Surface.** A CLI surface — provisionally `gombit db verify` (and/or manifest
  emission folded into `gombit db makemigrations`) — produces and/or checks the
  manifest. Whether the manifest is written beside each migration
  (`NNNN_name.manifest.json`) or emitted on demand is a HOST-3 detail. It does
  **not** alter `framework_migrations` (D4).
- **Gombit does not gate its own deploys.** Gombit provides classification +
  verification. The *approval gate* (blocking a destructive migration pending
  human confirmation) is the **host's** policy (Cloud C3 §32/§33), not a
  framework runtime behavior. Gombit's CLI may surface a non-zero exit / warning
  for scripting, but Gombit does not implement Cloud's approval workflow.

### Rejected alternatives

- **Repo-trivia inference for the app contract** (detect framework by scanning
  files, infer DB from README). Violates §10 and principle 6.2; brittle and
  unversioned. Rejected in favor of projecting declared config.
- **A third `/healthz` alias.** Gombit already committed to `/livez`+`/readyz`;
  adding `/healthz` splits the contract for no gain. Cloud consumes the
  existing pair.
- **Putting health endpoints in OpenAPI / Huma.** They are operational, not the
  application API; same reasoning that keeps `/metrics` and the admin SPA out of
  the spec (C1, ADR-013 §2).
- **A new migration representation or checksum-based drift detection.** Violates
  ADR-012 and D4. The verifier reads existing Atlas SQL; `sql_sha256` is
  anti-tamper, not drift.
- **Implementing the destructive-migration approval gate in Gombit.** That is
  host policy (Cloud §32). Gombit classifies and verifies; it does not own the
  deploy workflow, or Gombit would grow a PaaS.
- **Requiring Forge or Cloud.** These contracts work for any handwritten Gombit
  app and any host (DESIGN.md L18); a Gombit app stays deployable without them.

## Consequences

- **HOST-1** implements the application-contract projection + emitter CLI, with
  tests covering the schema, `contract_version` stamping, and that values come
  from declared config (no tree-walking). Docs + example.
- **HOST-2** documents `/livez`+`/readyz` as the health contract and makes
  `/readyz` reflect real readiness (datastore + start hooks), returning `503`
  when not ready. Tests cover ready/not-ready transitions on the DB matrix.
  `docs/health.md` added.
- **HOST-3** defines the manifest format and the verifier over Atlas SQL, with a
  closed operation set and destructive classification. Tests must include the
  adversarial cases Cloud relies on (DESIGN.md §100): manifest hash mismatch,
  and a manifest claiming safe while the SQL drops a column. Docs + example.
- The three GitHub issues opened for these contracts are reconciled to `[HOST-1]`
  / `[HOST-2]` / `[HOST-3]` titles under the `post-v0.1` milestone, and a
  `[HOST-0]` ADR issue tracks this document (build plan §4 `HOST` track).
- Gombit acquires no dependency on Cloud or Forge. Nothing here enters the v0.1
  critical path or an M6 battery.
- Cloud's DESIGN.md §24 note (`/healthz`) is superseded by `/livez`+`/readyz`;
  the corresponding Cloud issue is updated to consume the existing endpoints.

## References

- Build plan §3.3 (generate-vs-runtime), §5 (working agreement), §4 `HOST` track
- ADR-011 (contract layer / Huma), ADR-012 (Atlas migrations), ADR-013 (runtime
  admin — the ADR-0-gates-implementation precedent), ADR-014 (Cobra CLI)
- D4 (no migration checksums), D10 (response envelope), REL-9
  (`ResolveFrameworkVersion`)
- `framework/app.go` (`/livez`, `/readyz`, `/metrics`), `migrations/revisions.go`
- Consumer: `gombit-cloud/docs/DESIGN.md` §9, §10, §24, §29–§31, §53, §55, §69,
  §71, §100; L8, L18, L20 (rationale only — not a source of Gombit scope)
- Issues: HOST-0 (this ADR), HOST-1 (#282), HOST-2 (#283), HOST-3 (#284)
