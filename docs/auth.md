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
| Access JWT | **Memory only** | `Authorization: Bearer <token>` (the scheme in any case, as RFC 9110 has it). Never `localStorage` / `sessionStorage`. Lost on refresh, which is intended. |
| Refresh token | **Memory only** (JSON body) | Returned once on login/refresh. Rotated on each successful refresh. Not a cookie in v0.1. |

The refresh token stays in the JSON body (not a cookie) so this mode's
transport does not mix with `--auth cookie`'s HttpOnly session cookies
(see [`docs/auth-cookie.md`](auth-cookie.md)). Do not put tokens in
`VITE_*`.

## Endpoints

All paths use `config.API.Prefix` (default `/api/v1`). D10 envelopes. The
credential endpoints are throttled (429 with `Retry-After`); see
[Abuse protection](#abuse-protection).

| Method | Path | Auth |
| --- | --- | --- |
| `POST` | `/auth/register` | Public. Demo/bootstrap seed path (email + password); never sets `IsSuperuser`. |
| `POST` | `/auth/login` | Public. Returns `access_token`, `refresh_token`, `token_type`, `expires_in`. |
| `POST` | `/auth/refresh` | Public. Body `{ "refresh_token" }`. Issues a new pair; the old refresh token is invalid. |
| `POST` | `/auth/logout` | Public. Body `{ "refresh_token" }`. Ends that token's session: the token, or, when it has already rotated (a refresh in flight as the user signs out), the session's current token. Any unexpired token the session has had ends it, however many rotations behind its current token, so an old copy is enough: the session's id leads to its current token. Bound access JWTs then fail. A token unknown, past its expiry, or of a session already ended ends nothing and answers 200 alike, so logout is idempotent and its answer reveals nothing about a token (an expired token is no credential, as for refresh). A token whose session's current token cannot be found (a restore that lost it) answers 500 and ends nothing, every time, and so does a token 1000 or more rotations behind along rows without a session id (issued before the upgrade that added session ids, or by an older binary during a rolling deploy: followed along the rotation chain instead), or whose chain is broken; revoke that session instead (`DELETE /auth/sessions/{id}` or `?scope=all`). |
| `GET` | `/me` | Bearer access JWT. Example protected route for E2E. Missing/invalid Bearer is 401 with `WWW-Authenticate: Bearer realm="api"`; a database failure while the gate loads the token's user or session is a 500 (`internal`), not a 401: the session is still valid, and a client that keeps its tokens on a 5xx stays signed in. `auth.ErrInvalidCredentials` / `auth.ErrInvalidAccessToken` are the 401 cases of `Authenticate` / `ParseAccess`; any other error is a server failure. |
| `GET` | `/auth/sessions` | Bearer access JWT. Lists the user's active sessions; see [Sessions](#sessions). |
| `DELETE` | `/auth/sessions/{id}` | Bearer access JWT. Revokes one session; 404 if `id` names no active session of the user. |
| `DELETE` | `/auth/sessions?scope=others\|all` | Bearer access JWT. `others` revokes every session except the current one; `all` revokes every session, the current one included. |
| `POST` | `/auth/verify-email` | Public. Body `{ "token" }` from the verification message; marks the email verified. See [Email verification](#email-verification). |
| `POST` | `/auth/verify-email/resend` | Bearer access JWT. Mails a new verification link (at most one a minute). |
| `POST` | `/auth/password/forgot` | Public. Body `{ "email" }`. Always 202 `{"data":{"accepted":true}}`; mails a reset link when the email is an account. See [Password reset](#password-reset). |
| `POST` | `/auth/password/reset` | Public. Body `{ "token", "password" }`. Sets the new password and revokes every session of the user. |
| `POST` | `/auth/password/change` | Bearer access JWT. Body `{ "current_password", "new_password", "keep_other_sessions"? }`. See [Password change](#password-change). |
| `POST` | `/auth/email/change` | Bearer access JWT. Body `{ "current_password", "new_email" }`. Mails a confirmation link to the new address. See [Email change](#email-change). |
| `POST` | `/auth/email/change/confirm` | Public. Body `{ "token" }` from that link; makes the new address the account's email, verified. |
| `POST` | `/auth/users/{id}/disable` | Bearer access JWT with the `auth.users.manage` permission. Disables the account and revokes its sessions; see [Account state](#account-state). |
| `POST` | `/auth/users/{id}/enable` | Bearer access JWT with the `auth.users.manage` permission. Lets a disabled account sign in again. |
| `GET` | `/auth/mfa` | Bearer access JWT. The user's multi-factor state; see [Multi-factor authentication](#multi-factor-authentication). |
| `POST` | `/auth/mfa/totp` | Bearer access JWT. Body `{ "current_password" }`. Begins a TOTP enrollment: the secret and `otpauth://` URI. |
| `POST` | `/auth/mfa/totp/confirm` | Bearer access JWT. Body `{ "mfa_code" }`. Enables MFA, ends the user's other sessions; returns the recovery codes, once. |
| `POST` | `/auth/mfa/recovery-codes` | Bearer access JWT. Body `{ "current_password", "mfa_code" }`. Replaces the recovery codes. |
| `POST` | `/auth/mfa/disable` | Bearer access JWT. Body `{ "current_password", "mfa_code" }`. Turns MFA off. |
| `POST` | `/auth/users/{id}/mfa/reset` | Bearer access JWT with the `auth.users.manage` permission. Removes a user's MFA. |

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
winner. Neither does a refresh with a token rotated less than
`RefreshReuseGrace` ago (`config.AuthConfig`, 30 seconds by default) whose
successor is still live: the second tab's request was on the wire when the
first tab's rotation committed, a lost race rather than theft. It is a plain
401 that issues no tokens and revokes nothing, so the winner's new session
stays signed in, and it is audited (`auth.refresh.reuse_within_grace`). A
successor already rotated again is reuse, and one logged out leaves a plain
401. The cascade runs for a rotated token presented after that grace and
before it expires. The cost: if a thief rotates a stolen token first and the
legitimate client's refresh lands within the grace, that client gets the 401
and the thief's session is not revoked; the `auth.refresh.reuse_within_grace`
event, naming the thief's session by its id (the one `GET /auth/sessions`
lists, and `DELETE /auth/sessions/{id}` ends), is the trace it leaves, and
past the grace the cascade catches it as before. An app with no clients sharing a refresh
token across tabs can set `GOMBIT_AUTH_REFRESH_REUSE_GRACE=0` for the strict
rule.

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
  works. A reset that revokes the sessions also removes the user's linked
  external identities and voids their external sign-in codes not yet
  exchanged ([External identity providers](#external-identity-providers)).
  `GOMBIT_AUTH_PASSWORD_RESET_KEEP_SESSIONS=true`
  keeps them all. In cookie mode a reset that revoked the sessions also clears
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
  it. So do email change links, asked with the old password.
- **Races.** The change is written under the user's row lock, and only if
  the stored password is still the one checked: a reset or another change
  that commits in between wins, and this one is a 409 (try again with the
  current password). A login that checked the old password just before the
  change cannot start a session after it ([Sessions](#sessions)).
- **Audit.** `auth.password.changed` (`details.other_sessions_revoked`). No
  log line or audit row holds a password.
- **From Go.** `svc.ChangePassword(ctx, userID, current, next,
  auth.PasswordChange{})` is the same flow for an app's own operation. The
  requesting session is the one `ctx` carries: an operation behind the auth
  gate (`svc.RequireBearer()`, `RequireCookieSession()`) runs with the
  signed-in user's session in its context, and when that user is `userID`
  it stays signed in. With none (a command, another user's request),
  unless `KeepOtherSessions`, every session of the user ends. Its errors
  are for `errors.Is` / `errors.As`:

  ```go
  revoked, err := svc.ChangePassword(ctx, user.ID, current, next, auth.PasswordChange{})
  var policy auth.PasswordPolicyError
  switch {
  case errors.Is(err, auth.ErrWrongCurrentPassword): // 422 on the current password
  case errors.As(err, &policy): // policy.Field, policy.Err: ErrPasswordTooShort / TooLong / Required
  case errors.Is(err, auth.ErrPasswordChanged): // 409: a reset or another change won; retry
  case errors.Is(err, auth.ErrSessionEnded): // the request's session ended meanwhile
  case errors.Is(err, auth.ErrUserNotFound):
  case err != nil: // the database
  }
  ```

  None of those errors changes anything.

## Email change

A signed-in user asks for a new address with their current password; it
becomes the account's email only once the link mailed to it is confirmed:

```sh
curl -sX POST localhost:8080/api/v1/auth/email/change \
  -H "Authorization: Bearer $ACCESS" -H 'Content-Type: application/json' \
  -d '{"current_password":"correct-horse","new_email":"ada@new.example.com"}'
# 202 {"data":{"sent":true}}
curl -sX POST localhost:8080/api/v1/auth/email/change/confirm \
  -H 'Content-Type: application/json' -d '{"token":"<token from the message>"}'
# {"data":{"id":1,"email":"ada@new.example.com","email_verified":true}}
```

- **The new address is not trusted before it is proven.** Asking changes
  nothing: the account's email, its sign-in, and every link bound to it
  stay the old address until the token mailed to the new one is posted.
  Then the new address is the account's email, and verified (the token
  proved it).
- **Asking** requires the current password (a wrong one is a 422 on
  `current_password` and an `auth.email.change_failed` audit event); a new
  address that is no address, or the account's own, is a 422 on
  `new_email`; one another account has is a 409 (as registration answers).
  At most one message a minute (429 with `Retry-After`); a newer request
  supersedes the earlier link. The address is checked by the service as
  the route's schema checks it (a bare address of at most 255 characters),
  so a Go caller gets the same answer. A request racing another change of
  the account's email, or of its password (a reset, a password change,
  committed while it ran), is a 409: the old password authorizes nothing
  after a reset; ask again.
- **A reset or a password change** voids the outstanding email change
  links, the pending ones too: a link asked with the old password, by
  whoever knew it, dies with it, so recovering the account also stops a
  change it had asked for.
- **Confirming** is public (the link may open signed out). The token works
  once, expires as a verification link does
  (`GOMBIT_AUTH_EMAIL_VERIFICATION_TTL`), and only while the account's
  email is still the one the change was asked from: once it is another
  (another change, an admin), the link is a 422 on `token`. An address
  another account took between the request and the confirmation is a 409,
  and nothing changes: the unique email decides who has it, so racing
  changes cannot hijack an account.
- **The old address** gets a security notification once the change is
  confirmed (`msg.Kind` `email_changed`, through the app's `auth.Mailer`),
  naming no new address. It says that the old address no longer signs in
  to the account and cannot recover it (a password reset looks the account
  up by its current email), and points to the site's support: there is no
  revert link. An app's `auth.Mailer` can replace the text by `msg.Kind`,
  with its own support contact. It is sent in the background, best effort: a
  failure is logged and does not undo the change, and a slow mailer does
  not hold up the confirmation (when the background work is busy, it is
  sent with the confirmation instead).
- **Every other link stops for good.** Confirming voids every unused
  one-time link of the user, whatever its purpose (verification, password
  reset, other email changes), pending ones included. None comes back if
  the email is later changed back to the old address.
- **The link.** `GOMBIT_AUTH_EMAIL_CHANGE_URL` is the page the confirmation
  links to, with `{token}` where the token goes. Unset, the message carries
  the bare token. `msg.Kind` is `change_email`.
- **Audit.** `auth.email.change_requested`, `auth.email.changed`, and
  `auth.email.change_failed`; no address is recorded.
- **From Go.** `svc.RequestEmailChange(ctx, userID, current, newEmail)` and
  `svc.ConfirmEmailChange(ctx, token)` are the same flow. Errors:
  `auth.ErrInvalidEmail`, `ErrEmailUnchanged`, `ErrWrongCurrentPassword`,
  `ErrEmailTaken`, `ErrEmailChangedMeanwhile`, `ErrInvalidToken`,
  `ErrUserNotFound`.

## Account state

An account is **active** or **disabled** (`User.DisabledAt`; `User.Disabled()`). A
disabled user cannot sign in, and nothing issued to it works:

```sh
curl -sX POST localhost:8080/api/v1/auth/users/42/disable -H "Authorization: Bearer $ADMIN_ACCESS"
# {"data":{"id":42,"disabled":true}}
curl -sX POST localhost:8080/api/v1/auth/users/42/enable -H "Authorization: Bearer $ADMIN_ACCESS"
# {"data":{"id":42,"disabled":false}}
```

- **Disabling** revokes every session of the user and voids every one-time
  link (verification, password reset, email change), pending ones too, in
  one transaction under the user's row lock. Access tokens are bound to
  their session, so they fail at once (401), and so do refresh tokens and
  cookie sessions. A login, a refresh or a link racing the disable either
  finished before it, or is refused after it.
- **Every path refuses it** afterwards, not only the revoked rows: a login
  with the right password is a **403** (`authorization`, "account is
  disabled"), told only to whoever knows the password, since a wrong one is
  the usual 401; a session start, a refresh, an access token, a one-time
  link (422 on `token`), a password change, a verification message.
  `POST /auth/password/forgot` answers as always and mails nothing.
- **Enabling** lets the user sign in again; the sessions and links revoked
  when the account was disabled stay revoked.
- **Who may.** Both routes need the `auth.users.manage` permission
  (`auth.PermissionManageUsers`; a superuser has every permission). Create
  it once and grant it to the users or groups that manage accounts:

  ```go
  perm, err := auth.EnsurePermission(ctx, db, auth.PermissionManageUsers, "Disable and enable accounts")
  err = auth.GrantPermissionToGroup(ctx, db, &support, &perm)
  ```

  A superuser's account is changed by a superuser only (403 otherwise), and
  no one disables their own account (409), both decided on the target's row
  under its lock. An unknown id is a 404. Both are idempotent. Nothing keeps
  the last superuser active: two superusers who disable each other at once
  both succeed. Recover with `gombit createsuperuser` (another address) or
  `svc.EnableUser` from code.
- **Permissions.** `auth.HasPermission` / `HasPermissions` grant a disabled
  account nothing, a superuser's included, and decide on the user's row,
  reloaded: app code that authorizes a user it loaded earlier (a job, a
  webhook) does not trust a snapshot taken before the account was disabled
  or demoted.
- **From Go.** `svc.DisableUser(ctx, userID)` / `svc.EnableUser(ctx, userID)`
  do the same and check no permission: for an app's own code and commands.
  `auth.ErrAccountDisabled` is the error of a disabled account's login,
  password change and messages.
- **Audit.** `auth.user.disabled` and `auth.user.enabled`, with the
  operator as actor; a disabled account's login with the right password is
  `auth.login.failed` (`account_disabled`).

## Multi-factor authentication

A user may enroll a TOTP authenticator (RFC 6238: any authenticator app;
SHA-1, 30-second steps, 6 digits). From then on a login needs, besides the
password, a code from it, or one of the account's recovery codes:

```sh
curl -sX POST localhost:8080/api/v1/auth/mfa/totp -H "Authorization: Bearer $ACCESS" \
  -H 'Content-Type: application/json' -d '{"current_password":"correct-horse"}'
# {"data":{"secret":"JBSWY3DP...","otpauth_uri":"otpauth://totp/Gombit:ada@example.com?..."}}
curl -sX POST localhost:8080/api/v1/auth/mfa/totp/confirm -H "Authorization: Bearer $ACCESS" \
  -H 'Content-Type: application/json' -d '{"mfa_code":"492039"}'
# {"data":{"recovery_codes":["k7mpq-2xhvr", ...]}}

curl -sX POST localhost:8080/api/v1/auth/login -H 'Content-Type: application/json' \
  -d '{"email":"ada@example.com","password":"correct-horse"}'
# 422 {"error":{"code":"validation_error","fields":{"mfa_code":["a multi-factor code is required"]},...}}
curl -sX POST localhost:8080/api/v1/auth/login -H 'Content-Type: application/json' \
  -d '{"email":"ada@example.com","password":"correct-horse","mfa_code":"118204"}'
# {"data":{"access_token":"...","refresh_token":"...",...}}
```

- **Enrolling** takes the current password and returns the secret (to type
  in) and an `otpauth://` URI (to show as a QR code); nothing changes for
  the login until `POST /auth/mfa/totp/confirm` gets a code from the
  authenticator (`mfa_code`, the field every route of the second factor
  uses). A new enrollment replaces one pending; while MFA is on, enrolling
  is a 409 (turn it off first).
- **Enabling ends the other sessions.** The sessions started on the
  password alone end when the enrollment is confirmed (the confirming one
  stays; from Go with no session, all end), under the user's lock: refresh
  would otherwise keep a stolen one alive for good, and enrolling is what a
  user does who fears one was. A login in flight that found no factor
  before the confirmation is refused at its session start (it re-checks
  under the same lock): `mfa_code` required.
- **The login challenge.** `POST /auth/login` (both modes) takes an
  optional `mfa_code`. For an account with MFA, a login without it is a
  **422 on `mfa_code`** ("a multi-factor code is required"), said only
  after the right password, and a wrong code is a 422 ("invalid
  multi-factor code"): the client asks for the code and logs in again with
  the email, the password and the code. Nothing is kept between the two
  requests. For an account without MFA, `mfa_code` is ignored. The
  framework's login pages (the admin's and a generated app's) show a code
  field when the 422 asks for it and send the login again with it.
- **A code works once.** A TOTP code is accepted for its step and the one
  either side (clock drift), and never twice: once a step's code is used,
  no code of that step or an earlier one is accepted again, even from two
  requests at once. A recovery code works once.
- **Recovery codes.** Ten, shown once when MFA is enabled (and when
  replaced with `POST /auth/mfa/recovery-codes`, which takes the current
  password and a code, since new codes are a second factor: the old ones
  stop working). Each signs in once in place of an
  authenticator code, typed with or without the dash, in any case. Using
  one is audited; `GET /auth/mfa` says how many remain.
- **Turning it off** (`POST /auth/mfa/disable`) takes the current password
  *and* a code (authenticator or recovery): a stolen session and password
  alone do not remove the second factor.
- **Reset.** `POST /auth/users/{id}/mfa/reset`, with the `auth.users.manage`
  permission (a superuser's account by a superuser only; an account turns
  its own off with `/auth/mfa/disable`, 409 here), removes a user's MFA,
  for one who lost the authenticator and the codes, and ends every session
  of the user (the lost device may hold one). `svc.ResetMFA` is the same,
  with no permission check.
- **Brute force.** Wrong codes are failures of the login throttles (per
  address and email, per address, per email), and of `MFAPerUser`: 5 wrong
  codes per account in 15 minutes, from any address (enrollment
  confirmations and disables count too, and so does a replayed code, such
  as a login submitted twice). `MFAPerUser` stays on with
  `GOMBIT_AUTH_THROTTLE_DISABLED=true`: it is what bounds guessing a
  six-digit code, which a gateway's per-address limit does not.
- **Keys.** The TOTP secret is stored encrypted (AES-256-GCM, bound to the
  user), and the recovery codes as HMAC-SHA256 hashes, under keys derived
  from `GOMBIT_AUTH_MFA_KEY` (at least 32 characters), or from
  `GOMBIT_JWT_SECRET` when it is unset. Rotating that key makes every
  enrolled authenticator and recovery code unusable: those accounts' logins
  answer 500, with an authenticator code and a recovery code alike, and a
  warning naming the key change is logged (at most once a minute), until
  their MFA is reset and they enroll again. A server error is never charged
  to `MFAPerUser`: only a wrong code is. Set
  `GOMBIT_AUTH_MFA_KEY` to rotate the JWT secret on its own; setting it on
  an app whose users already enrolled is such a rotation, once.
- **From Go.** `svc.Authenticate` / `Login` refuse an account with MFA
  without a code (`auth.ErrMFARequired`): an app's own login cannot skip
  the second factor by calling them. `AuthenticateMFA` / `LoginMFA` take the
  code (`auth.ErrInvalidMFACode` for a wrong one). `BeginTOTPEnrollment`,
  `ConfirmTOTPEnrollment`, `RegenerateRecoveryCodes`, `DisableMFA`,
  `ResetMFA` and `MFAStatusOf` are the routes' operations. Other sensitive
  operations (a password or email change) ask for the password, not a code.
- **Audit.** `auth.mfa.enabled`, `auth.mfa.disabled`, `auth.mfa.reset`,
  `auth.mfa.recovery_codes_regenerated`, `auth.mfa.recovery_code_used`,
  `auth.mfa.failed` (a wrong password or code in an MFA operation), and
  `auth.login.failed` with `wrong_mfa_code`. No secret or code is recorded.

## External identity providers

Users can sign in with an account at an external identity provider (Google,
Microsoft Entra ID, Okta, Auth0, Keycloak, GitHub, ...). Core auth ships no
provider. A provider is an **adapter** behind `auth.IdentityProvider`, which
has two jobs: build the provider's authorization URL, and redeem the code the
provider sends back for an `auth.ExternalIdentity` (a stable subject, an
email, whether the provider verified it). **The framework does everything
else**: the state, the nonce and the PKCE pair, the mapping of identities
to local users, signup, linking, and the session.

The optional `auth/oidc` package is an adapter for any OpenID Connect
provider. It reads the provider's discovery document, runs the authorization
code flow with PKCE, and verifies the ID token (signature, issuer, audience,
expiry, nonce):

```go
google, err := oidc.New(ctx, oidc.Config{
	Name:         "google",
	Issuer:       "https://accounts.google.com",
	ClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
	ClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
	// Registered with the provider as the redirect URI.
	RedirectURL: "https://app.example.com/api/v1/auth/external/google/callback",
})
if err != nil {
	return err
}
app, err := framework.New(framework.WithAuthIdentityProviders(google))
```

A plain OAuth2 provider (no ID token) needs an adapter of the app's own. It
puts the request's state and PKCE challenge in the URL, sends the verifier
with the token request, reads the user from the provider's API, and leaves
`ExternalIdentity.Nonce` empty. An OIDC adapter must check the ID token's
nonce against `ExchangeRequest.Nonce`, return it, and declare itself an
`auth.IDTokenProvider` (`IssuesIDTokens() bool`, as `auth/oidc` does). The
framework then refuses an identity whose nonce is missing or not the
flow's. From an adapter that does not declare ID tokens, the framework
refuses only a nonce that is returned and wrong: it cannot tell a plain
OAuth2 adapter from an OIDC one that forgot the nonce.

With at least one provider, both auth modes mount these routes. Without
one, nothing is mounted and the OpenAPI document does not list them.

| Route | |
| --- | --- |
| `GET {prefix}/auth/external` | The providers' names, for a login page |
| `POST {prefix}/auth/external/{provider}/login` | Start a sign-in: `{"return_to":"/home"}` → `{"authorization_url": ...}` |
| `POST {prefix}/auth/external/{provider}/link` | Signed in: start linking an identity to the account, with `current_password` |
| `GET {prefix}/auth/external/{provider}/callback` | The redirect URI: the provider sends the browser back here |
| `POST {prefix}/auth/external/exchange` | Bearer mode: trade the callback's `gombit_code` for tokens |
| `GET {prefix}/auth/external/identities` | Signed in: the account's linked identities |
| `DELETE {prefix}/auth/external/identities/{id}` | Signed in: remove one (204) |

**The flow.** The client POSTs `/login` (or `/link`) and navigates the
browser to the `authorization_url` it gets back. The response also sets
`gombit_external`, the flow's state cookie:
- HttpOnly and AES-GCM-encrypted under a key derived from the JWT secret;
- scoped to `{prefix}/auth/external`;
- valid for ten minutes;
- `SameSite=Lax` whatever `GOMBIT_COOKIE_SAMESITE` says, because the
  provider's redirect back is a cross-site navigation;
- `Secure` with `GOMBIT_COOKIE_SECURE`, and always in production, in
  either auth mode.

It holds the state, the nonce, the PKCE verifier and `return_to`; one flow
at a time per browser. The callback checks the returned state against the
cookie and clears it. A missing, expired or foreign state is a **422 on
`state`**: there is no flow to send the browser back to. Otherwise the
callback has the adapter redeem the code, then **redirects (302) to
`return_to`**:
- **cookie mode**: a sign-in sets the session cookies;
- **Bearer mode**: a sign-in adds `?gombit_code=...`, and sets
  `gombit_external_code`: an HttpOnly cookie for the exchange's path only,
  sealed like the state cookie, which binds the code to this browser. The
  page trades the code with `POST /auth/external/exchange {"code": ...}`
  (the browser sends the cookie) for the token pair `/auth/login`
  returns. The code works once, within two minutes, and **only from the
  browser that ran the flow**. A code in a URL or a log, or one an
  attacker hands a victim's page to sign it into the attacker's account
  (login CSRF), exchanges nothing anywhere else: a 422 on `code`. Ending
  the account's sessions voids the codes not yet exchanged (each a
  session about to start): signing out everywhere or the other sessions,
  a password reset or change, a disable, enabling MFA. So a code whose
  account was disabled or given MFA since the callback is a 422 on
  `code`, like a used one. The session starts under the user's lock with
  the factor re-checked, so the only 409 is the narrow race of MFA
  enabled between the code's use and that start;
- **a link** adds `?gombit_linked={provider}`.

A refusal adds `?gombit_error=` with one of these codes:
- `access_denied`: the user declined at the provider;
- `provider_error`: the provider refused the code or could not be reached
  (logged);
- `invalid_identity`: no subject, or a foreign nonce;
- `account_exists`, `email_unverified`, `signup_disabled`;
- `mfa_enabled`, `account_disabled`;
- `identity_linked_elsewhere`, `session_ended`, `login_failed`;
- `server_error`.

`return_to` must be a path on this site: no scheme or host, no `//`, no
backslash, no fragment, and a path and query the callback can parse (it
adds its parameter to them). The default is `/`.

The provider must reach the callback on the browser's origin. If the SPA is
served from another origin than the API (the Vite dev server), register the
URL the browser uses (Vite proxies `/api`). In Bearer mode the SPA's
`fetch` of `/login` and of `/exchange` must carry credentials (the default
for a same-origin `fetch`; `credentials: "include"` across origins), so
that the browser keeps the state cookie and sends the code's binding.

**Identities and accounts.** An identity is (provider, subject):
`auth.IdentityLink` (`auth_identity_links`) maps it to one user, and a user
may link several. Subjects compare exactly, byte for byte: the link is keyed
by the subject's SHA-256, so no database collation (MySQL's default ignores
case and trailing spaces) makes two users' subjects one. On a sign-in:

- **A linked identity** signs its user in. The link's email is refreshed
  for display, and decides nothing: a provider's email can change hands,
  its subject cannot.
- **An identity no user linked makes an account**, its email verified, but
  only when the provider *verified* the email (`email_unverified`
  otherwise, and for no email). Otherwise anyone could register at a lax
  provider with someone else's address and pre-claim the account it would
  make.
- **An email that is already an account's is refused**
  (`account_exists`). The framework never links by email, since that
  would let any provider vouching for an address take over the account
  that has it. The account's owner signs in with their password and links
  the identity from the account (`/link`).
- `framework.WithoutAuthExternalSignup()` (`auth.WithoutExternalSignup`)
  makes no accounts. An unlinked identity is `signup_disabled`, and users
  link identities from their accounts only.

An account an external sign-in made has **no password**: no password signs
in to it (`User.HasUsablePassword` is false). A password reset gives it
one. Until then it cannot do what asks for the current password: a
password or email change, enrolling MFA, linking another identity.

**Linking** takes the current password (throttled, wrong ones audited, as
a password change). A link signs in for good, past every session, so a
session alone does not add one: otherwise a stolen access token would
become a credential that survives every revocation. An account without a
password links nothing (409) until a password reset gives it one. The
callback links the identity only while the session that asked is still
active, the account enabled and without MFA. An identity linked to another
account is `identity_linked_elsewhere`. **Unlinking** an account's only
link is a 409 when the account has no password, since nothing could sign
in to it again.

**A password reset removes the links** when it ends the sessions (the
default; not with `GOMBIT_AUTH_PASSWORD_RESET_KEEP_SESSIONS`): a reset is
the recovery from a compromise, and an identity an intruder linked would
otherwise outlive it. The user links their identities again, with the new
password. Signing out everywhere ends sessions only: links are listed at
`/identities` and removed there.

**Refusals that hold at the session too.** A disabled account does not sign
in (`account_disabled`). Nor does an account with multi-factor
authentication (`mfa_enabled`), which signs in with its password and code.
An external sign-in does not prove the second factor, and the framework
does not take the provider's word for one. Such an account cannot start a
link either (409). Like a password login, the session starts under the
user's lock and re-checks both, so a disable or an enrollment that commits
meanwhile wins.

**From Go.** `svc.IdentityProviders()`, `svc.ListIdentityLinks` and
`svc.UnlinkIdentity` (`auth.ErrLastSignInMethod`,
`auth.ErrIdentityLinkNotFound`). The flow itself runs through the routes,
which own the browser's state.

**Audit.** These events are recorded:
- `auth.login.succeeded` with `method: external` and the provider (in
  Bearer mode, at the exchange: the code's binding names it);
- `auth.login.failed` with the refusal's code as `reason`;
- `auth.user.registered` with the provider (an account the sign-in made);
- `auth.identity.linked`, `auth.identity.link_failed` and
  `auth.identity.unlinked`;
- `auth.password.reset` with `identities_unlinked`, the links it removed.

No code, token, state or nonce is recorded.

## Abuse protection

The credential endpoints bound how often they may be tried, in both auth
modes (AUTH-8). Each rule counts one subject's attempts in a sliding window;
over its limit the endpoint answers **429** (`rate_limited`) with
`Retry-After` (seconds), before it does any of the work (the lookup,
bcrypt, the token's transaction):

| Rule (`auth.ThrottlePolicy` field) | Counts | Default |
| --- | --- | --- |
| `LoginPerAccountAddress` | failed logins for one email from one client address | 5 / 15 min |
| `LoginPerAddress` | failed logins from one client address, any email (credential stuffing) | 100 / 15 min |
| `LoginPerAccount` | failed logins for one email, any address (guessing spread over addresses) | 100 / hour |
| `RegisterPerAddress` | `POST /auth/register` from one client address, every request (each hashes a password and mails a verification link) | 20 / hour |
| `PasswordResetPerAddress` | `POST /auth/password/forgot` from one client address, any email | 20 / 15 min |
| `TokenPerAddress` | invalid tokens posted to `/auth/verify-email`, `/auth/password/reset`, `/auth/email/change/confirm` from one client address | 30 / 15 min |
| `CurrentPasswordPerUser` | wrong current passwords of one user (`/auth/password/change`, `/auth/email/change`) | 5 / 15 min |
| `VerificationPerUser` | verification messages sent to one user (on top of the one-a-minute resend wait) | 10 / hour |

- **Nothing to enumerate.** A login is counted by the email as typed
  (normalized), whether or not it is an account, and a reset request by
  the client address alone: a 429 says nothing about which addresses have
  accounts.
- **One email, one identity.** Only the account's own email signs in or
  asks for a reset, exactly as stored: on a database whose collation folds
  more (MySQL's default ignores zero-width characters and accents, and
  folds fullwidth letters), a spelling it folds is not the account, so it
  cannot be a fresh allowance of guesses against it.
- **No bans.** No rule shuts an address out: the per-address limits are
  wide, for the users behind one NAT or proxy, and count failures only; the
  tight one is per address *and* email. A successful login does not count,
  and clears that address's failures for the email.
- **`LoginPerAccount` can be filled by anyone.** It bounds a guessing that
  rotates addresses, but a handful of addresses failing at their own limit
  (five, with the defaults) keep it full for as long as they go on, and
  every client is then refused for that account. Except a **known client**:
  an address that signed in to the account within `KnownClientTTL` (30
  days, renewed by each login) is not held by it, so the owner signs in
  from where they did before. Only the password makes an address known, so
  this is no oracle. A password reset empties the rule. A new address of the
  owner's waits until the attack stops (and the window or two after it).
- **Concurrent attempts** cannot get past a limit together: an attempt
  takes its place before the work, atomically, and gives it back when it
  did not count (a success, a valid token). So attempts still in flight
  count too: a burst of more concurrent logins from one address than its
  limit can see the last ones refused until the first finish.
- **A refused request writes nothing.** Every rule is read before any is
  counted: a request refused by one allocates no counter for the subjects
  it named (the emails an attacker sprays, the addresses it rotates).
- **The window slides.** The count is the current fixed bucket plus the
  previous one weighted by how much of it the window still covers, so a
  burst at a bucket's edge does not double the limit; `Retry-After` is when
  one more attempt fits, at most two windows.
- **The client address** is gin's `ClientIP`: behind a proxy or load
  balancer, list it in `GOMBIT_HTTP_TRUSTED_PROXIES` so the forwarded
  client is counted, not the proxy (and so that a client cannot pick its
  own address with `X-Forwarded-For` when no proxy is trusted). An IPv6
  client is counted by its /64, one host's usual allocation, so it cannot
  take a fresh address per attempt. A call from Go with no request has no
  address: only the other rules apply.
- **Where the counts live.** Under `framework.App`, in the app's cache
  (`GOMBIT_CACHE_DRIVER`): with `redis` every instance shares them; with
  `memory` each process counts alone. With `noop`, or for an
  `auth.NewService` without `auth.WithThrottleStore`, the Service keeps
  them in a memory cache of its own (`Close` stops it). Counters expire on
  their own (`cache.Counter`), and an attempt's refund only takes back from
  a counter that still exists, so one cleared or evicted meanwhile is never
  recreated; emails and addresses are hashed into the keys, never stored. A cache that fails does not lock anyone out: the
  attempt goes on unthrottled and a warning is logged (at most once a
  minute). A cache that answers but keeps nothing (a noop one, however
  wrapped) is found by a probe the Service runs in the background when it
  starts (a counter written to a random key, read back, deleted; retried a
  minute after one that could not reach the store): it then warns and
  counts in its own memory instead. No request ever waits on the probe.
- **Configuration.** `auth.WithThrottlePolicy(p)` replaces the limits (start
  from `auth.DefaultThrottlePolicy()`; a zero `Limit` turns a rule off).
  `GOMBIT_AUTH_THROTTLE_DISABLED=true` turns them all off, for an app whose
  gateway already limits these routes, but `MFAPerUser`, which bounds
  guessing a multi-factor code from any number of addresses.
- **From Go.** `Register`, `Authenticate` / `Login`, `ResetPassword`, `VerifyEmail`,
  `ConfirmEmailChange`, `ChangePassword`, `RequestEmailChange` and
  `SendEmailVerification` apply the same rules and return a
  `*auth.ThrottleError` (`errors.Is(err, auth.ErrTooManyAttempts)`, with
  `Rule` and `RetryAfter`). A throttled attempt is not written to the audit
  trail (under attack that would be a write per request); a rule that is
  refusing attempts is logged instead, at most once a minute per rule,
  without its subject.

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
      "current": true,
      "user_agent": "Mozilla/5.0 (X11; Linux x86_64; rv:133.0) Gecko/20100101 Firefox/133.0",
      "client_address": "203.0.113.0/24",
      "location": ""
    }
  ]
}
```

- `id` is the session's for its whole life: drawn at random when it signs in
  and kept by every refresh (`refresh_tokens.session_id`), so an id listed
  before a refresh still revokes the session, and the audit trail names one
  session by one id. It says nothing about the refresh token rows, whose ids
  are sequential; the access JWT's `rid` claim is still the row id, readable
  by the token's holder. A session that signed in before the upgrade that
  added the column keeps the id it was listed under then (an HMAC of its
  current refresh row id under the JWT secret): its next refresh stores that
  id. Only a refresh by a binary from before the column (an old instance in
  a rolling deploy, or a rollback that kept the column) writes a row without
  an id, and the session is then listed under that row's HMAC id, which it
  keeps from there on.
- `last_refreshed_at` is when the session last signed in or refreshed. An
  active client refreshes about once per access-token TTL, so that is its
  precision; it is not a per-request "last seen".
- `current` marks the session that made the request, also when that
  session refreshed while the request ran.
- `user_agent`, `client_address` and `location` describe the device the
  session signed in from, for a user to recognize it (AUTH-12). They are
  the sign-in request's and refreshes do not change them, so a session
  that signed in on a laptop at home stays "that laptop at home" wherever
  it refreshes from. Each is empty when unknown: a session started outside
  a request (`Service.IssueTokens` from application code, with a context
  that did not come through the auth routes), or one that signed in before
  the upgrade that added them.
  - `user_agent` is the `User-Agent` header as the client sent it, cut to
    255 bytes, as one line of valid text: invalid UTF-8 replaced, and
    control characters (C0, DEL and C1), line and paragraph separators and
    bidirectional formatting characters dropped. The `location` an
    application names is stored the same way.
    The server does not parse it: a client that wants "Firefox on Linux"
    makes that label from it, with the parser of its choice, and keeps the
    raw value for anything its parser does not know.
  - `client_address` is the client address of the sign-in (gin's
    `ClientIP`, so `GOMBIT_HTTP_TRUSTED_PROXIES` decides whether a
    proxy's `X-Forwarded-For` counts), only as much of it as
    `GOMBIT_AUTH_SESSION_CLIENT_ADDRESS` keeps: `coarse` (the default) its
    network, an IPv4 `/24` or an IPv6 `/48` (`203.0.113.0/24`,
    `2001:db8:1234::/48`), enough to tell home from an office but not to
    single out a device; `full` the whole address; `none` nothing. An IPv4
    address mapped into IPv6 is shown as IPv4.
  - `location` is empty unless the application names one. The framework
    does no geolocation. An application that wants it passes a
    `SessionLocator` (`framework.WithAuthSessionLocator`, or
    `auth.WithSessionLocator` on a Service it builds): a function from the
    request's context and the client's full address, whatever
    `GOMBIT_AUTH_SESSION_CLIENT_ADDRESS` stores, to a short label such as
    `Lisbon, PT`, or `""` when it does not know. It runs once per sign-in,
    in the request, before the session is written, so it should be a local
    lookup (a GeoIP database file), not a network call. Its answer is
    stored cut to 128 bytes.

What a session stores about its device is exactly those three values, on
each of its `refresh_tokens` rows (columns `user_agent`, `client_address`,
`location`; the login writes them, each rotation copies them to the row it
writes). They live as long as the rows do: nothing prunes revoked and
expired rows yet, so today they outlive the session; the retention policy
for `refresh_tokens` (#578) will bound them with the rows. Changing
`GOMBIT_AUTH_SESSION_CLIENT_ADDRESS` applies to sessions that sign in after
the change; the rows already written keep what they stored. The security
audit trail is separate: its events record the request's full client
address and User-Agent ([Audit trail](#audit-trail)), whatever this setting
says.

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
token is presented again before it expires (past `RefreshReuseGrace`, or
within it once the successor has been rotated again), runs inside the rotation
and takes the same lock.
What that guarantees depends on the operation, when a session refreshes at
the same moment:

- **Revoking the others, revoking all, and the reuse cascade** cover a
  concurrent rotation. A session that rotates while one of them runs cannot
  slip out with its new tokens: whichever comes second waits for the other, so
  the new tokens are revoked too (revoking the others keeps the requesting
  session, including any rotations it made since the request was
  authenticated).
- **Revoking one** covers a concurrent rotation too: it waits for the lock,
  and the session's id, which a rotation keeps, names the token the rotation
  wrote, so the revocation ends the session (AUTH-11). A logout of the same
  session that commits first makes it a 404 (the session has already ended),
  so a success always means the revocation ended the session itself; a
  logout that comes second waits for it and ends nothing more.
- **A revocation from a session that has ended** answers 401 and revokes
  nothing. The request's own session is checked again once the revocation
  holds the lock, so a session that another request revoked, or that
  expired, while its request waited for the lock cannot go on to revoke
  anything.
- **Logout** (`POST /auth/logout`, `RevokeRefresh`) takes the lock too, and
  follows a token that has rotated to its session's current token. A logout
  that races a refresh of the same token therefore ends the session either
  way: if the refresh commits first, the logout revokes the token it issued;
  if the logout does, the refresh finds its token revoked (401). The audit
  event (`auth.logout`) names the session it ended and, when a rotated token
  ended it, says so (`"via":"rotated"`): that token is an old copy, a sibling
  tab's or a thief's.
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

An app `gombit new` scaffolds has a page for these routes, `/sessions`
([frontend.md](frontend.md#sessions-page)).

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
| `auth.logout` | `POST /auth/logout` ended a session: the target is that session, and `"via":"rotated"` marks one an already rotated token ended. |
| `auth.user.registered` / `auth.superuser.created` | `POST /auth/register`; `gombit createsuperuser`. |
| `auth.user.disabled` / `auth.user.enabled` | `POST /auth/users/{id}/disable` / `/enable` (or `DisableUser` / `EnableUser`): the account's state changed. |
| `auth.mfa.enabled` / `auth.mfa.disabled` / `auth.mfa.reset` | MFA turned on (an enrollment confirmed), off by the user, or removed by an operator. |
| `auth.mfa.recovery_codes_regenerated` / `auth.mfa.recovery_code_used` / `auth.mfa.failed` | The recovery codes replaced; one used; an MFA operation refused a wrong password or code. |
| `auth.refresh.reuse_detected` | A rotated-away refresh token came back past `RefreshReuseGrace`, or within it once its successor had been rotated again: every session of the user was revoked. |
| `auth.refresh.reuse_within_grace` | A rotated-away refresh token came back within `RefreshReuseGrace` of its rotation: a 401, nothing revoked. Target type `session` names the successor's session by the id `GET /auth/sessions` lists it under, which its refreshes keep, so the id revokes it (or revoke all); details `successor` is `live` (a sibling tab's session, or a thief's who rotated a stolen token first: the one trace of that) or `ended` (signed out before the request came). |
| `auth.session.revoked` | `DELETE /auth/sessions…` (`details.scope`: `one`, `others` or `all`). |
| `auth.email.verified` | `POST /auth/verify-email`. |
| `auth.password.reset_requested` / `auth.password.reset` | A reset message sent to an account; a password set with its token (`details.sessions_revoked`). |
| `auth.password.changed` / `auth.password.change_failed` | A signed-in user's password change (`details.other_sessions_revoked`); one refused for a wrong current password. |
| `auth.email.change_requested` / `auth.email.changed` / `auth.email.change_failed` | An email change asked (a link mailed to the new address), confirmed (the account's email is the new one), or refused for a wrong current password. No address is recorded. |
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
| `GOMBIT_AUTH_THROTTLE_DISABLED` | `Config.Auth.ThrottleDisabled` | `false` (the credential endpoints are throttled; see [Abuse protection](#abuse-protection)) |
| `GOMBIT_AUTH_MFA_KEY` | `Config.Auth.MFAKey` | empty (MFA keys derive from `GOMBIT_JWT_SECRET`); at least 32 characters when set ([MFA](#multi-factor-authentication)) |

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
