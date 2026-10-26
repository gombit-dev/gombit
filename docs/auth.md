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
| `POST` | `/auth/verify-email` | Public. Body `{ "token" }` from the verification message; marks the email verified. See [Email verification](#email-verification). |
| `POST` | `/auth/verify-email/resend` | Bearer access JWT. Mails a new verification link (at most one a minute). |
| `POST` | `/auth/password/forgot` | Public. Body `{ "email" }`. Always 202 `{"data":{"accepted":true}}`; mails a reset link when the email is an account. See [Password reset](#password-reset). |
| `POST` | `/auth/password/reset` | Public. Body `{ "token", "password" }`. Sets the new password and revokes every session of the user. |
| `POST` | `/auth/password/change` | Bearer access JWT. Body `{ "current_password", "new_password", "keep_other_sessions"? }`. See [Password change](#password-change). |

Passwords are hashed with bcrypt. Access JWTs are HS256, bound to the refresh
row so logout, session revocation, and reuse of a rotated refresh token 401
`/me` immediately, not when the access JWT expires. Presenting an
**already-rotated** refresh token again before it expires is treated as theft
and revokes all of that user's sessions. Once it has expired it is just invalid
(401), so a stale copy cannot end the user's live sessions. The trade-off: a
stolen refresh token is detected only while the rotated token is unexpired; a
thief who rotated it first keeps the session if the legitimate client comes
back after that, until the user revokes it from the sessions list. A refresh
token revoked by logout or session revocation is just invalid (401): a revoked
device retrying its refresh must not end the session that revoked it.
Concurrent refresh of the **current** still-valid token (two tabs, parallel
`POST /auth/refresh`) shares one rotation and does not family-revoke the
winner.

## Email verification

A user's email starts **unverified**. Registration mails a verification
message, and posting its token proves the user owns the address:

```sh
curl -sX POST localhost:8080/api/v1/auth/verify-email \
  -H 'Content-Type: application/json' -d '{"token":"<token from the message>"}'
# {"data":{"id":1,"email":"ada@example.com","email_verified":true}}
```

`GET /me` and `POST /auth/register` report `email_verified`.

- **The token.** 32 random bytes, stored as its SHA-256 in
  `auth_one_time_tokens` (the raw token exists only in the message). It
  expires after `GOMBIT_AUTH_EMAIL_VERIFICATION_TTL` (24h), works **once**,
  and proves the address it was sent to: once the user's email is another
  one, it fails. An unknown, expired, used or superseded token is a 422 on
  `token`; the cases are not told apart.
- **Resending.** `POST /auth/verify-email/resend` (authenticated) mails a new
  token and **supersedes** the earlier ones: only the latest link works. A
  user may ask once a minute (429 with `Retry-After`), counted from the
  latest message delivered or still being sent; a verified user gets a 409.
  On SQLite, which has no row locks, two resends at the very same moment can
  both be sent.
- **Failures.** A new token is written *pending*, and does not work until
  its message was delivered: the mailer's `Send` returned nil. Delivery then
  activates it and supersedes the earlier tokens in one transaction. So
  whatever fails, either the new link works and no earlier one does, or
  nothing changed and the earlier link keeps working. A message the mailer
  could not send (503), or whose token could not be activated (500), is
  withdrawn, and the user can ask again at once; if withdrawing it fails
  too, the token stays pending (it never works) and counts as a message in
  flight until the minute is up.
- **Bound to the address.** A verification is for the address it proved
  (`User.VerifiedEmail`): once the user's email is another one, by any path
  (a GORM update, the admin), `User.EmailVerified()` is false and
  `RequireVerifiedEmail` denies until the new address is verified.
- **The link.** `GOMBIT_AUTH_EMAIL_VERIFICATION_URL` is the page the message
  links to, with `{token}` where the token goes
  (`https://app.example.com/verify-email?token={token}`); that page posts the
  token to `POST /auth/verify-email`. Unset, the message carries the bare
  token.
- **Requiring it.** `auth.RequireVerifiedEmail()` is a Huma middleware for an
  app's own operations, after the authentication gate. It answers 403 for an
  unverified user:

  ```go
  svc, _ := auth.NewService(db.DB, cfg)
  huma.Register(api, huma.Operation{
      OperationID: "publish-post", Method: http.MethodPost, Path: "/api/v1/posts/{id}/publish",
      Middlewares: huma.Middlewares{svc.RequireBearer(), auth.RequireVerifiedEmail()},
  }, publish)
  ```

  In cookie mode the gate is `svc.RequireCookieSession()`. Login does not
  require a verified email: an app decides which actions do.
- **Superusers.** `gombit createsuperuser` creates a verified user, and sends
  nothing. Users that existed before this release start unverified; to mark
  one verified, set both `email_verified_at` and `verified_email` (to its
  `email`).

### Sending the messages

The framework sends mail through one interface, `auth.Mailer`
(`Send(ctx, auth.Message)`), that the app implements over its email provider
and passes to `framework.WithAuthMailer` (or `auth.WithMailer`):

```go
type smtpMailer struct{ /* ... */ }

func (m smtpMailer) Send(ctx context.Context, msg auth.Message) error {
    // msg.To, msg.Subject, msg.Text: a plain default message;
    // msg.Kind, msg.URL, msg.Token, msg.ExpiresAt: for your own template.
}

app, err := framework.New(framework.WithAuthMailer(smtpMailer{}))
```

`msg.Token` is a credential: never log it. Without a mailer, development and
test apps log each message, link and token included (`auth.LogMailer`), so the
flow works locally; a production app sends none (registration still succeeds,
a resend is a 503) and logs a warning at startup. `Send` is synchronous: it
returns nil once the provider accepted the message, because that is when its
link starts working and the earlier ones stop. Don't return nil after only
queueing the message: the earlier link would be superseded before the new
one arrives. Registration sends its message on the request, so a slow mailer
makes registration slow.

Tests use `authtest.Mailer`, which records the messages instead of sending
them:

```go
mail := &authtest.Mailer{}
app, _ := framework.New(framework.WithAuthMailer(mail) /* ... */)
// register ada@example.com, then:
msg, _ := mail.Last("ada@example.com")
// POST msg.Token to /api/v1/auth/verify-email
```

## Password reset

A user who forgot their password asks for a reset link, and sets a new
password with the token it carries:

```sh
curl -sX POST localhost:8080/api/v1/auth/password/forgot \
  -H 'Content-Type: application/json' -d '{"email":"ada@example.com"}'
# 202 {"data":{"accepted":true}}
curl -sX POST localhost:8080/api/v1/auth/password/reset \
  -H 'Content-Type: application/json' \
  -d '{"token":"<token from the message>","password":"a-new-password"}'
# {"data":{"ok":true,"sessions_revoked":true}}
```

- **No enumeration.** `POST /auth/password/forgot` answers the same 202,
  body and headers whether or not the email is an account, and answers
  before that is known: the lookup and the message run in the background,
  so the timing does not tell either. Only an account gets a message. Its
  outcome (sent, the one-a-minute wait, a mailer failure) is logged, never
  answered.
- **Background work.** The message is sent by the auth Service's own
  workers (16), each task with a 30s deadline; the bookkeeping after the
  message (activating or withdrawing its token, the audit event) has its
  own 5s deadline and is cancelled by `Close` too. A Mailer must return
  once its context is done: Go cannot stop one that does not, and its
  worker is gone. Even then what the Service holds stays bounded: never
  more than 16 workers and 64 waiting requests.
- **Admission.** Whether a request is taken depends on when requests
  arrived, nothing else: a token bucket (a burst of 32, refilled at the rate
  the workers finish tasks in the worst case, 16 per 45s) spends one token
  per request, an account or not. A request the bucket refuses, or one
  arriving while the app stops, is a **503** with `Retry-After` (declared
  in the OpenAPI document), decided before the lookup. How long earlier
  requests' work runs (an account's send takes longer than an address that
  is none) never changes the answer, so saturating the endpoint tells
  nothing about which addresses are accounts. A 202 is a best-effort
  acceptance: while the workers keep to their deadlines the 64-request
  queue behind the bucket always has room, and every admitted request is
  sent; if they do not (a Mailer that ignores cancellation), a request the
  full queue cannot take is discarded before any lookup and logged, still
  answered with the same 202.
  `framework.App` closes the Service when it stops, before its stop hooks
  run: admitted messages are sent until the shutdown timeout, then
  cancelled. An app that mounts the routes itself keeps the Service
  `auth.Mount` returns and calls `svc.Close(ctx)` on shutdown, before
  closing its database. Per-client rate limits are AUTH-8's.
- **The token** is a [one-time token](#email-verification) like the
  verification one, for its own purpose (a verification token never resets
  a password, nor the reverse): random, stored hashed, pending until its
  message was delivered, works **once**, expires after
  `GOMBIT_AUTH_PASSWORD_RESET_TTL` (1h), and a newer one supersedes it. It
  proves the address it was sent to: once the user's email is another one,
  it fails. A bad token is a 422 on `token`, and a password outside the
  policy (at least 8 characters, at most 72 bytes, enforced by
  `ResetPassword` itself) a 422 on `password`; a refused password leaves
  the token usable. A token that cannot work (unknown,
  used, expired, or for an address the user no longer has) is refused
  before bcrypt runs, and a database that cannot answer that check fails
  the request (500), not open.
- **Sessions.** A reset revokes every session and refresh token of the user
  in the same transaction as the new password: whoever knew the old one is
  signed out (`sessions_revoked: true`). A login that checked the old
  password just before the reset cannot start a session after it: a login
  starts its session under the user's lock and only if the password it
  checked is still the user's ([Sessions](#sessions)), so it is either
  revoked by the reset or refused. The reset also voids every other reset
  token of the user, a message still being sent included: its link never
  works. `GOMBIT_AUTH_PASSWORD_RESET_KEEP_SESSIONS=true`
  keeps them. In cookie mode a reset that revoked the sessions also clears
  the browser's session cookies.
- **The link.** `GOMBIT_AUTH_PASSWORD_RESET_URL` is the page the message
  links to, with `{token}` where the token goes
  (`https://app.example.com/reset-password?token={token}`); that page asks for
  the new password and posts both to `POST /auth/password/reset`. Unset, the
  message carries the bare token.
- **Mail** goes through the same `auth.Mailer` ([Sending the
  messages](#sending-the-messages)), with `msg.Kind` `reset_password`.
- **Audit.** `auth.password.reset_requested` when a message is sent (a
  request for an address that is no account records nothing) and
  `auth.password.reset` when the password is set.
- **From Go.** `svc.RequestPasswordReset(ctx, email)` and
  `svc.ResetPassword(ctx, token, password)` are the same flow. The first
  returns nil for an address that is no account, but an error for an account
  in its wait: don't show its result to the person asking.

## Password change

A signed-in user changes their password by giving the current one:

```sh
curl -sX POST localhost:8080/api/v1/auth/password/change \
  -H "Authorization: Bearer $ACCESS" -H 'Content-Type: application/json' \
  -d '{"current_password":"correct-horse","new_password":"correct-horse-2"}'
# {"data":{"ok":true,"other_sessions_revoked":true}}
```

- **The current password is required.** A wrong one is a 422 on
  `current_password` (not a 401: the session is fine) and an
  `auth.password.change_failed` audit event; the password stays. bcrypt
  checks it outside any lock.
- **The new password** must have at least 8 characters and at most 72
  bytes (bcrypt's limit: a multibyte password reaches it in fewer
  characters). `ChangePassword` enforces that itself, not only the route's
  schema; a refused one is a 422 on `new_password`.
- **Sessions.** Every other session of the user is signed out, in the same
  transaction as the new password; the session that asked stays signed in.
  `"keep_other_sessions": true` keeps them. In cookie mode the browser keeps
  its cookies (its session is the one kept), and the route sits behind CSRF
  like every unsafe request.
- **Reset links** mailed before the change stop working: one must not undo
  it.
- **Races.** The change is written under the user's row lock, and only if
  the stored password is still the one checked: a reset or another change
  that commits in between wins, and this one is a 409 (try again with the
  current password). A login that checked the old password just before the
  change cannot start a session after it ([Sessions](#sessions)).
- **Audit.** `auth.password.changed` (`details.other_sessions_revoked`). No
  log line or audit row holds a password.
- **From Go.** `svc.ChangePassword(ctx, userID, current, next,
  auth.PasswordChange{Session: rowID})`; with no `Session`, unless
  `KeepOtherSessions`, every session of the user ends.

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
follows that. With auth on, config validation (and `auth.NewService`)
requires `GOMBIT_JWT_REFRESH_TTL` to be at least `GOMBIT_JWT_ACCESS_TTL`, so
a token issued now never outlives its session. The gate still checks the
session's expiry, as a defense for a token issued before that rule or whose
session's expiry changed: a token never authenticates a session that has
left the list and can no longer be revoked.

What is serialized: token rotation (`POST /auth/refresh`), revoking one
session, revoking the others, and revoking all of a user's sessions each lock
the user's row first, so they run one at a time per user. The refresh-token
reuse cascade, which ends all of a user's sessions when an already-rotated
token is presented again before it expires, runs inside the rotation and takes
the same lock.
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
- **A revocation from a session that has ended** answers 401 and revokes
  nothing. The request's own session is checked again once the revocation
  holds the lock, so a session that another request revoked, or that
  expired, while its request waited for the lock cannot go on to revoke
  anything.
- **Logout** (`POST /auth/logout`, `RevokeRefresh`) is **not** serialized: it
  does not take the lock and revokes only the refresh token it is given. A
  logout that races a refresh of the same token can therefore leave the
  refresh's new session alive. That is a known limitation; to be sure a user
  is signed out everywhere, revoke all their sessions.
- **Login** checks the password without the lock (bcrypt is slow), then
  starts its session under it, and only if the password hash it checked is
  still the user's (`Service.Login`). Every session start makes that check:
  `Service.IssueTokens(ctx, user)` compares `user.PasswordHash`, the
  snapshot the caller authenticated or loaded, with the stored one, so an
  app's own `Authenticate` + `IssueTokens` login is as safe as `Login`. A login in flight when a password
  reset commits is therefore either revoked by the reset or refused (401):
  the old password never outlives the reset. Against a plain revocation
  (`RevokeAllSessions`, `DELETE /auth/sessions?scope=all`), which does not
  change the password, a login that completes after it starts a new
  session: revoking all sessions ends the ones that exist at that moment.

The `auth.Service` methods `ListSessions`, `RevokeSession`, and
`RevokeAllSessions` are exported for application code (for example, an admin
action that signs a user out everywhere). They do not check the caller's own
session; the routes check the request's, as above. Revoking "the others" is
not exported: it needs the request's current session, which only the HTTP
handlers know. Revoked and expired refresh rows are kept; nothing prunes them yet.

`User.IsSuperuser` bypasses all permission checks.
`gombit createsuperuser` is the only built-in path that sets it;
`/auth/register` never does. Regular users receive `Permission` keys
directly or through `Group` membership. See [admin.md](admin.md) for the
ADMIN-3 assignment helpers and admin enforcement rules.

## Audit trail

The framework records every security-sensitive operation in
`auth_audit_events` (`auth.AuditEvent`), one row each:

| Action | When |
| --- | --- |
| `auth.login.succeeded` / `auth.login.failed` | A sign-in. A failure names the account when the email is one (a wrong password, `details.reason` `wrong_password`), and no account when it is not: the attempted address is not recorded. |
| `auth.logout` | `POST /auth/logout` ended a session. |
| `auth.user.registered` / `auth.superuser.created` | `POST /auth/register`; `gombit createsuperuser`. |
| `auth.refresh.reuse_detected` | A rotated-away refresh token came back: every session of the user was revoked. |
| `auth.session.revoked` | `DELETE /auth/sessions…` (`details.scope`: `one`, `others` or `all`). |
| `auth.email.verified` | `POST /auth/verify-email`. |
| `auth.password.reset_requested` / `auth.password.reset` | A reset message sent to an account; a password set with its token (`details.sessions_revoked`). |
| `auth.password.changed` / `auth.password.change_failed` | A signed-in user's password change (`details.other_sessions_revoked`); one refused for a wrong current password. |
| `auth.group.member_added` / `auth.permission.granted` | `auth.AddUserToGroup`, `GrantPermissionToGroup`, `GrantPermissionToUser`, when they changed something: granting what is already granted returns nil and records nothing. |
| `admin.record.created` / `updated` / `deleted` | A write through the admin data plane (`target_type` the model's slug, `target_id` the record's key). Recorded where the write commits: a write that fails records nothing, and one that committed is recorded even if its response then fails. |

Each row has the **actor** (`actor_user_id`: the request's user, or the
user signing in), the **target** (`target_user_id`, or `target_type` /
`target_id`), `occurred_at`, and the **request**: `request_id` (the
`X-Request-Id` the logs carry), `ip` (the client address, trusted proxies
respected) and `user_agent`. `details` is a JSON object of names and ids.
The events the framework records never hold a password, a token, or a hash
of either; an application recording its own (`auth.RecordAuditEvent`,
`Service.Audit`) is responsible for what it puts in them. Every event must
have an `action`, and `details` that are a JSON object (`{}` when empty):
one that does not is refused before any Auditor sees it.

Identifiers are stored **whole**: `target_type` and `target_id` have no
length bound (an admin slug and a string primary key have none either), and
`request_id` holds up to `contract.MaxRequestIDLength` (128) bytes, the
bound the framework enforces on an inbound `X-Request-Id`
([router.md](router.md)). An event whose `action`, `request_id` or `ip`
does not fit its column is refused, not cut, since a cut identifier would
name something else. Only
`user_agent`, free text, is cut (to 255 bytes).

Recording is best effort: an event that cannot be written is logged as a
warning, and the operation it records still succeeds. The Service's events
and the admin's are logged through the Service's logger; the group and
permission helpers, which take a database and no Service, log through that
database's logger. The write is not cancelled by the request (the
operation committed; a client that hung up still gets its record), but has
a deadline of its own (5s), so an audit insert stalled in the database
delays the answer by at most that, never holds it. An Auditor gets that
bounded context, and must honor it.

Inside a caller's transaction (a grant helper given a `tx`) the event is
written under an explicit savepoint, so a failed insert (the table not
migrated yet) rolls back to it and the transaction goes on, even with
GORM's `DisableNestedTransaction` set. The same 5s is one absolute budget
for the savepoint, the insert and the rollback: the database's own limit
(`statement_timeout` on PostgreSQL, the lock wait timeouts on MySQL), set
from what is left of it, ends a lock wait first, and the insert rolls back
to its savepoint. A stall the database cannot end (an unresponsive
connection) meets the client-side deadline instead; cancelling a statement
in flight drops its connection, so the caller's transaction is lost then,
and the helper returns that error rather than call the audit failure
harmless.

- **Reading it.** `auth.ListAuditEvents(ctx, db, auth.AuditQuery{TargetUserID: id})`
  returns events newest first by `occurred_at` (when the operation ran,
  stored in UTC; the row id breaks ties). Filters: actor, target, action;
  100 by default, at most 1000. To browse it in the admin, register
  `auth.AuditEvent` and grant staff only its view permission
  (`admin.<slug>.view`, see [admin.md](admin.md)); a superuser can still
  edit any model.
- **Sending it elsewhere.** `framework.WithAuthAuditor(a)` replaces where the
  auth routes and the admin record (`auth.Auditor`:
  `Record(ctx, auth.AuditEvent) error`), for a SIEM or a log pipeline; the
  event arrives complete. The group and permission helpers take a database,
  not a Service, and always write to its `auth_audit_events`.
- **Your own operations.** `auth.RecordAuditEvent(ctx, db, event)` records an
  app's own sensitive operation, with the actor and request taken from the
  context; put `auth.CaptureRequestInfo()` first in the operation's
  middlewares so the IP and user agent are known. `Service.Audit` does the
  same through the Service's Auditor.
- **Retention.** Nothing prunes the table; delete old rows on your own
  schedule.

## Config

| Variable | Field | Default |
| --- | --- | --- |
| `GOMBIT_JWT_SECRET` | `Config.Auth.JWTSecret` | empty (auth unmounted) |
| `GOMBIT_JWT_ACCESS_TTL` | `Config.Auth.AccessTokenTTL` | `15m` |
| `GOMBIT_JWT_REFRESH_TTL` | `Config.Auth.RefreshTokenTTL` | `168h` (7 days) |
| `GOMBIT_AUTH_EMAIL_VERIFICATION_TTL` | `Config.Auth.EmailVerificationTTL` | `24h` |
| `GOMBIT_AUTH_EMAIL_VERIFICATION_URL` | `Config.Auth.EmailVerificationURL` | empty (the message carries the bare token); must contain `{token}` |

With auth on, `config.Load` / `Validate` refuse a `GOMBIT_JWT_REFRESH_TTL`
shorter than `GOMBIT_JWT_ACCESS_TTL` (equal is fine): the refresh token's
lifetime is the session's, and an access token must not outlive its session
([Sessions](#sessions)).

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
