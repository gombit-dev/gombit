# Cookie/session auth + CSRF example (`--auth cookie`)

Minimal `framework.App` with `Config.Auth.Mode = config.AuthModeCookie` +
SQLite. `framework.New` mounts `GET /api/v1/auth/csrf`,
`POST /api/v1/auth/{register,login,refresh,logout}`, `GET /api/v1/me`, and
the session-management routes under `/api/v1/auth/sessions`, plus the global
CSRF middleware. See [`docs/auth-cookie.md`](../../docs/auth-cookie.md)
for the threat model.

## Run

```sh
go run ./examples/auth-cookie
```

Interactive docs: [http://127.0.0.1:8081/docs](http://127.0.0.1:8081/docs).

## E2E (D10 envelopes, double-submit CSRF)

A cookie jar is required: the session and CSRF cookies must flow between
requests exactly like a browser would send them.

```sh
JAR=$(mktemp)

# bootstrap: fetch a CSRF cookie/token pair
CSRF_BODY=$(curl -sS -c "$JAR" -b "$JAR" http://127.0.0.1:8081/api/v1/auth/csrf)
CSRF=$(python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["csrf_token"])' <<<"$CSRF_BODY")

# register (state-changing: double-submit the CSRF token)
curl -sS -c "$JAR" -b "$JAR" -X POST http://127.0.0.1:8081/api/v1/auth/register \
  -H 'Content-Type: application/json' -H "X-CSRF-Token: $CSRF" \
  -d '{"email":"ada@example.com","password":"correct-horse"}'

# login: sets gombit_access + gombit_refresh HttpOnly cookies in $JAR
curl -sS -c "$JAR" -b "$JAR" -X POST http://127.0.0.1:8081/api/v1/auth/login \
  -H 'Content-Type: application/json' -H "X-CSRF-Token: $CSRF" \
  -d '{"email":"ada@example.com","password":"correct-horse"}'

# protected route: the access cookie in $JAR authenticates automatically
curl -sS -c "$JAR" -b "$JAR" http://127.0.0.1:8081/api/v1/me

# rotate the session (reads gombit_refresh from $JAR; no request body)
curl -sS -c "$JAR" -b "$JAR" -X POST http://127.0.0.1:8081/api/v1/auth/refresh \
  -H "X-CSRF-Token: $CSRF"

# list sessions ("current": true marks this jar's session), then revoke every
# other session; revoking is CSRF-protected like any DELETE
curl -sS -c "$JAR" -b "$JAR" http://127.0.0.1:8081/api/v1/auth/sessions
curl -sS -c "$JAR" -b "$JAR" -X DELETE \
  "http://127.0.0.1:8081/api/v1/auth/sessions?scope=others" \
  -H "X-CSRF-Token: $CSRF"

# logout (revokes the refresh token and clears both session cookies)
curl -sS -c "$JAR" -b "$JAR" -X POST http://127.0.0.1:8081/api/v1/auth/logout \
  -H "X-CSRF-Token: $CSRF"

# now unauthenticated
curl -sS -c "$JAR" -b "$JAR" http://127.0.0.1:8081/api/v1/me

rm -f "$JAR"
```

Try the register/login call **without** `-H "X-CSRF-Token: $CSRF"` to see
the 403 D10 rejection:

```sh
curl -sS -i -c "$JAR" -b "$JAR" -X POST http://127.0.0.1:8081/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"ada@example.com","password":"correct-horse"}'
# HTTP/1.1 403 Forbidden
# {"error":{"code":"authorization","message":"csrf token missing or invalid", ...}}
```

The development JWT secret in `main.go` also signs the CSRF token.
Production config rejects secrets shorter than 32 characters, and rejects
`CookieSecure=false` outright (Appendix C) — this example sets it to
`false` only because it serves plain HTTP on `127.0.0.1`.

## Create an admin account (`gombit createsuperuser`)

Identical to the [Bearer example](../auth/README.md#create-an-admin-account-gombit-createsuperuser):
`createsuperuser` does not depend on the auth mode, only on
`GOMBIT_JWT_SECRET` being set.

```sh
GOMBIT_DATABASE_DRIVER=sqlite \
GOMBIT_DATABASE_DSN='file:auth-cookie-example.db?cache=shared&_fk=1' \
GOMBIT_JWT_SECRET='dev-only-example-jwt-secret-not-for-prod' \
go run ./cmd/gombit createsuperuser --no-input \
  --email admin@example.com --password correct-horse-battery-staple
```

See [`docs/cli.md`](../../docs/cli.md#gombit-createsuperuser) and
[`docs/auth-cookie.md`](../../docs/auth-cookie.md).
