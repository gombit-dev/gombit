# Feature conventions (locked)

Do not re-litigate these. Full text: `AGENTS.md` and the ADRs in `docs/adr/` (the historical `docs/GOMBIT_BUILD_PLAN.md` §1–§3 is where they started).

## Contract pipeline

```
Huma-typed handler (input/output structs, validated)
        ↓  Huma emits
OpenAPI 3.1  (/openapi.json, `gombit openapi generate`)
        ↓  openapi-typescript
TypeScript types
        ↓  openapi-fetch + thin wrapper
React client
```

- Anything in the public API is a Huma handler.
- Raw `*gin.Engine` (`app.Router()`) is the tested escape hatch for non-contract routes and must **not** appear in the OpenAPI doc.
- Drift between server and generated client fails CI.

## Envelope (D10)

Success:

```json
{"data": {}, "meta": {}}
```

Error:

```json
{"error": {"code": "validation_error", "message": "...", "fields": {"email": ["..."]}, "request_id": "..."}}
```

Do not emit `{"error":"string"}` or a new wrapper.

## Auth

- v0.1 API default: Bearer JWT + refresh rotation.
- Generated frontend stores the access token **in memory only**. Never `localStorage`.
- Session/cookie (`--auth cookie`) is first-class (HttpOnly / Secure / SameSite + CSRF). Required before the admin milestone.
- `X-API-Key` is off by default for browser apps; server-to-server only.

## Persistence

- GORM is the ORM. Expose `*gorm.DB`; do not invent a universal ORM interface.
- v0.1 databases: SQLite + PostgreSQL + MySQL.
- Migrations: Atlas GORM provider (Program Mode) + `atlas migrate diff`. No hand-rolled DSL.
- Migration metadata: `version, name, batch, applied_at`. No checksums (D4).
- Optional generic repo `repository.New[T]` lives in the **runtime**, never generated per model.

## CLI

- Cobra (`spf13/cobra`) is the command framework (D13 / ADR-014).
- Nested families and M4-7 app-registered management commands use Cobra
  `AddCommand`. Do not adopt Kong or grow the pre-M4 stdlib `flag` router as
  the long-term architecture; M4-1 is the adoption / migration point.

## Frontend

- Vite + React + TypeScript. Minimal/headless UI is the default; MUI is `--ui mui`.
- Package manager: detect (prefer `pnpm`, else `npm`); default `npm` when none present.
- Forms: map D10 `error.fields` into field errors (React Hook Form in the skeleton).

## Security defaults (draft Appendix C)

Production must reject or loudly fail:

- JWT secret too short
- cookie auth without Secure under HTTPS production URL
- wildcard CORS with credentials
- trusted proxies = all without explicit opt-in
- debug Gin mode in production
- embedded browser API secret
- SQLite path unwritable
- pending required migrations
- Redis selected but unreachable when required for auth/session
