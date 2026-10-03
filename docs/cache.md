# Cache

M1-5 introduces the runtime cache boundary. Cache callers depend on value
semantics instead of go-redis command types:

```go
type Cache interface {
	Get(ctx context.Context, key string, dst any) (bool, error)
	Set(ctx context.Context, key string, value any, ttl time.Duration) error
	Delete(ctx context.Context, keys ...string) error
	Increment(ctx context.Context, key string, delta int64) (int64, error)
}
```

Values are JSON encoded by the built-in drivers. `Get` returns `false, nil`
for a cache miss.

## Drivers

`cache.Open` supports:

| Driver | Config value |
| --- | --- |
| Memory | `memory` |
| Redis | `redis` |
| Disabled | `noop` |

The default cache driver is memory, which keeps tests and local SQLite apps free
from an external Redis dependency. Redis remains available when selected:

```go
store, err := cache.Open(cfg.Cache)
if err != nil {
	return err
}
defer store.Close()

app, err := framework.New(framework.WithCache(store))
if err != nil {
	return err
}

redisClient := app.Redis()
```

`app.Redis()` returns `nil` unless the Redis driver or `framework.WithRedis` is
used. Application code that only needs `Get`, `Set`, `Delete`, or `Increment`
should use `app.Cache()` instead. `framework.WithRedis` wraps the provided
client directly and does not apply `Config.Cache.Namespace`; use `cache.Open`
when configured namespacing is required.

## Configuration

| Environment variable | Config field | Default |
| --- | --- | --- |
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

The `GOMBIT_REDIS_*` settings are shared: the `redis` background-job driver
(`GOMBIT_JOBS_DRIVER=redis`) connects with them too, and they are validated
when either the cache or the jobs driver uses Redis. See
[jobs.md](jobs.md#drivers).

Cache keys opened through `cache.Open` are prefixed with the configured
namespace. Caller keys are prefixed as-is: leading colons are not stripped, so
`foo` and `:foo` occupy distinct slots. When no namespace is configured
explicitly, `config.Load` derives it from the normalized app name and
environment, such as `gombit:development`.
