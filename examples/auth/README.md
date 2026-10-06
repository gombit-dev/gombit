# Bearer auth example

Minimal `framework.App` with JWT secret + SQLite. `framework.New` mounts
`POST /api/v1/auth/{register,login,refresh,logout}`, `GET /api/v1/me`, and
the session-management routes under `/api/v1/auth/sessions`.

## Run

```sh
go run ./examples/auth
```

Interactive docs: [http://127.0.0.1:8080/docs](http://127.0.0.1:8080/docs).

## E2E (D10 envelopes)

```sh
# register
curl -sS -X POST http://127.0.0.1:8080/api/v1/auth/register \
  -H 'Content-Type: application/json' \
  -d '{"email":"ada@example.com","password":"correct-horse"}'

# login
LOGIN=$(curl -sS -X POST http://127.0.0.1:8080/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"ada@example.com","password":"correct-horse"}')
echo "$LOGIN"

ACCESS=$(python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["access_token"])' <<<"$LOGIN")
REFRESH=$(python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["refresh_token"])' <<<"$LOGIN")

# protected route
curl -sS http://127.0.0.1:8080/api/v1/me -H "Authorization: Bearer $ACCESS"

# rotate refresh (old refresh is then invalid)
ROTATED=$(curl -sS -X POST http://127.0.0.1:8080/api/v1/auth/refresh \
  -H 'Content-Type: application/json' \
  -d "{\"refresh_token\":\"$REFRESH\"}")
echo "$ROTATED"
ACCESS=$(python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["access_token"])' <<<"$ROTATED")
REFRESH=$(python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["refresh_token"])' <<<"$ROTATED")

# logout (revokes the current refresh token and bound access JWT)
curl -sS -X POST http://127.0.0.1:8080/api/v1/auth/logout \
  -H 'Content-Type: application/json' \
  -d "{\"refresh_token\":\"$REFRESH\"}"
```

## Sessions

Each login is a session. Log in twice (for example, once per device) and
manage the sessions from either one:

```sh
LOGIN=$(curl -sS -X POST http://127.0.0.1:8080/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"ada@example.com","password":"correct-horse"}')
ACCESS=$(python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["access_token"])' <<<"$LOGIN")
curl -sS -X POST http://127.0.0.1:8080/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"ada@example.com","password":"correct-horse"}' >/dev/null

# list: the session making the request has "current": true
SESSIONS=$(curl -sS http://127.0.0.1:8080/api/v1/auth/sessions -H "Authorization: Bearer $ACCESS")
echo "$SESSIONS"
OTHER=$(python3 -c 'import json,sys; print(next(s["id"] for s in json.load(sys.stdin)["data"] if not s["current"]))' <<<"$SESSIONS")

# revoke one (ids change on every refresh: list again before revoking)
curl -sS -X DELETE "http://127.0.0.1:8080/api/v1/auth/sessions/$OTHER" \
  -H "Authorization: Bearer $ACCESS"

# revoke every other session, or every session including this one
curl -sS -X DELETE "http://127.0.0.1:8080/api/v1/auth/sessions?scope=others" \
  -H "Authorization: Bearer $ACCESS"
curl -sS -X DELETE "http://127.0.0.1:8080/api/v1/auth/sessions?scope=all" \
  -H "Authorization: Bearer $ACCESS"
```

See [`docs/auth.md`](../../docs/auth.md#sessions) for the semantics.

The development JWT secret in `main.go` is not a production secret.
Production config rejects secrets shorter than 32 characters.

## Create an admin account (`gombit createsuperuser`)

The example uses an in-memory SQLite database, so point `gombit
createsuperuser` at a file-backed database instead if you want the account
to persist across runs:

```sh
GOMBIT_DATABASE_DRIVER=sqlite \
GOMBIT_DATABASE_DSN='file:auth-example.db?cache=shared&_fk=1' \
GOMBIT_JWT_SECRET='dev-only-example-jwt-secret-not-for-prod' \
go run ./cmd/gombit createsuperuser --no-input \
  --email admin@example.com --password correct-horse-battery-staple
```

See [`docs/cli.md`](../../docs/cli.md#gombit-createsuperuser) and
[`docs/auth.md`](../../docs/auth.md).
