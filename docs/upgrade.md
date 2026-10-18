# Upgrading an application

Gombit keeps reusable behavior in the versioned framework module, and
generates application code that you own. Moving an app to a new framework
version is then mostly a module upgrade, plus whatever the release says your
own code, configuration, generated client or database needs. The `gombit
upgrade` tooling works that out for you, and proposes changes for review: it
never rewrites code you own without an explicit write action.

This page grows with the UPGRADE-0 epic. Today it covers **planning an
upgrade** (`gombit upgrade --dry-run`), **applying its dependency changes**
(`gombit upgrade --to`), **checking for deprecated APIs** (`gombit upgrade
check`), **rewriting them with codemods** (`gombit upgrade fix`), the
**baseline** (where an application stands) and the
**compatibility manifest** (what each release asks of an application moving
to it).

## Planning an upgrade

`gombit upgrade --dry-run` works out what moving the application to a
framework release takes, and changes nothing. For example (the changes
shown are illustrative):

```text
$ gombit upgrade --dry-run --to v0.8.0
Gombit upgrade plan (dry run: nothing is written)

Current: v0.6.1
Target:  v0.8.0

Dependency changes:
  github.com/gombit-dev/gombit v0.6.1 -> v0.8.0 (require)

Automatic changes available: 1
  - record-upgrade-baseline: gombit new records the app's upgrade baseline ...
    (record the upgrade baseline in gombit.yaml (gombit upgrade baseline --write))

Manual actions: 2
  - rename-setting [breaking]: A setting is renamed.
      Rename GOMBIT_OLD to GOMBIT_NEW.
  ...

Breaking behavior changes: 1
  - rename-setting (manual): A setting is renamed.

Informational: 1
  - faster-lists: Lists are faster.

No files changed.
```

It plans from the [baseline](#the-baseline) and the
[compatibility manifest](#the-compatibility-manifest) built into this
`gombit`, across every release after the current one up to the target:

- **Target:** `--to` selects it; by default, the newest release this
  `gombit` knows.
- **Dependency changes:** every go.mod directive about the framework that
  must change for the upgraded build to be the target release. The
  `require` moves. Then every `replace` of the framework, active today or
  not, is resolved at the target as the go command would once the require
  has moved, and whatever would decide that build without being the target
  release is reported:
  - a replace of every version by the framework module would keep pinning
    the build, so it moves to the target;
  - a dormant replace of the target version itself, or a replace of every
    version that an exact replace shadows today (a local checkout, a fork),
    would start to apply, so the plan says to drop it;
  - a replace of only today's required version stops applying, so it is
    dropped as dead.
- **Automatic changes:** only those the application needs (each automatic
  action can say whether an app needs it; recording the baseline is not
  needed once it is recorded).
- **Manual actions:** every one the manifest declares on the way, with what
  to do.
- **Breaking changes** this app faces, of every kind (a breaking automatic
  change the app does not need is left out, as it is of the automatic
  ones), and the informational changes.
- **Refusals:** a target or current version the manifest does not list, a
  downgrade, or an application whose baseline names no framework release (a
  `go.work` decides the build, a local checkout, a fork) fails with the
  reason, rather than planning from a guess.

It never prompts and needs no terminal, so it runs as is in CI; `--json`
prints the plan (`current`, `target`, `dependencies`, `releases`,
`automatic`, `manual`, `breaking`, `informational`, and the `baseline`) for
a script to read, and `--dir` selects the application directory. The
command exits 0 whatever the plan holds; to gate a CI job on it, read the
JSON, for example to fail when the upgrade has breaking changes or manual
actions:

```bash
gombit upgrade --dry-run --to v0.8.0 --json | jq -e '.breaking == [] and .manual == []'
```

## Applying the dependency changes

`gombit upgrade --to vX.Y.Z` (without `--dry-run`) carries out the plan's
dependency changes with the go command, and nothing else:

```text
$ gombit upgrade --to v0.7.1
Gombit upgrade v0.7.0 -> v0.7.1

Dependency changes:
  github.com/gombit-dev/gombit v0.7.0 -> v0.7.1 (require)

Go commands run:
  go get github.com/gombit-dev/gombit@v0.7.1

go.mod:
  - require github.com/gombit-dev/gombit v0.7.0
  + require github.com/gombit-dev/gombit v0.7.1
go.sum: 2 line(s) added, 0 removed

Review them with `git diff go.mod go.sum`. Still to do (not applied):
...
```

- **Target:** `--to` is required: nothing is applied to a default target.
  It plans first, so the plan's refusals stand (an unlisted version, a
  downgrade, a workspace, a local checkout, a fork).
- **The go command does the work.** Each `replace` the plan names is edited
  first (`go mod edit -dropreplace`, or `-replace`), then `go get
  github.com/gombit-dev/gombit@vX.Y.Z` moves the require and records the sums
  it needs. The resolution is Go's own (minimal version selection), so the
  same go.mod, module sources and go command give the same result (your
  `GOFLAGS`, `GOPROXY` and toolchain take part, as for any `go get`, and the
  go command may update the `go` or `toolchain` line); `go get` keeps the old
  version's sums, which a later `go mod tidy` prunes. Every command is
  pinned to the module's own `go.mod` and `go.sum` (a `-modfile` in
  `GOFLAGS`, or set with `go env -w`, is overridden), so no other file is
  written. In a vendored app (a top-level `vendor/`, with `modules.txt` or
  not) it leaves `vendor/` as it was, and says to run `go mod vendor` before
  building.
- **Visible:** every go command is printed, with the `go.mod` lines it
  removed and added and the `go.sum` line counts (`--json` has them all,
  with each command's output). Commit them after review.
- **Never masked:** a go command that fails is reported as it is: the
  command, its exit status, and everything Go printed. A result whose build
  is not the target release (another directive deciding it) is refused too.
  Either way `go.mod` and `go.sum` get back the bytes they had.
- **Only the dependencies.** The plan's automatic and manual changes are
  printed as still to do; they are not applied.

## Checking for deprecated APIs

`gombit upgrade check` reports every use, in the application, of a framework
API marked deprecated, with what to use instead:

```text
$ gombit upgrade check
Deprecated framework APIs used: 1
  internal/book/hooks.go:21:18: database.KeepStoredZeros
      Deprecated: use ScopeEdit with StoredValues, which also keeps the row's stored text editable and judges columns by whether their value changed.
```

- **What is deprecated** is what the framework's doc comments say, with the
  Go convention: a paragraph starting `Deprecated:` (the one `go doc`,
  gopls and pkg.go.dev show). Every such paragraph names its replacement; a
  test fails one that does not.
- **What can be deprecated:** a package (its package doc), a function, a
  method, a type, a constant, a variable, a struct field (named or embedded)
  or an interface method, documented with a line or a block comment.
- **How uses are found:** from type information, not text. The app's
  packages, tests included, and everything they depend on are loaded and
  type-checked from source as one graph (`go/packages`), so a framework
  declaration and the app's use of it are the same `go/types` object: a
  deprecated declaration is tied to the object it defines, and every use in
  the app resolved to the object it denotes, never matched by file
  positions. Every framework package in that graph is read, including one
  the app reaches only through another dependency (a wrapper embedding a
  framework type, whose promoted method the app calls). A use is a name
  resolving to such an object (a call, a selector, a composite literal key,
  a dot-imported name), an element of an unkeyed struct literal
  (`db.Options{"x", 0}` uses the first field), or the import of a
  deprecated package; one token can be two uses. So a local name that
  happens to match, a comment or a string is never reported.
- **Never a false clean:** the app and its dependencies must type-check, and
  a framework object from a package outside the loaded graph stops the scan;
  either is an error, with Go's diagnostics, rather than a scan that misses
  uses.
- **Output:** file, line and column, the symbol (`pkg.Name`,
  `pkg.Type.Field`, `(*pkg.Type).Method`) and the deprecation; `--json`
  prints them as a list. It exits 1 when there is any use, so a CI job can
  gate on it; `--dir` selects the application directory.

## Rewriting with codemods

For a change whose replacement is mechanical (a rename, a move to another
package, reordered arguments), the framework can ship a **codemod**: a
rewrite of the application's uses of one framework API. `gombit upgrade
fix` runs them and proposes the result as a patch. By default it writes
nothing:

```text
$ gombit upgrade fix > upgrade.patch     # the patch on stdout, a summary on stderr
$ git apply upgrade.patch                # after review
$ gombit upgrade fix --patch upgrade.patch  # always written; empty when there is nothing to do
$ gombit upgrade fix --write             # rewrite the files in place
$ gombit upgrade fix --only old-to-open  # one codemod (by ID)
```

**This gombit ships no codemods yet.** Each one comes with the release
whose change it rewrites. The only deprecated API today,
`database.KeepStoredZeros`, has no mechanical replacement (`StoredValues`
must be taken before the edit), so it stays a manual change, and `gombit
upgrade fix` says there is nothing to run.

- **Syntax and types, not text.** Uses are found from type information,
  exactly as `gombit upgrade check` finds them (the same `go/types` object,
  never a matching name), and rewritten in the syntax tree. A local name,
  a comment or a string is never touched. Imports follow: a moved API's
  package is imported, under a free name when its own is taken in the
  file, and the old import is dropped once nothing else uses it.
- **gofmt output.** Changed files are printed with `go/format`. CRLF line
  endings are kept, so the patch shows only the rewritten lines. A file
  that was not gofmt-formatted gets its formatting fixed too.
- **Checked before it is proposed.** The codemods run in passes, each on
  the application as the last pass left it, so one codemod can rewrite
  another's output (a rename of a rename, shipped by two releases). They
  stop when a pass changes nothing. That last pass type-checks the
  rewritten application. A rewrite that does not compile, or never
  settles, is refused, and nothing is proposed.
- **Idempotent.** The run ends only when the codemods change nothing more,
  so running them again on the result changes nothing.
- **Never silently.** Without `--write` nothing is changed.
  - The rewrite is computed from the exact bytes the type checker parsed,
    never a second read.
  - `--write` writes each file atomically. It writes nothing if any file's
    content is not exactly those bytes, because a file edited during the
    run would otherwise be reverted.
  - A file changed between passes fails the run.
  - The check is not a lock: an edit made in the instant between it and
    the write is lost, so don't run `--write` while editing.
- **Calls through wrappers.** A call through parentheses or type arguments
  (`(db.F)(x)`, `db.F[T](x)`) is still a call of the API.
- **Left for you.** A use a codemod cannot rewrite safely is left as it was
  and listed with the reason, and the command exits 1. Its position is in
  the file as the patch leaves it. Such uses include:
  - the new name would resolve to something else there (shadowed by a local
    or by the app's own method), or a pointer method on a value that is
    not addressable;
  - an embedded field, whose name would change with its type;
  - a generated file (regenerate it instead) or a cgo file;
  - an API the app's framework version does not have yet (run `gombit
    upgrade --to` first).
- **Build constraints.** Files the current build excludes are not
  type-checked, so not rewritten. Those that mention a codemod's API by
  name are listed: rerun with their `GOOS`, `GOARCH` or `-tags` (through
  `GOFLAGS`).
  - They are found by walking the directory tree the way `./...` matches
    it, so a platform-only directory (every file excluded, so no package
    to load) is covered too.
  - The walk skips `testdata`, `vendor`, names starting with `.` or `_`,
    and nested modules.
- **Uses, not declarations.** A codemod rewrites the app's uses of the API.
  Renaming an interface method that the app implements still means
  renaming the app's own methods by hand.

In the compatibility manifest, an `automatic` change applies a codemod as
the action `codemod:<id>`.

- **Its check** reports the change as needed when the app has uses to
  rewrite, or when files its build excludes mention the API.
- **Applying it** writes the rewrite. It then fails with
  `upgrade.ErrCodemodIncomplete`, naming what remains, if uses were left
  for the developer or excluded files mention the API: the change is not
  done until a run under those files' `GOOS`, `GOARCH` or tags is clean.

In Go, `upgrade.RunCodemods` runs codemods and returns the
proposed `upgrade.Fix` (`Patch`, `Write`, `Left`, `Unchecked`).
`upgrade.Codemod` declares one:

- `Rename` and/or `MoveTo`, for a rename or a move;
- or a `Rewrite` func for anything else. It gets each `upgrade.Use`: the
  identifier, the expression, the enclosing call, and `Import`/`Skip`
  helpers.

Its target is `Package` and `Symbol`, where `Symbol` is `Name` or
`Type.Name` for a method or field that `Type`'s own declaration declares.

A member that `Type` only reaches is refused, naming the type that
declares it. That covers a member promoted from an embedded type or
interface, and `Base`'s member when `Type` is defined from `Base`
(`type Type Base`) or is an alias of it. Uses are matched by the
declaration, so such a codemod would rewrite every use of `Base`'s member.

## The baseline

An application's baseline is two facts:

| Fact | Where it comes from |
| --- | --- |
| The **framework version** it builds against | `go.mod`, read offline: the `require github.com/gombit-dev/gombit` version, or the version of a `replace` of it by the framework module (when both an exact-version and an every-version replace exist, the exact one applies, as in Go). It is never copied anywhere else, and `gombit contract app` reads it the same way: both name a framework release or nothing, and both refuse a `go.mod` the go command refuses (the framework required twice, conflicting replaces). |
| The **scaffold version**: the generation conventions it was created with (layout, framework-owned files, their contents) | `gombit.yaml`, recorded by `gombit new` |

The scaffold version moves only when those conventions change in a way an
upgrade has to account for, not with every release; this release generates
scaffold version 1. The two are independent: an app can move to a newer
framework version and still carry the conventions it was generated with,
which is exactly what an upgrade needs to know.

`gombit new` records the scaffold version in a block of `gombit.yaml`:

```yaml
# Upgrade baseline (gombit upgrade): the format of this block, and the
# scaffold conventions the app was generated with. The framework version is
# go.mod's, and is not repeated here.
gombit:
  metadata: 1
  scaffold: 1
```

`metadata` is the version of the block's own format. Both keys are required,
both are integers, and no other key belongs in the block (a misspelt key is
refused rather than ignored; a new key means a new format). A framework that
finds a newer format, or a newer scaffold version, than it understands
refuses to guess, and asks you to upgrade the `gombit` CLI.

Show an app's baseline:

```text
$ gombit upgrade baseline
Framework: github.com/gombit-dev/gombit v0.8.2 (go.mod)
Scaffold:  1 (recorded in gombit.yaml, metadata format 1)
```

`--json` prints it as JSON (`framework.version`, `framework.required`,
`framework.replace` as `{path, version}`, `framework.local`,
`framework.workspace`, `scaffold`, `metadata`, `recorded`, and with
`--write`, `written`), and `--dir` selects the application directory.

`framework.version` is a framework release, or nothing. No version is
claimed when:

- `go.mod` replaces the framework with a local directory, a framework
  checkout (`framework.local`): there is no published version to upgrade
  from;
- `go.mod` replaces it with another module, a fork (`framework.replace`): a
  fork's version is not the framework's;
- a `go.work` applies to the app (`framework.workspace`), as the go command
  says (`go env GOWORK`, so `GOWORK=auto` and `go env -w` count). In a
  workspace, every module's requirements and replaces decide the framework
  version, which `gombit` does not work out. Run with `GOWORK=off` to read
  the app's own `go.mod`.

### Apps without upgrade metadata

An app generated before the metadata block existed has none. Its baseline is
still detected: the framework version from `go.mod` as always, and scaffold
version **0**, meaning "generated before upgrade metadata existed". Upgrade
rules for such apps start from scaffold 0.

To make that explicit, record it once:

```text
$ gombit upgrade baseline --write
Framework: github.com/gombit-dev/gombit v0.8.2 (go.mod)
Scaffold:  0 (recorded in gombit.yaml now, metadata format 1)
```

`--write` appends the metadata block (with `scaffold: 0`) to `gombit.yaml`,
creating the file if there is none, and changes nothing else in it. It does
nothing for an app that already records its baseline. A `gombit.yaml` the
block cannot simply be appended to (a flow-style `{...}` mapping, or several
YAML documents) is left alone, and the command prints the block to add by
hand. Commit the change with the app.

## The compatibility manifest

Every framework release declares its upgrade-relevant changes in a
machine-readable manifest, [`upgrade/manifest.yaml`](../upgrade/manifest.yaml),
built into the `gombit` binary. It is the single source for:

- the upgrade tooling, which plans an upgrade from it;
- [upgrade-notes.md](upgrade-notes.md), rendered from it (a test fails when
  the page is stale);
- the top of each GitHub release's notes, rendered from it by the release
  workflow.

Each change is classified by what moving across it takes:

| Kind | Meaning |
| --- | --- |
| `manual` | The developer acts; the change says how. |
| `automatic` | The upgrade tooling applies it, as a reviewable change. It names the action that does it (`upgrade.LookupAction`: an action carries its implementation; `codemod:<id>` is a [codemod](#rewriting-with-codemods)). The manifest is refused if this framework does not implement that action. |
| `informational` | Nothing to do, but worth knowing when moving across it. |

A change of any kind can be `breaking`: an app that relies on documented (or
reasonably relied-on) behavior can stop building, starting or working across
it until it acts. A fix that only code matching undocumented output notices
is not breaking. Each change also names the `area` it
touches (`api`, `behavior`, `cli`, `client`, `config`, `dependency`,
`scaffold`, `schema`, `security`), a one-line `summary`, Markdown `details`,
and the issues or pull requests behind it.

The manifest lists **every release** from the first one it covers (v0.6.1),
including releases with nothing to declare, so a version it does not list is
not a version with no changes. It has no known upgrade path, and the tooling
says why instead of guessing:

- a release older than the first one covered (see the
  [changelog](../CHANGELOG.md));
- a release newer than this `gombit` knows: upgrade the CLI first;
- a pseudo-version (an untagged commit, as an app on a branch has) or a
  pre-release the manifest does not list, whatever it sorts after: use a
  release it lists;
- a release within the manifest's range that is missing from it;
- a downgrade.

Planning from an application (`Manifest.PathFrom`, from the baseline
`upgrade.Detect` reports) also refuses a baseline that names no framework
release: a `go.work` that decides the build, a local checkout, or a fork.

Print the notes:

```text
$ gombit upgrade notes                      # every release, newest first
$ gombit upgrade notes --release v0.7.0     # one release
$ gombit upgrade notes --from v0.6.1        # an upgrade, to the newest release
$ gombit upgrade notes --from v0.6.1 --to v0.7.0 --json
```

`--json` prints the releases and their changes, classified (`automatic`,
`manual`, `informational`, and `breaking` across all three).

The changes of the next release collect under an `unreleased` entry, which
is renamed to the release's version when it is cut
([releasing.md](releasing.md)). The release workflow refuses a stable tag
the manifest is not closed at: the tag must be listed, be its newest
release, and leave nothing under `unreleased`. Tests check that every
issue the changelog's Unreleased section links is declared under
`unreleased`, even a follow-up to an issue an earlier release declares, or
exempt with a reason, and that every released section links only issues
the manifest declares under that release (`TestChangelogIsDeclared`,
`TestChangelogReleasesMatchManifest`).
