# Typed Configuration

Gombit's runtime packages receive typed configuration through
`config.Config`. Environment reads belong at this boundary, not in low-level
runtime packages.

## Defaults

`config.Default()` returns the current development defaults
(`config.DefaultFor(development)`). `config.DefaultFor(env)` applies the same
shape with environment-derived `/docs` and cache namespace. Mutating
`Environment` on `Default()` does not flip those fields.

- app name: `Gombit`
- environment: `development`
- HTTP address: `:8080`
- HTTP request timeout: `60s`
- API prefix: `/api/v1`
- interactive API docs: enabled (`/docs`)
- database driver: `sqlite`
- database DSN: `file:gombit.db?cache=shared&_fk=1`
- cache driver: `memory`
- cache namespace: `gombit:development`
- Redis address: `127.0.0.1:6379`
- log level: `info`
- log sink: `stderr`

## Environment

`config.Load()` reads the process environment and validates the resulting
configuration. Before that, it applies a `.env` file from the current working
directory if one exists — `gombit new` writes one with a per-project random
`GOMBIT_JWT_SECRET`, and `.env` values never override a variable the process
environment already sets. A real deployment sets configuration through its
own environment and does not ship a `.env` file (it is gitignored by every
`gombit new` scaffold), so this is a no-op in production. The M1-1 boundary
recognizes:

| Variable | Field | Default |
| --- | --- | --- |
| `GOMBIT_APP_NAME` | `Config.AppName` | `Gombit` |
| `GOMBIT_ENV` | `Config.Environment` | `development` |
| `GOMBIT_HTTP_ADDR` | `Config.HTTP.Addr` | `:8080` |
| `GOMBIT_HTTP_TRUSTED_PROXIES` | `Config.HTTP.TrustedProxies` | unset |
| `GOMBIT_HTTP_REQUEST_TIMEOUT` | `Config.HTTP.RequestTimeout` | `60s` |
| `GOMBIT_API_PREFIX` | `Config.API.Prefix` | `/api/v1` |
| `GOMBIT_DOCS_ENABLED` | `Config.API.DocsEnabled` | `true` (off in production when unset) |
| `GOMBIT_DATABASE_DRIVER` | `Config.Database.Driver` | `sqlite` |
| `GOMBIT_DATABASE_REQUIRED` | `Config.Database.Required` | `true` |
| `GOMBIT_DATABASE_DSN` | `Config.Database.DSN` | `file:gombit.db?cache=shared&_fk=1` |
| `GOMBIT_DATABASE_MAX_OPEN_CONNS` | `Config.Database.MaxOpenConns` | `0` |
| `GOMBIT_DATABASE_MAX_IDLE_CONNS` | `Config.Database.MaxIdleConns` | `0` |
| `GOMBIT_DATABASE_CONN_MAX_LIFETIME` | `Config.Database.ConnMaxLifetime` | `0` |
| `GOMBIT_CACHE_DRIVER` | `Config.Cache.Driver` | `memory` |
| `GOMBIT_CACHE_NAMESPACE` | `Config.Cache.Namespace` | derived from app/environment |
| `GOMBIT_REDIS_ADDR` | `Config.Cache.Redis.Addr` | `127.0.0.1:6379` |
| `GOMBIT_REDIS_USERNAME` | `Config.Cache.Redis.Username` | empty |
| `GOMBIT_REDIS_PASSWORD` | `Config.Cache.Redis.Password` | empty |
| `GOMBIT_REDIS_DB` | `Config.Cache.Redis.DB` | `0` |
| `GOMBIT_REDIS_DIAL_TIMEOUT` | `Config.Cache.Redis.DialTimeout` | `5s` |
| `GOMBIT_REDIS_READ_TIMEOUT` | `Config.Cache.Redis.ReadTimeout` | `3s` |
| `GOMBIT_REDIS_WRITE_TIMEOUT` | `Config.Cache.Redis.WriteTimeout` | `3s` |
| `GOMBIT_REDIS_TLS` | `Config.Cache.Redis.TLS` | `false` |
| `GOMBIT_REDIS_TLS_INSECURE` | `Config.Cache.Redis.TLSInsecure` | `false` |
| `GOMBIT_LOG_LEVEL` | `Config.Logging.Level` | `info` |
| `GOMBIT_LOG_SINK` | `Config.Logging.Sink` | `stderr` |
| `GOMBIT_JWT_SECRET` | `Config.Auth.JWTSecret` | empty (auth unmounted) |
| `GOMBIT_JWT_ACCESS_TTL` | `Config.Auth.AccessTokenTTL` | `15m` |
| `GOMBIT_JWT_REFRESH_TTL` | `Config.Auth.RefreshTokenTTL` | `168h` |
| `GOMBIT_AUTH_MODE` | `Config.Auth.Mode` | `jwt` |
| `GOMBIT_COOKIE_SECURE` | `Config.Auth.CookieSecure` | `true` |
| `GOMBIT_COOKIE_SAMESITE` | `Config.Auth.CookieSameSite` | `lax` |

`GOMBIT_API_PREFIX` is a live setting (D8). Go routes, Huma, and the admin
SPA honor it. The generated application SPA honors it when the prefix is
injected into `index.html`: `gombit build --embed` (Gin) and `gombit dev`
(Vite). A split/CDN deploy must replace `__GOMBIT_API_PREFIX__` in the
built `index.html` (or set `window.__GOMBIT_API_PREFIX__`) before serving;
the placeholder alone falls back to `/api/v1`. `gombit client generate`
normalizes OpenAPI path keys to `/api/v1` so page source does not need
regenerating. See [frontend.md](frontend.md#talking-to-the-api).

`GOMBIT_ENV` accepts the exact lowercase values `development`, `test`, and
`production`.
`GOMBIT_DATABASE_DRIVER` accepts `sqlite`, `postgres`, and `mysql`.
`GOMBIT_CACHE_DRIVER` accepts `memory`, `redis`, and `noop`.
When `GOMBIT_CACHE_NAMESPACE` is unset, the namespace is derived from the
normalized app name and environment, such as `gombit:development`.
`GOMBIT_HTTP_TRUSTED_PROXIES` is a comma-separated list of IPs or CIDRs passed
to Gin's trusted-proxy configuration. When unset, forwarded-client IP headers
are ignored. Production config rejects values that trust all proxies, such as
`0.0.0.0/0`.
`GOMBIT_HTTP_REQUEST_TIMEOUT` uses Go duration syntax such as `30s` or `2m`.
The value sets the cooperative per-request context deadline and the
`http.Server` read/write/idle timeouts; `0` disables all four.
`GOMBIT_DATABASE_CONN_MAX_LIFETIME` uses Go duration syntax such as `30m` or
`1h`.
Redis timeout values use the same Go duration syntax.
`GOMBIT_DOCS_ENABLED` accepts boolean values (`true`/`false`, `1`/`0`,
`yes`/`no`, `on`/`off`). When unset, docs stay on in `development` and `test`
and turn off in `production`. `/openapi.json` is always served.
`GOMBIT_LOG_LEVEL` accepts `debug`, `info`, `warn`, and `error`.
`GOMBIT_LOG_SINK` accepts `stderr`, `stdout`, and `mongo`; Mongo logging is an
external module hook, not a runtime dependency.

Validation returns `config.FieldErrors`, which names the typed field, the
environment variable, the invalid value, and the validation message.

`gombit config show` prints this typed result as aligned key/value lines.
DSN userinfo/passwords (URL form, MySQL `user:pass@tcp`, libpq
`password=`/`pwd=`/`pass=` keywords, and `?password=` query params), the Redis
password, and the JWT secret are redacted (`*****`); `Config.Redacted()` /
`RedactDSN` / `SanitizeError` are the helpers. Do not log raw DSNs or JWT
secrets.

Appendix C production checks fail loudly for a **non-empty** JWT secret
shorter than 32 characters, for the generated-app development placeholder,
and for cookie-mode auth (`GOMBIT_AUTH_MODE=cookie`) without
`GOMBIT_COOKIE_SECURE=true` (`config.Load` / `Validate` and `gombit
doctor`). The secret is never copied into `FieldError.Value` and is
redacted by `Config.Redacted()`. Remaining Appendix C cases (CORS) land
with the features that introduce those fields. Do not put JWT material in
`VITE_*`.
See [auth.md](auth.md) (Bearer default) and
[auth-cookie.md](auth-cookie.md) (`--auth cookie`, threat model).

Runtime extraction work should accept `config.Config` values instead of calling
`os.Getenv` or `os.LookupEnv` directly. The CLI (`gombit doctor`,
`gombit config show`) may call `config.Load()`.
