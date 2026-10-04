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

- Object storage (`app.Storage()`, `GOMBIT_STORAGE_*`) and storage-backed `file` / `image` model fields. (`object-storage`, schema, [#279](https://github.com/gombit-dev/gombit/issues/279))

  File fields record ownership in a framework table, `storage_claims`,
  which new apps' migrations create. An existing app that adopts file
  fields adds the table to its migrations (`claims.Models()`; see
  docs/storage.md, "Setup").

## v0.6.1

The first release the compatibility manifest covers: upgrades are
described from it. For its own changes, and earlier releases, see the
[changelog](https://github.com/gombit-dev/gombit/blob/main/CHANGELOG.md).
