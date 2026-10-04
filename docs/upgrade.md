# Upgrading an application

Gombit keeps reusable behavior in the versioned framework module, and
generates application code that you own. Moving an app to a new framework
version is then mostly a module upgrade, plus whatever the release says your
own code, configuration, generated client or database needs. The `gombit
upgrade` tooling works that out for you, and proposes changes for review: it
never rewrites code you own without an explicit write action.

This page grows with the UPGRADE-0 epic. Today it covers the **baseline**:
where an application stands.

## The baseline

An application's baseline is two facts:

| Fact | Where it comes from |
| --- | --- |
| The **framework version** it builds against | `go.mod`, read offline: the `require github.com/gombit-dev/gombit` version, or the version of a `replace` of it by the framework module (when both an exact-version and an every-version replace exist, the exact one applies, as in Go). It is never copied anywhere else, and `gombit contract app` reads it the same way. |
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
