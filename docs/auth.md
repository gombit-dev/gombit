# Bearer JWT auth

Gombit's v0.1 API default is **Bearer JWT with refresh rotation** (C3 / D3).
Cookie/session + CSRF ([M5-3]) is a first-class alternative mode
(`gombit new --auth cookie`) — see [`docs/auth-cookie.md`](auth-cookie.md)
for its threat model; this page documents the Bearer default.
`gombit createsuperuser` ([M4-6]) is the CLI admin seed path; see
[cli.md](cli.md#gombit-createsuperuser).

Behavior lives in the `auth` runtime package. `framework.New` mounts the
Huma routes when `GOMBIT_JWT_SECRET` is set **and** a database is attached.
Generated apps (`gombit new --auth jwt`, the default) wire this through
`config.Load` + `framework.WithDatabase`; they do not copy handler code.

## Token storage (SPA)

| Token | Where | Notes |
| --- | --- | --- |
| Access JWT | **Memory only** | `Authorization: Bearer`. Never `localStorage` / `sessionStorage`. Lost on refresh, which is intended. |
| Refresh token | **Memory only** (JSON body) | Returned once on login/refresh. Rotated on each successful refresh. Not a cookie in v0.1. |

The refresh token stays in the JSON body (not a cookie) so this mode's
transport does not mix with `--auth cookie`'s HttpOnly session cookies
(see [`docs/auth-cookie.md`](auth-cookie.md)). Do not put tokens in
`VITE_*`.

## Endpoints

All paths use `config.API.Prefix` (default `/api/v1`). D10 envelopes.

| Method | Path | Auth |
| --- | --- | --- |
| `POST` | `/auth/register` | Public. Demo/bootstrap seed path (email + password); never sets `IsSuperuser`. |
| `POST` | `/auth/login` | Public. Returns `access_token`, `refresh_token`, `token_type`, `expires_in`. |
| `POST` | `/auth/refresh` | Public. Body `{ "refresh_token" }`. Issues a new pair; the old refresh token is invalid. |
| `POST` | `/auth/logout` | Public. Body `{ "refresh_token" }`. Revokes that refresh token. Bound access JWTs then fail. |
| `GET` | `/me` | Bearer access JWT. Example protected route for E2E. Missing/invalid Bearer is 401 with `WWW-Authenticate: Bearer realm="api"`. |
| `GET` | `/auth/sessions` | Bearer access JWT. Lists the user's active sessions; see [Sessions](#sessions). |
| `DELETE` | `/auth/sessions/{id}` | Bearer access JWT. Revokes one session; 404 if `id` names no active session of the user. |
| `DELETE` | `/auth/sessions?scope=others\|all` | Bearer access JWT. `others` revokes every session except the current one; `all` revokes every session, the current one included. |

Passwords are hashed with bcrypt. Access JWTs are HS256, bound to the refresh
row so logout, session revocation, and reuse of a rotated refresh token 401
`/me` immediately, not when the access JWT expires. Presenting an
**already-rotated** refresh token again is treated as theft and revokes all of
that user's sessions. A refresh token revoked by logout or session revocation
is just invalid (401): a revoked device retrying its refresh must not end the
session that revoked it.
Concurrent refresh of the **current** still-valid token (two tabs, parallel
`POST /auth/refresh`) shares one rotation and does not family-revoke the
winner.

## Sessions

A session is one sign-in: the chain of refresh tokens a login starts and each
refresh extends. `GET /auth/sessions` returns the user's active sessions, most
recently refreshed first:

```json
{
  "data": [
    {
      "id": "3f6c2a9e0b7d41c58e2f6a1d9c4b7e20",
      "last_refreshed_at": "2026-09-28T14:05:00Z",
      "expires_at": "2026-10-05T14:05:00Z",
      "current": true
    }
  ]
}
```

- `id` is opaque (an HMAC of the refresh row id under the JWT secret), so the
  API never exposes row ids, which are sequential. The access JWT's `rid`
  claim is still the row id, and the JWT is readable by its holder, so the
  opaque id hides row ids in the sessions API only, not from a client that
  decodes its own token. The id **changes every time the
  session refreshes**: list again right before revoking. Revoking an id that
  has since refreshed returns 404 rather than silently missing.
- `last_refreshed_at` is when the session last signed in or refreshed. An
  active client refreshes about once per access-token TTL, so that is its
  precision; it is not a per-request "last seen".
- `current` marks the session that made the request, also when that
  session refreshed while the request ran: it is then marked under its new id.

In the OpenAPI document the session schemas are named `AuthSession`,
`DataListAuthSession`, `AuthRevokeResult`, and `DataAuthRevokeResult`, and
the operations `auth-list-sessions`, `auth-revoke-session`, and
`auth-revoke-sessions`. Huma names schemas after Go types in one namespace
the whole app shares, and panics at boot on a duplicate name, so the prefix
leaves `Session` and `list-sessions` free for an application's own types and
for `gombit make resource Session`.

Revocation takes effect on the revoked session's next request. A session is
active while its refresh row is unrevoked and unexpired, and the access JWT
follows that: it also stops authenticating when its session expires, even if
the JWT's own lifetime (`GOMBIT_JWT_ACCESS_TTL`) is longer than the
refresh token's, so a token never outlives a session that has left the list
and can no longer be revoked.

What is serialized: token rotation (`POST /auth/refresh`), revoking one
session, revoking the others, and revoking all of a user's sessions each lock
the user's row first, so they run one at a time per user. The refresh-token
reuse cascade, which ends all of a user's sessions when an already-rotated
token is presented again, runs inside the rotation and takes the same lock.
What that guarantees depends on the operation, when a session refreshes at
the same moment:

- **Revoking the others, revoking all, and the reuse cascade** cover a
  concurrent rotation. A session that rotates while one of them runs cannot
  slip out with its new tokens: whichever comes second waits for the other, so
  the new tokens are revoked too (revoking the others keeps the requesting
  session, including any rotations it made since the request was
  authenticated).
- **Revoking one** answers 404 when its session rotated first. It waits for
  the lock, finds that the id it was given belongs to a token that has since
  been replaced, and revokes nothing: the session is still alive under its new
  id. The client lists again and retries, as the 404 above says. A logout of
  the same session that lands while the revocation runs also makes it a 404
  (the session has already ended), so a success always means the revocation
  ended the session itself.
- **Logout** (`POST /auth/logout`, `RevokeRefresh`) is **not** serialized: it
  does not take the lock and revokes only the refresh token it is given. A
  logout that races a refresh of the same token can therefore leave the
  refresh's new session alive. That is a known limitation; to be sure a user
  is signed out everywhere, revoke all their sessions.
- **Login** does not take the lock either. A login that completes while a
  revocation runs starts a session the revocation does not cover, even if it
  checked the password before the revocation began. Revoking all of a user's
  sessions ends the sessions that exist at that moment; an application that
  must also exclude a login in flight (a password reset, say) needs more than
  `RevokeAllSessions`.

The `auth.Service` methods `ListSessions`, `RevokeSession`, and
`RevokeAllSessions` are exported for application code (for example, an admin
action that signs a user out everywhere). Revoking "the others" is not
exported: it needs the request's current session, which only the HTTP
handlers know. Revoked and expired refresh rows are kept; nothing prunes them yet.

`User.IsSuperuser` bypasses all permission checks.
`gombit createsuperuser` is the only built-in path that sets it;
`/auth/register` never does. Regular users receive `Permission` keys
directly or through `Group` membership. See [admin.md](admin.md) for the
ADMIN-3 assignment helpers and admin enforcement rules.

## Config

| Variable | Field | Default |
| --- | --- | --- |
| `GOMBIT_JWT_SECRET` | `Config.Auth.JWTSecret` | empty (auth unmounted) |
| `GOMBIT_JWT_ACCESS_TTL` | `Config.Auth.AccessTokenTTL` | `15m` |
| `GOMBIT_JWT_REFRESH_TTL` | `Config.Auth.RefreshTokenTTL` | `168h` (7 days) |

Production rejects a **non-empty** JWT secret shorter than 32 characters, and
the generated-app development placeholder, at `config.Load` / `Validate` and
`gombit doctor` (Appendix C). The secret is redacted by `Config.Redacted()`
and `gombit config show`. Empty in production leaves Bearer auth off; set a
long random secret to enable it.

`Config.Auth.Mode` (`GOMBIT_AUTH_MODE`, default `jwt`) selects between this
Bearer surface and `cookie` mode; see
[`docs/auth-cookie.md`](auth-cookie.md#config) for the cookie-only fields
(`GOMBIT_COOKIE_SECURE`, `GOMBIT_COOKIE_SAMESITE`) and their production
requirements.

`gombit new` writes a gitignored `.env` with a per-project random HMAC secret.
Generated `.env.example` keeps a short development placeholder for
documentation. Do not `cp .env.example .env` over an existing `.env` — that
replaces the per-project secret with the public placeholder. Production
rejects that value.

## Generated frontend

`frontend/src/auth/session.ts` holds both tokens in module variables.
`createAppClient` sends the access token and, on 401, rotates `/auth/refresh`
once. Concurrent 401s wait on that same refresh and retry instead of failing
with the stale response. The retry rebuilds the request from buffered body
bytes so POST/PATCH JSON is resent after silent refresh (buffering is gated
on method because Firefox's Request.body getter is unimplemented). `RequireAuth`
sends anonymous users to `/login`.
Logout clears memory and revokes the refresh token.

Product pages sit behind `RequireAuth`. Register from the login page is a
demo/bootstrap path, not a full identity product.

## Example

```sh
go run ./examples/auth
```

See [`examples/auth`](../examples/auth) and [`docs/frontend.md`](frontend.md).
