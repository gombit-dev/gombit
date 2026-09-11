# Installation

Install the `gombit` CLI, then verify it with `gombit doctor`.

- [Prerequisites](#prerequisites)
- [Install](#install)
- [Verify](#verify)
- [Platform notes](#platform-notes)
- [Troubleshooting](#troubleshooting)
- [Upgrading and uninstalling](#upgrading-and-uninstalling)

## Prerequisites

| Tool | Version | Required for |
| --- | --- | --- |
| [Go](https://go.dev/dl/) | 1.26+ | building the CLI and your application |
| A C toolchain | gcc/clang | **SQLite only** — see below |
| [Node.js](https://nodejs.org/) | 22+ | `gombit dev`, the frontend, and TypeScript client generation |
| [Atlas](https://atlasgo.io/) | Community Edition | `gombit db makemigrations` and `gombit db migrate` |

Only Go is needed to install the binary. The rest are needed by the commands
that use them, and `gombit doctor` tells you which are missing.

### SQLite needs cgo

Gombit's SQLite driver (`gorm.io/driver/sqlite` → `mattn/go-sqlite3`) is
cgo-only, and SQLite is the default for `gombit new`. A binary built with
`CGO_ENABLED=0` compiles and starts, but fails at the first SQLite connection:

```text
FAIL  database  database: open sqlite: Binary was compiled with 'CGO_ENABLED=0',
                go-sqlite3 requires cgo to work. This is a stub
```

Consequences:

- **Official release binaries are built with cgo on native runners**, so
  SQLite works out of the box. Nothing to do.
- **`go install` needs a C compiler on your machine** if you intend to use
  SQLite. `cc`/`gcc` on Linux, Xcode Command Line Tools on macOS, a MinGW-w64
  toolchain on Windows.
- **PostgreSQL and MySQL are pure Go.** If you only target those, a
  `CGO_ENABLED=0` build is fine.

Install Atlas (needed for migrations):

```bash
curl -sSf https://atlasgo.sh | sh -s -- --community
```

## Install

### Option 1 — `go install` (recommended)

```bash
go install github.com/gombit-dev/gombit/cmd/gombit@latest
```

Pin a version instead of tracking `@latest`:

```bash
go install github.com/gombit-dev/gombit/cmd/gombit@v0.1.0
```

The binary lands in `$(go env GOBIN)`, or `$(go env GOPATH)/bin` when `GOBIN`
is unset. That directory must be on your `PATH`:

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
```

Add that line to `~/.bashrc`, `~/.zshrc`, or your shell's equivalent to make it
permanent.

Binaries installed this way carry no ldflags, so `gombit version` reads the
module version recorded by Go — `@v0.1.0` reports `v0.1.0`, while `@latest` on
an untagged commit may report a pseudo-version.

### Option 2 — release archive

Download from the [releases page](https://github.com/gombit-dev/gombit/releases).
Archives are published for `linux/amd64`, `linux/arm64`, `darwin/amd64`,
`darwin/arm64`, and `windows/amd64`.

```bash
VERSION=v0.1.0
OS=linux      # or darwin
ARCH=amd64    # or arm64

curl -fsSLO "https://github.com/gombit-dev/gombit/releases/download/${VERSION}/gombit-${VERSION}-${OS}-${ARCH}.tar.gz"
curl -fsSLO "https://github.com/gombit-dev/gombit/releases/download/${VERSION}/SHA256SUMS.txt"

# Verify before extracting.
sha256sum -c SHA256SUMS.txt --ignore-missing

tar -xzf "gombit-${VERSION}-${OS}-${ARCH}.tar.gz"
sudo install -m 0755 gombit /usr/local/bin/gombit
```

On macOS `sha256sum` may not exist; use `shasum -a 256 -c SHA256SUMS.txt
--ignore-missing`.

Windows (PowerShell):

```powershell
$Version = "v0.1.0"
Invoke-WebRequest -Uri "https://github.com/gombit-dev/gombit/releases/download/$Version/gombit-$Version-windows-amd64.zip" -OutFile gombit.zip
Expand-Archive gombit.zip -DestinationPath "$env:LOCALAPPDATA\gombit"
$env:PATH += ";$env:LOCALAPPDATA\gombit"
```

### Option 3 — from source

```bash
git clone https://github.com/gombit-dev/gombit.git
cd gombit
go build -o gombit ./cmd/gombit
```

A source build reports `dev` from `gombit version`. To stamp it the way
releases do:

```bash
pkg=github.com/gombit-dev/gombit/cli
go build -ldflags "-X ${pkg}.Version=$(git describe --tags --always)" -o gombit ./cmd/gombit
```

Contributors run the CLI directly with `go run ./cmd/gombit …` — see
[CONTRIBUTING.md](../CONTRIBUTING.md).

## Verify

```bash
gombit version
```

A release archive (Option 2) is stamped with build metadata:

```text
gombit:   v0.1.0
commit:   9abb3c6ecc8c1bf93419aa43c4d4f1ae3de97a2b
built:    2026-08-18T19:33:15Z
go:       go1.26.0
platform: linux/amd64
```

`go install` (Option 1) and a plain `go build` from source (Option 3) carry
no ldflags, so `commit` and `built` read `unknown` instead — only `gombit`
(the module version) and the `go`/`platform` fields are populated. That's
expected, not a broken install.

Then check the whole environment:

```bash
gombit doctor
```

```text
STATUS  CHECK       DETAIL
ok      go          go version go1.26.0 linux/amd64
ok      node        v22.13.1
ok      config      valid (development)
ok      database    sqlite reachable
skip    redis       cache driver is memory
skip    migrations  database/migrations not present
ok      http        :8080 parses, not in use
ok      insecure    no issues in current config fields
```

`skip` outside an application directory is expected. Run `doctor` again from
inside a generated app for the full picture. See [cli.md](cli.md).

Next: [the tutorial](tutorial.md).

## How generated apps find the framework

`gombit new` pins the generated `go.mod` to **the same gombit version as the
binary that generated it**, then runs `go mod tidy` to populate `go.sum`. An
installed CLI therefore produces an app that builds immediately:

```bash
gombit new demo --database sqlite
cd demo && go build ./...   # no replace, no manual steps
```

Two flags control this:

| Flag | Use |
| --- | --- |
| `--framework-version` | Pin a specific framework version instead of the CLI's own |
| `--skip-tidy` | Don't run `go mod tidy` (offline; you'll run it yourself) |

**If you built the CLI from source**, it reports `dev` (or a `+dirty`
pseudo-version), which the module proxy cannot resolve. `gombit new` says so,
skips tidy, and pins `v0.0.0`; point the app at your checkout:

```bash
cd demo
go mod edit -replace github.com/gombit-dev/gombit=/path/to/gombit
go mod tidy
```

That's the right behaviour for framework development — see
[CONTRIBUTING.md](../CONTRIBUTING.md).

## Platform notes

### Linux

Install a C toolchain for SQLite:

```bash
sudo apt-get install -y build-essential   # Debian/Ubuntu
sudo dnf groupinstall -y "Development Tools"   # Fedora/RHEL
```

### macOS

```bash
xcode-select --install
```

Both Intel (`darwin/amd64`) and Apple Silicon (`darwin/arm64`) archives are
published. Downloaded binaries are not notarized, so Gatekeeper may quarantine
them:

```bash
xattr -d com.apple.quarantine /usr/local/bin/gombit
```

`go install` builds locally and avoids that entirely.

### Windows

Use the `windows/amd64` archive, or `go install` with a MinGW-w64 toolchain
(for example via [MSYS2](https://www.msys2.org/)) if you want SQLite.

### WSL2

Treat WSL2 as Linux and install everything **inside** the WSL filesystem.
Working out of `/mnt/c/...` makes file watching in `gombit dev` unreliable and
slows Go builds substantially. Clone to `~/` instead.

If `gombit dev` starts but the browser can't reach it, bind explicitly:

```bash
GOMBIT_HTTP_ADDR=0.0.0.0:8080 gombit dev
```

## Troubleshooting

**`gombit: command not found`** — `$(go env GOPATH)/bin` is not on your `PATH`.
See [Option 1](#option-1--go-install).

**`go: module ... requires go >= 1.26`** — your Go toolchain is older than
`go.mod`. Upgrade from [go.dev/dl](https://go.dev/dl/); the version in your
distribution's package manager is often well behind.

**`go-sqlite3 requires cgo to work`** — a `CGO_ENABLED=0` binary hit SQLite. Use
a release archive, or rebuild with `CGO_ENABLED=1` and a C compiler installed.
See [SQLite needs cgo](#sqlite-needs-cgo).

**`atlas: executable file not found in $PATH`** — install Atlas Community
Edition (above), or point `gombit db` at it with `--atlas-bin`.

**`gombit doctor` reports the http port in use** — something else holds `:8080`.
Change it with `GOMBIT_HTTP_ADDR=:8081`. See [config.md](config.md).

**`createsuperuser` fails with a missing JWT secret** — set `GOMBIT_JWT_SECRET`.
Without it Bearer auth is unmounted, so a superuser could never log in. See
[auth.md](auth.md).

## Upgrading and uninstalling

```bash
go install github.com/gombit-dev/gombit/cmd/gombit@latest
```

Read the [CHANGELOG](../CHANGELOG.md) first — Gombit is pre-1.0, so minor
versions may break compatibility.

The framework module version used by *your application* is independent of the
CLI, and is upgraded in the app:

```bash
go get github.com/gombit-dev/gombit@v0.1.0
go mod tidy
```

To uninstall, delete the binary:

```bash
rm "$(go env GOPATH)/bin/gombit"     # go install
sudo rm /usr/local/bin/gombit        # release archive
```
