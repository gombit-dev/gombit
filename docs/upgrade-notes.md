# Upgrade notes

<!-- Generated from upgrade/manifest.yaml by
     go test ./upgrade -run TestUpgradeNotesDoc -update
     Do not edit by hand: change the manifest. -->

What each release asks of an application moving to it, newest first. This
page, the release notes, and the upgrade tooling all come from the same
compatibility manifest (see [upgrade.md](upgrade.md)).

## Unreleased

### Manual: action required

- **Breaking.** `upgrade` is a framework command, and a reserved management-command name. (`reserved-upgrade-command`, cli, [#341](https://github.com/gombit-dev/gombit/issues/341))

  An app that registers its own `upgrade` command loses it: the
  framework's is added first, and Cobra resolves to it. Rename the
  app's command (and its `internal/<pkg>/commands.go` registration).

- **Breaking.** `admin.Register` refuses a model whose primary key spans several columns. (`admin-composite-primary-keys`, api, [#453](https://github.com/gombit-dev/gombit/issues/453))

  Such a model used to register, and the admin addressed its rows by
  the first key column alone. Startup now fails with `admin: <Model>
  has a composite primary key, which is not supported`. Give the
  model a single-column primary key, or keep it out of the admin
  registry.

- `OnStop` hooks get their own shutdown-timeout budget, so a full shutdown can take longer. (`stop-hook-shutdown-budget`, behavior, [#431](https://github.com/gombit-dev/gombit/issues/431))

  A shutdown can now take the drain delay plus up to twice the
  shutdown timeout (`framework.WithShutdownTimeout`, 10s by default).
  Check that the orchestrator's termination grace period (Kubernetes
  `terminationGracePeriodSeconds`, ECS `stopTimeout`) still covers
  it.

- **Breaking.** `gombit contract app` refuses a framework replaced by a fork, and a `go.mod` the go command refuses. (`contract-app-fork-unresolved`, cli, [#341](https://github.com/gombit-dev/gombit/issues/341))

  A `replace` of `github.com/gombit-dev/gombit` by another module used
  to report the fork's version as the framework's; it is now
  unresolved, like a local path. A `go.mod` requiring the framework
  twice, or replacing it twice with different targets, is an error.
  The replace that counts is the one the go command applies to the
  required version (an exact-version replace over an every-version
  one; a replace of another version does not apply), so a go.mod
  carrying several replaces can report a different version, or
  resolve where it used to fail. Build the contract against a
  framework release.

- **Breaking.** A `framework.App` runs once: every return from `Run`, `RunContext` or `RunWorker` runs the stop hooks and closes what the app opened. (`app-runs-once`, api, [#435](https://github.com/gombit-dev/gombit/issues/435))

  That includes the returns before serving (a listener that cannot
  bind, refused worker options). Code that retried `RunContext` with
  the same `*App` after such an error now runs against a closed cache
  and job dispatcher: build a new `App` (`framework.New`) to retry.

- **Breaking.** Production refuses `GOMBIT_HTTP_TRUSTED_PROXIES` values that trust every peer: a zero-length prefix in any spelling, or ranges that together cover all addresses. (`trusted-proxies-trust-all`, security, [#500](https://github.com/gombit-dev/gombit/issues/500))

  `10.0.0.0/0`, `::0/0` or `0.0.0.0/1,128.0.0.0/1` used to start;
  `Config.Validate` now refuses them in production, since trusting
  every peer lets any client spoof its forwarded IP. List only the
  addresses of the proxies you control.

### Automatic: applied by the upgrade tooling

- `gombit new` records the app's upgrade baseline in a `gombit:` block of `gombit.yaml`. (`record-upgrade-baseline`, scaffold, action `record-baseline`, [#341](https://github.com/gombit-dev/gombit/issues/341))

  An app generated before records none, and is detected as scaffold
  version 0. `gombit upgrade baseline --write` records it.

### Informational

- `framework.SanitizeHTML` keeps text after a stray `<` when the value also has a real tag. (`sanitize-html-stray-angle-bracket`, security, [#433](https://github.com/gombit-dev/gombit/issues/433))

  That tail comes back as `SanitizeHTML` returns a value with no
  complete tag: raw, undecoded input. It is no safer than that case;
  escape on output as before.

- The admin and embedded SPA pages' Content-Security-Policy allows the object store's origin for images and requests. (`admin-csp-allows-storage-origin`, security, [#330](https://github.com/gombit-dev/gombit/issues/330))

  With a remote store (an S3 bucket, a CDN), its origin joins the
  policy's `img-src` and `connect-src`, so file previews and direct
  uploads work. Review it if the store's origin is shared with
  content you do not control.

- With `GOMBIT_HTTP_REQUEST_TIMEOUT` set, the 504 a timed-out handler writes reaches the client; `WriteTimeout` is the request timeout plus 5s. (`request-timeout-response-reaches-client`, behavior, [#430](https://github.com/gombit-dev/gombit/issues/430))

- Requests whose handler panics are counted in `gombit_http_requests_total` (as 500, or the status already sent). (`panicking-requests-in-metrics`, behavior, [#432](https://github.com/gombit-dev/gombit/issues/432))

- On Windows, `gombit make resource` replaces files with a POSIX-semantics rename, so a file open with delete sharing no longer blocks it. (`make-resource-windows-posix-rename`, cli, [#341](https://github.com/gombit-dev/gombit/issues/341))

  On a volume that refuses that rename (FAT or exFAT, a network share
  or mapped drive, a `\\wsl$` path, or Windows before 10 1709) it keeps
  its previous plain rename and its rollback.

- Unknown paths and recovered panics answer with the D10 error envelope (`not_found` 404, `internal` 500). (`d10-not-found-and-panics`, behavior, [#438](https://github.com/gombit-dev/gombit/issues/438))

  A 404 used to be Gin's plain-text body (or empty under an embedded
  frontend) and a panic an empty 500. A client matching those bodies
  sees JSON now; the documented error contract was already D10, so
  this is not marked breaking.

- A response that cannot be encoded as JSON is a D10 500 instead of a 200 with a plain-text body. (`unencodable-response-500`, behavior, [#442](https://github.com/gombit-dev/gombit/issues/442))

- Every HTML document of an embedded frontend gets the SPA browser security policy, not only the root `index.html`. (`embedded-html-browser-policy`, security, [#434](https://github.com/gombit-dev/gombit/issues/434))

- The in-memory cache's `Increment` refuses to overflow, and a stored `nil`, as Redis does. (`memory-cache-increment-overflow`, behavior, [#437](https://github.com/gombit-dev/gombit/issues/437))

- `cache.WithJanitor` with a zero or negative interval means no janitor, instead of crashing the process. (`cache-janitor-non-positive`, behavior, [#436](https://github.com/gombit-dev/gombit/issues/436))

- A job whose worker died mid-attempt is no longer redelivered past `MaxAttempts`: it moves to the failed jobs as exhausted. (`jobs-crash-redelivery-max-attempts`, behavior, [#494](https://github.com/gombit-dev/gombit/issues/494))

  An attempt interrupted by `Release` or worker shutdown may still run
  once more past `MaxAttempts`, as before.

- `gombit jobs retry --all` retries each failed job at most once per invocation. (`jobs-retry-all-once`, cli, [#493](https://github.com/gombit-dev/gombit/issues/493))

  A job that fails again while the command runs is left for the next
  invocation, instead of being retried until the command times out.

- The framework requires newer database drivers: `github.com/go-sql-driver/mysql` v1.10.1 and `github.com/mattn/go-sqlite3` v1.14.52. (`database-driver-updates`, dependency, [#536](https://github.com/gombit-dev/gombit/issues/536), [#540](https://github.com/gombit-dev/gombit/issues/540))

  Go's minimal version selection moves an app's build to them with the
  framework upgrade. The framework also newly requires the AWS SDK
  for Go v2 (the S3 storage driver), `golang.org/x/mod`,
  `golang.org/x/sys` and `gopkg.in/yaml.v3`.

- Object storage (`app.Storage()`, `GOMBIT_STORAGE_*`) and storage-backed `file` / `image` model fields. (`object-storage`, schema, [#279](https://github.com/gombit-dev/gombit/issues/279), [#323](https://github.com/gombit-dev/gombit/issues/323), [#324](https://github.com/gombit-dev/gombit/issues/324), [#325](https://github.com/gombit-dev/gombit/issues/325), [#326](https://github.com/gombit-dev/gombit/issues/326), [#327](https://github.com/gombit-dev/gombit/issues/327), [#328](https://github.com/gombit-dev/gombit/issues/328), [#329](https://github.com/gombit-dev/gombit/issues/329), [#530](https://github.com/gombit-dev/gombit/issues/530))

  File fields record ownership in a framework table, `storage_claims`,
  which new apps' migrations create. An existing app that adopts file
  fields adds the table to its migrations (`claims.Models()`; see
  docs/storage.md, "Setup").

## v0.6.1

The first release the compatibility manifest covers: upgrades are
described from it. For its own changes, and earlier releases, see the
[changelog](https://github.com/gombit-dev/gombit/blob/main/CHANGELOG.md).
