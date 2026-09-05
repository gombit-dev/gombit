# Health probes — the runtime health contract

Gombit's runtime serves two operational probes on every app. They are the
**stable contract a deployment host** (a container platform, Kubernetes, a CI
gate, or [Gombit Cloud](adr/015-host-deployment-contracts.md)) uses to decide
whether a process is alive and whether it should receive traffic.

| Probe | Path | Question it answers |
| --- | --- | --- |
| Liveness | `GET /livez` | Is the process up and serving HTTP? |
| Readiness | `GET /readyz` | Is it safe to send this instance traffic right now? |

Both are **raw Gin routes**, not application API. Like `/metrics` and the
`/admin/` SPA, they are deliberately **absent from OpenAPI** (`/openapi.json`,
`/docs`) — they describe the operational surface, not the app's contract.

They are installed on the default router. An app that supplies its own router
via `framework.WithRouter` owns the whole HTTP surface, including these probes;
the framework does not inject them there (the same caveat applies to `/metrics`).

## `/livez` — liveness

Always returns `200` with a D10 success envelope while the process is up:

```json
{ "data": { "status": "ok" } }
```

It performs no dependency checks. A host uses it to detect a hung or crashed
process and restart it. `/livez` must stay cheap and dependency-free — a
liveness probe that fails when the database is briefly unreachable would cause a
restart loop instead of letting readiness shed traffic.

## `/readyz` — readiness

Reports whether the instance can serve traffic. Ready:

```json
200 OK
{ "data": { "status": "ready" } }
```

Not ready:

```json
503 Service Unavailable
{ "error": { "code": "not_ready", "message": "<reason>", "request_id": "<id>" } }
```

An instance is **ready** when both hold:

1. **Not shutting down.** On graceful shutdown the probe flips to `503`
   *before* connections drain, so a host stops routing new requests to the
   instance while in-flight ones finish.
2. **The configured datastore is reachable.** When a database is attached
   (`framework.WithDatabase`), readiness pings it with a short timeout
   (2s). An app with no database attached is ready on criterion 1 alone.

Start hooks (`OnStart`) need no separate check: [`RunContext`](lifecycle.md)
begins serving **only after** every start hook succeeds, so any request that
reaches `/readyz` is already past startup.

The readiness ping is bounded so a hung datastore cannot hang the probe — and
with it, the host's traffic decision.

## Why liveness and readiness are separate

A host acts on them differently:

- **`/livez` fails** → the process is broken; **restart** it.
- **`/readyz` fails** → the process is fine but not ready (starting,
  datastore blip, draining); **stop sending traffic**, do not restart.

Collapsing them into one endpoint forces a host to either restart on a
transient dependency hiccup or route traffic to an instance that cannot serve
it. Gombit follows the Kubernetes-style split; there is intentionally **no
`/healthz` alias**.

## Using the probes

Kubernetes:

```yaml
livenessProbe:
  httpGet: { path: /livez, port: 8080 }
readinessProbe:
  httpGet: { path: /readyz, port: 8080 }
```

Docker:

```dockerfile
HEALTHCHECK CMD curl -fsS http://localhost:8080/readyz || exit 1
```

A host that gates traffic on healthy revisions should poll `/readyz` and route
only to instances returning `200`.

## Stability

The paths (`/livez`, `/readyz`), the ready body (`data.status`), and the
not-ready envelope (`error.code = "not_ready"`, `503`) are a stable contract.
Changes go through an ADR. See
[ADR-015](adr/015-host-deployment-contracts.md) for the decision and its
relationship to the other host/deployment contracts.
