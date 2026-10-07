# Releasing

Maintainer runbook. Gombit is an importable Go module, so **every tag is a
permanent public API version** on pkg.go.dev — tags are cut deliberately, never
automatically on merge.

## Before you tag

1. **`main` is green**, including the full matrix in
   [`ci.yml`](../.github/workflows/ci.yml): lint, build, test, contract drift,
   and the SQLite/PostgreSQL/MySQL database, migration, and conformance jobs.
2. **No contract drift:**

   ```bash
   go run ./cmd/gombit client check --spec examples/client/openapi.json --out examples/client/frontend/src/api/generated
   ```

3. **Update [`CHANGELOG.md`](../CHANGELOG.md).** Move `[Unreleased]` entries
   under a new version heading with today's date, and refresh the link
   definitions at the bottom.
4. **Sanity-check the docs** that name a version — mainly
   [`installation.md`](installation.md).
5. **Decide the number.** Pre-1.0, breaking changes bump the minor
   (`0.1.0` → `0.2.0`); fixes bump the patch.
6. **Name the release in the compatibility manifest**
   ([`upgrade/manifest.yaml`](../upgrade/manifest.yaml)). Rename its
   `unreleased` entry to the version, or, only when there is no `unreleased`
   entry (no upgrade-relevant change), add a `- version: vX.Y.Z` entry with
   no changes: every release is listed, or upgrades from it have no known
   path. Check that the upgrade-relevant changes since the last release are
   there, classified: audit `git log vPREVIOUS..HEAD`, not only the CHANGELOG
   (see
   [upgrade.md](upgrade.md#the-compatibility-manifest)), then regenerate
   [`upgrade-notes.md`](upgrade-notes.md) and preview the release body:

   ```bash
   go test ./upgrade -run TestUpgradeNotesDoc -update
   go run ./cmd/gombit upgrade notes --release v0.1.0 --release-gate
   ```

   The release workflow puts these notes at the top of the GitHub release,
   and fails, before publishing, a stable release the manifest is not
   closed at: the version must be listed, be the newest release it lists,
   and leave no change under `unreleased` (`--release-gate`, as above). From
   the Actions tab that happens before the tag is created. **A pushed tag is
   already public** when the check runs, and since a tag is never moved, a
   missing entry then costs a new patch version. Run the preview above
   before pushing one.

## Cut the release

Two equivalent paths.

### From a tag (preferred)

```bash
git checkout main && git pull
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

### From the Actions tab

Run the **Release** workflow with a `patch` / `minor` / `major` bump. It
computes the next version from the latest `v*.*.*` tag, then creates and pushes
that tag for you. With no existing tags it starts at `v0.1.0` (or `v1.0.0` for
a `major` bump).

> A tag pushed by the workflow's `GITHUB_TOKEN` does not start another workflow
> run, so the dispatch path can't recurse into the tag-push trigger.

## What the workflow does

[`release.yml`](../.github/workflows/release.yml), in three jobs:

1. **version** — verifies modules, runs `go test ./...` as a release gate, and
   resolves the tag. Tags that aren't plain `vX.Y.Z` (for example
   `v0.2.0-rc.1`) are marked as GitHub **pre-releases**.
2. **build** — a matrix over `linux/amd64`, `linux/arm64`, `darwin/amd64`,
   `darwin/arm64`, and `windows/amd64`, then verifies each binary reports the
   right version before uploading it.
3. **publish** — packages `.tar.gz` (`.zip` for Windows), writes
   `SHA256SUMS.txt`, and publishes the GitHub release with generated notes.

### Why native runners and cgo

Binaries are built on **native runners with `CGO_ENABLED=1`**, not
cross-compiled from one Linux box.

The SQLite driver (`gorm.io/driver/sqlite` → `mattn/go-sqlite3`) is cgo-only,
and SQLite is the default for `gombit new`. A `CGO_ENABLED=0` build links and
starts fine, then fails at the first SQLite connection:

```text
database: open sqlite: Binary was compiled with 'CGO_ENABLED=0',
go-sqlite3 requires cgo to work. This is a stub
```

That failure doesn't show up until a user runs `gombit new` and hits the
database — exactly the first five minutes of using Gombit. Hence one runner per
target OS/arch.

If the SQLite driver ever moves to a pure-Go implementation
(`glebarez/sqlite` / `modernc.org/sqlite`), this collapses back to a single
cross-compiling job.

## After the release

1. **Verify the artifacts.**

   ```bash
   VERSION=v0.1.0
   curl -fsSLO "https://github.com/gombit-dev/gombit/releases/download/${VERSION}/gombit-${VERSION}-linux-amd64.tar.gz"
   curl -fsSLO "https://github.com/gombit-dev/gombit/releases/download/${VERSION}/SHA256SUMS.txt"
   sha256sum -c SHA256SUMS.txt --ignore-missing
   tar -xzf "gombit-${VERSION}-linux-amd64.tar.gz"
   ./gombit version
   ```

   `gombit version --short` must print the tag exactly.

2. **Verify the module path** — `go install` resolves through the proxy, which
   can lag a few minutes:

   ```bash
   go install github.com/gombit-dev/gombit/cmd/gombit@v0.1.0
   gombit version --short
   ```

3. **Check pkg.go.dev** has indexed
   [the new version](https://pkg.go.dev/github.com/gombit-dev/gombit).
4. **Smoke-test the quickstart** from the README in a clean directory with the
   released binary — `gombit new`, `gombit dev`, `gombit doctor`.

   The important assertion is that a scaffolded app builds with **no manual
   steps**: `gombit new` pins `go.mod` to the CLI's own version and runs
   `go mod tidy`, so a released binary must produce a tree that compiles
   straight away.

   ```bash
   cd "$(mktemp -d)"
   gombit new smoke --database sqlite
   cd smoke
   grep gombit go.mod          # must show the tag you just published
   go build ./...              # must succeed with no replace directive
   ```

   If this fails, the release is not usable — fix forward and cut a patch.
5. **Open a `[Unreleased]` section** in the changelog for the next cycle.

## If a release goes wrong

**Do not delete or move a published tag.** The Go module proxy caches
immutably; a re-pointed tag will serve the old content forever to anyone who
already fetched it, and mismatched content to everyone else.

Instead:

1. Mark the GitHub release as a pre-release or draft to stop promoting it.
2. Fix forward on `main`.
3. Cut the next patch version.
4. If the bad version is actively harmful, add a `retract` directive to
   `go.mod` in the follow-up release:

   ```go
   retract v0.1.1 // published with a broken SQLite build
   ```

## Pre-releases

```bash
git tag -a v0.2.0-rc.1 -m "v0.2.0-rc.1"
git push origin v0.2.0-rc.1
```

The workflow detects the non-plain version and publishes it as a GitHub
pre-release. `go install ...@latest` ignores pre-release versions, so testers
must ask for the exact tag.
