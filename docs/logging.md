# Logging

M1-6 introduces the runtime logging surface. Gombit uses Zap for structured
logs and defaults to stderr JSON output without requiring MongoDB.

## Configuration

`config.Default()` enables:

- level: `info`
- sink: `stderr`

`GOMBIT_LOG_LEVEL` accepts `debug`, `info`, `warn`, and `error`.
`GOMBIT_LOG_SINK` accepts `stderr`, `stdout`, and `mongo`.

The `mongo` sink is an external module hook. The runtime does not import or
open a MongoDB client, so setting `GOMBIT_LOG_SINK=mongo` alone is not enough:
`framework.New` then fails with `logging: mongo sink requires an external
zapcore.Core`. Applications that want Mongo-backed logs supply a Zap core from
their Mongo logging module, build the logger with it, and pass that logger
through `framework.WithLogger`:

```go
logger, err := logging.New(cfg.Logging, logging.WithCore(mongoCore))
if err != nil {
	return err
}
app, err := framework.New(framework.WithConfig(cfg), framework.WithLogger(logger))
```

## Runtime Use

`framework.New` builds a default logger from `Config.Logging` when no logger is
provided. `app.Logger()` returns the `*zap.Logger` escape hatch:

```go
app, err := framework.New()
if err != nil {
	return err
}

app.Logger().Info("started")
```

HTTP-only apps and apps without Mongo configuration boot with the default
stderr logger.

## Database Logging

A database attached with `framework.WithDatabase` logs through the app's logger
(named `database`), so it follows `GOMBIT_LOG_SINK` and `GOMBIT_LOG_LEVEL`:

| Statement | Level |
| --- | --- |
| Succeeds, or finds nothing (`gorm.ErrRecordNotFound`) | `debug` |
| Fails with an error the API answers with a 4xx, identified by the driver's error code: a unique, foreign-key or NOT NULL violation, or a model `Validate` error | `debug` |
| Slower than `database.SlowQueryThreshold` (200ms) | `warn` |
| Fails with any other error (a CHECK violation, a missing table, a failed migration), whatever its text says | `error` |

So at the default `info` level, normal traffic logs nothing and every server
error is reported. GORM's `Debug()` raises its session's statements to `info`.

Each entry carries the SQL with its placeholders (`sql`), `elapsed`, `rows`, the
calling `source` line and, for a failure, the driver's `error`. The SQL never
carries parameter values: they can be password hashes, tokens or personal data.
That covers `Scan` too, which GORM traces through its recorder: `database.Open`
sets GORM's process-wide `logger.RecorderParamsFilter` to drop values. The
driver's error text is logged as the driver reports it, and some drivers quote
the offending input there (for example a malformed UUID).

`database.Open` and `database.OpenConn` install a quiet logger of their own for
code that uses them without an app, such as a command. It writes slow and
failed statements to stderr, also without parameter values and without
not-found lookups. To use another GORM logger, set it on the database
(`db.Logger = ...`, including `db.Logger.LogMode(...)`) before passing it to
`framework.WithDatabase`; the app replaces only the logger `Open` installed.
`database.NewLogger` builds the app's logger from any `*zap.Logger`.
