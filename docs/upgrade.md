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
check`), **rewriting them with codemods** (`gombit upgrade fix`),
**configuration changes** (`gombit upgrade config`), the **generated
contract artifacts** (`gombit upgrade contract`), **framework schema
changes** (the migrations an upgrade needs), the **baseline** (where an application stands) and the
**compatibility manifest** (what each release asks of an application moving
to it).

Which releases may break, and how long a deprecated API stays, is set by
the [compatibility and deprecation policy](compatibility.md). The tooling
follows that policy.

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
- **Never a migration.** It runs no migration, in any environment. When the
  path changes framework-owned tables the app has, the output says the
  upgrade is not complete yet and lists the
  [migration steps](#framework-schema-changes) (`migration_required` in
  `--json`).

## Checking for deprecated APIs

`gombit upgrade check` reports every use, in the application, of a framework
API marked deprecated, with what to use instead:

```text
$ gombit upgrade check
Deprecated framework APIs used: 1
  internal/book/hooks.go:21:18: database.KeepStoredZeros
      Deprecated: since v0.8.0, use ScopeEdit with StoredValues, which also keeps the row's stored text editable and judges columns by whether their value changed.
      (deprecated in v0.8.0; the compatibility policy lets v0.10.0 (or v1.0.0, if it comes first) remove it)
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
  `pkg.Type.Field`, `(*pkg.Type).Method`) and the deprecation. When the
  deprecation names its release (`Deprecated: since vX.Y.Z, ...`, as every
  framework deprecation does), a line follows with the deprecation window:
  the first release the [compatibility policy](compatibility.md#deprecations)
  lets remove the API. `--json` prints the uses as a list, with `since` and
  `removable_from` (and `removable_from_major`, v1.0.0 for a pre-1.0
  deprecation) when the release is named. It exits 1 when there is any
  use, so a CI job can gate on it; `--dir` selects the application
  directory.

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

## Configuration changes

`gombit upgrade config` reports what an upgrade's configuration changes
mean for the application. It covers each variable the releases between the
app's framework version and the target rename, remove, re-default, newly
require, or newly refuse values of:

```text
$ gombit upgrade config --to v0.7.1
Upgrade v0.6.1 -> v0.7.1
Configuration changes: 1
  - [security review] `GOMBIT_HTTP_TRUSTED_PROXIES`: some values are now refused in production (v0.7.0, trusted-proxies-trust-all)
      Check the value of GOMBIT_HTTP_TRUSTED_PROXIES in production against trusted-proxies-trust-all (in .env.example and the deployment's environment). Security review: Production refuses ...
  Environment files read: .env (names only), .env.example (template). No value from them is shown.
```

`gombit upgrade --dry-run` shows the same section in its plan.

- **Categories.** Each change is one of:
  - **renamed:** a variable has a new name, and the old one is ignored;
  - **default changed:** what an unset variable means changed;
  - **action required:** a variable is removed, now required, or now refuses
    values it accepted;
  - **security review:** any of these, when the change is
    security-sensitive (the manifest's `security` area).
- **Clearly reported.** Every rename and removal is reported whether or
  not a file mentions the variable, since a deployment sets its
  environment elsewhere. The report says which files set it. Across a chain
  of renames (A to B, then B to C), a `.env` that sets A is named for every
  later name too (`set_as` in `--json`), so the advice reaches the final
  name, but it proves nothing about B or C.
- **Cannot pass silently.** The command exits 1 while any change needs
  action or review.
  - A default change needs no action only when files prove the variable
    set wherever the change applies, and files can prove little. The only
    file gombit loads is `.env`, from the directory a process runs in, so
    this one covers local development runs.
  - Test binaries run in their package directories, and a deployment sets
    its own environment, so no file proves test or production.
  - Other `.env.*` files (`.env.production`) are loaded by nothing in
    gombit, and templates are copied, not loaded.
  - All of these are reported as mentions, never as overrides.
- **The loader's grammar.** Files are read by the same `.env` grammar
  `config.Load` uses (`config.DotEnvKey`). So `export GOMBIT_X=1`, which the
  loader takes as a variable named `export GOMBIT_X`, does not set
  `GOMBIT_X` here either.
- **Never a secret.** It reads the app's `.env` and `.env.*` files, through
  a symlink. From a file that is not a template (`.env`, `.env.production`),
  only the names it sets are read, over its bytes, which are then zeroed.
  No value from any file is ever printed, in text or `--json`.
- **Unreadable files.** A file it cannot read is listed as not checked, and
  the report then needs review. A template it cannot read is not
  upgraded.
- **Templates upgraded safely.** `--write` upgrades the templates:
  `.env.example`, and any `.env.*` ending in `.example`, `.sample`,
  `.template` or `.dist`.
  - A renamed variable is renamed where a template sets it or documents it
    (a commented-out `# GOMBIT_X=`). If the template already has the new
    name, the old line is commented out instead.
  - A removed variable is commented out.
  - Each edit gets a note line above it saying which release and change
    caused it.
  - Every other byte is kept, line endings included. Each file is written
    atomically (a symlinked template through its link), and a second run
    changes nothing.
  - A file that is not a template is never written.
- **After the dependency upgrade.** Once `gombit upgrade --to` has moved
  `go.mod`, the app's version is the target. `--from vX.Y.Z` analyzes from
  the release it was on.

`upgrade.AnalyzeConfig` and `upgrade.WriteConfigTemplates` are the API.

## Generated contract artifacts

An upgrade can change what generation produces: the framework renders the
resource files and the TypeScript client, and the running app derives the
OpenAPI document. Run `gombit upgrade contract` after `gombit upgrade --to`
(the plan says so) to see which committed artifacts are stale:

```text
$ gombit upgrade contract --url http://127.0.0.1:8080/openapi.json
Generated contract artifacts:
  resources: stale: internal/book/dto.gen.go
      regenerate: gombit generate (or gombit upgrade contract --write)
  openapi:  unchecked: the resource files are stale, so the running app's document is not the one it will serve: regenerate them (gombit generate), rebuild and restart the app, then rerun
  client:   unchecked: the resource files are stale, ...
```

Each check is the one the existing drift gates make, so it agrees with them
(and they keep working unchanged across an upgrade):

| Artifact | Check | Regenerate |
| --- | --- | --- |
| `resources`: the generator-owned `*.gen.go` | rendered as `gombit generate --check` renders them, by a loader run inside the app's module, so with the framework version its `go.mod` requires | `gombit generate` |
| `openapi`: the committed `openapi.json` | compared semantically with the running app's (`--url`); unchecked without it, since the app derives it; absent when not committed (`gombit dev` writes it). A committed document that is not JSON is stale | `gombit openapi generate` |
| `client`: `frontend/src/api/generated` | regenerated as `gombit client generate` does, from the `--url` document or the committed one, and compared byte for byte (`gombit client check`'s comparison); unchecked without a document or without npx (openapi-typescript) | `gombit client generate` |

- **`--write` regenerates what is stale** with that same generation and
  writes only the stale files. The resources are rendered once, and only the
  stale `*.gen.go` are written. The document is written as `gombit openapi
  generate` writes it, and the client is regenerated from the document it
  was checked against. A current artifact is left alone.
- **The artifacts depend on each other.** The running app is built from the
  resource files and serves the document the client is generated from. So
  while the resources are not current (stale, just regenerated by
  `--write`, or unchecked), the checks against `--url` are unchecked, and
  `--write` does not write the document or the client from that app. Rebuild
  and restart it, then run the command again.
- **When the models don't compile** (a breaking upgrade not yet fixed), the
  resources are unchecked, with the loader's error, and the other checks
  still report. Any other failure, such as a write that fails partway, is an
  error naming the files already written.
- **Generator-owned files only.** `--write` never touches a resource's
  `hooks.go`, any other handwritten frontend file, or a file in the client
  directory that gombit did not generate (no generated banner). Such a file
  is reported instead, and the artifact stays stale.
- **Exit status.** The command exits 1 while anything is left stale. An
  artifact it could not check is reported with the reason and does not fail
  it, since without `--url` the document is always unchecked. `--strict`
  fails on it too: a CI gate runs `gombit upgrade contract --strict --url
  ...` against an app built from the committed resources, with npx
  available. `--spec` and `--out` select other paths, and `--json` prints
  the report (each check's `status`, its `files`, and the `reason`).

`contractcheck.Check` (package `upgrade/contractcheck`) is the API.

## Framework schema changes

The framework owns some of the app's tables: the auth tables (`users`,
`refresh_tokens`, `groups`, `permissions` and their join tables) and the
storage claims (`storage_claims`). The app's migrations create them from
its model registry (`database/migrations/models.json`), like its own
tables, so a release that changes one needs a migration that the app
generates, reviews and applies. The plan lists them, and the steps:

```text
Framework schema changes: 1
  - [migration required] `auth.RefreshToken` (`refresh_tokens`): altered (v0.9.0, refresh-device-name)
      The registry lists auth.RefreshToken, so the next migration carries its altered refresh_tokens.
Required migration steps (the upgrade is not complete until they are done):
  1. After the dependency change, review what the migration will do: gombit db plan.
  2. Generate it, with the same flags: gombit db makemigrations upgrade_framework_schema. If it reports a destructive or unsafe step, handle its data first and add --allow <its id>.
  3. Apply it in each environment with gombit db migrate, as a reviewed deploy step. gombit upgrade never applies a migration.
```

- **In the plan.** `gombit upgrade --dry-run` and `gombit upgrade --to`
  show the section; `--json` has it as `schema` (`findings`, `steps`), and
  `--to` adds `migration_required`.
- **Per app.** The registry (`--migrations` names another directory, as
  `gombit db --dir` does) says which framework models the app has:
  - a changed table it lists needs a migration;
  - one it does not list is not its table, so nothing is needed;
  - a new framework model the release requires is added to the app's
    models (its `AutoMigrate` in `internal/platform/database.go`) and
    generated with `--model`;
  - a model the framework drops (its own table removed) is forgotten with
    `--forget-model`, once its data is no longer needed;
  - a join table a model declares (`join: true`) changing, appearing or
    going away alters that model, as a column would: it never adds or
    removes the model.

  The steps carry one set of flags (`--dir` for another migration
  directory, `--model`, `--forget-model`) on `gombit db plan`, `gombit db
  makemigrations` and `gombit db migrate`, so the plan you review is the
  migration you generate, in the app's own directory. The analysis runs
  as part of the plan, before `--to` changes `go.mod`: a registry it cannot
  read stops the upgrade with nothing changed.

  A table that belongs to an optional feature (the storage claims, for
  file fields) is listed as nothing to migrate unless the app adopts it.
  An app with no registry has generated no migration yet, and the plan
  says so first; which framework tables it has is then its `AutoMigrate`'s
  call, so every change to one it may have counts as needing a migration.
  The steps come from each model's state at the end of the path, through
  every entry in order: a model added and removed again on the way needs
  nothing, one removed and added back stays (the migration carries the
  net change), and one added and then altered is added once.
- **Never silent.** No upgrade command generates or applies a migration.
  `gombit db plan` classifies what the migration does (a destructive or
  unsafe step needs `--allow`), and `gombit db migrate` applies it, in each
  environment, as a deploy step you review.
- **Declared, and detected.** A manifest change lists the tables it
  touches (`schema`, below), and
  [`upgrade/testdata/framework-schema.json`](../upgrade/testdata/framework-schema.json)
  holds the framework's tables as of a release, as GORM (and so the Atlas
  GORM provider) reads the models: columns (types, keys, defaults),
  indexes (class, type, where, option, and per column sort, collation,
  length or expression), foreign keys with their delete and update rules,
  and CHECK constraints. `TestFrameworkSchemaIsDeclared` fails when a
  table is new, changed or gone since then without a declaration, a
  declaration describes no change, a change that loses data (a table or
  column gone, a type changed other than widened) is not declared
  `destructive`, or an entry's `model` and `join` do not say whose table
  it is.
  `TestFrameworkModelsAreTheFrameworksTables` keeps the catalog
  (`upgrade.FrameworkModels`) equal to every `Models()` the framework
  exports, and `TestManifestSchemaModelsAreFrameworkModels` holds each
  entry to it (the parser does not, so a published entry stays valid once
  its model is removed). When a release is cut, `go test ./upgrade -run
  TestFrameworkSchemaIsDeclared -update` moves the snapshot to it.

`upgrade.AnalyzeSchema` is the API.

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

A change can list the **configuration variables** it touches (`config`, in
the `config` or `security` area), each with what happens to it:

```yaml
config:
  - {var: GOMBIT_OLD, change: renamed, to: GOMBIT_NEW}
  - {var: GOMBIT_GONE, change: removed}
  - {var: GOMBIT_X, change: default, from: "a", to: "b", environment: production}
  - {var: GOMBIT_Y, change: required, environment: production}
  - {var: GOMBIT_Z, change: refused, environment: production}
  - {var: GOMBIT_NEW, change: added}
```

`environment` limits a default change, a requirement or a refusal to one
runtime environment. The notes list each entry, and `gombit upgrade config`
checks the app against them.

An ordinary new variable needs no entry: an addition asks nothing of an
application. Declare it `added` when another entry needs it to exist, such
as a newly required production setting, or when the change is in the
`security` area, which makes the addition a security review:

```yaml
config:
  - {var: GOMBIT_NEW, change: added}
  - {var: GOMBIT_NEW, change: required, environment: production}
```

In a `security` change, an addition is a security review like any other
entry: the analysis reports it and needs action. A change that lists
variables beyond ordinary additions, or any variable in the `security`
area, is `manual`. Any of them can need the
developer: a default only some environments override, a security review. So
the notes and the plan never call such a change nothing to do, or something
the tooling applies, while the analysis asks for action.

Configuration changes are detected, not only declared:
- [`upgrade/testdata/config-variables.json`](../upgrade/testdata/config-variables.json)
  is `config.Variables()` as of a release (its `version`).
- `TestConfigVariablesAreDeclared` holds the code to that snapshot through
  what the manifest's later releases, `unreleased` included, declare. It
  fails until these are declared:
  - a variable the code stops reading (`removed` or `renamed`);
  - a default that moves, in development, test or production (`default`
    declarations, chaining from the snapshot's value to the code's across
    releases);
  - a variable that stops being secret (the test's `declassified` list,
    with the reason).
- The declarations are replayed on a copy of the snapshot. An `added`
  variable enters at its release; its first defaults need no declaration,
  but a later default change is checked from the one it declares. A rename moves
  the variable, defaults and secrecy included, to its new name, so a
  default or secrecy change that comes with a rename is still compared,
  and a default declaration can name the variable before or after it.
- The test also fails on any declaration that describes no real change: a
  `from` that was not the old value, a "change" where nothing moved, or a
  removal of a variable still read.
- A newly refused value (a validation rule) leaves no trace in
  `config.Variables()`, so no snapshot can detect it. Instead,
  `TestBreakingConfigChangesNameTheirVariables` fails on a breaking change
  that lists no variables when it is in the `config` area, or in the
  `security` area and names a `GOMBIT_` variable. Declare them `refused`.
- When a release is cut, `go test ./upgrade -run
  TestConfigVariablesAreDeclared -update` moves the snapshot to it. It
  refuses while `unreleased` still declares configuration changes.

A change in the `schema` area can list the **framework-owned tables** it
touches (`schema`), each with what happens to it (`added`, `altered` or
`removed`, and `destructive` when the migration drops or narrows data):

```yaml
schema:
  - {model: auth.RefreshToken, table: refresh_tokens, change: altered}
  - {model: auth.User, table: auth_user_groups, join: true, change: removed, destructive: true}
```

`model` is the framework package and type, as the app's registry names it
under the framework module, and `table` its own table, or, with `join:
true`, a join table it declares (a table that is not the own table of a
model this `gombit` has is read as a join table even without the flag; the
drift guard requires the flag anyway). In a `manual` change the entries are
migrations the app needs. An `informational` change may only add a new
model's own table, for an app that adopts the feature (`object-storage`
adds `storage_claims` that way): anything else changes a table an app may
have, so the parser rejects it: the change must be manual. A change that lists tables is
never `automatic`: the developer migrates.

A change of any kind can be `breaking`: an app that relies on documented (or
reasonably relied-on) behavior can stop building, starting or working across
it until it acts. A fix that only code matching undocumented output notices
is not breaking. Each change also names the `area` it
touches (`api`, `behavior`, `cli`, `client`, `config`, `dependency`,
`scaffold`, `schema`, `security`), a one-line `summary`, Markdown `details`,
and the issues or pull requests behind it. A change that deprecates
framework APIs lists them in `deprecates`, named as `gombit upgrade check`
names them; a `Deprecated: since vX.Y.Z` paragraph must name a release that
declares its API there ([the policy](compatibility.md#deprecations)).

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
release, and leave nothing under `unreleased`. A test checks that every
issue the changelog's Unreleased section links is declared in the
manifest, or exempt with a reason.
