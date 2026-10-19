# Compatibility and deprecation policy

What a Gombit release may change, and how much notice an application gets.
The upgrade tooling follows this policy rather than deciding release by
release: where a rule can be checked, the code checks it
([`upgrade/policy.go`](../upgrade/policy.go)), and the rest is reviewed when
a release is prepared ([releasing.md](releasing.md)).

The policy governs releases from **v0.8.0** on. Earlier releases predate it:
v0.7.1, a patch release, has breaking fixes, and is the one recorded
exception (below).

## Versions

Gombit follows [Semantic Versioning](https://semver.org/). It is a Go
module, so every tag is a permanent, public version (`go get` resolves it
forever), and tags are cut deliberately, never moved.

| Release | Before 1.0 (`0.y.z`) | From 1.0 (`x.y.z`) |
| --- | --- | --- |
| Patch (`z`) | Fixes, never breaking (security exception below) | Fixes, never breaking (security exception) |
| Minor (`y`) | May break: read the upgrade notes | New features, never breaking (security exception) |
| Major (`x`) | — | May break |

**Breaking** means what the compatibility manifest means by it: an
application that relies on documented, or reasonably relied-on, behavior
can stop building, starting or working across the release until it acts.
A fix that only code matching undocumented output notices is not breaking.
Every breaking change is declared in the manifest (`breaking: true`), with
what to do, so `gombit upgrade --dry-run` and the release notes list it.

**Enforced when a release is cut:** the release gate (`gombit upgrade notes
--release-gate`, run by the release workflow) refuses a release that is a
smaller version change than its changes need, judged against the latest
stable release before it (a release candidate is not one), and names the
release it needs. A non-security breaking change in a patch release is
refused before 1.0 (it needs the next minor), as is one in a minor from 1.0
on (it needs the next major). See `upgrade.RequiredBump` and
`Manifest.CheckPolicy`.

**Published releases are recorded truthfully.** A tag is never moved, so
when a published release turns out to have broken something, its manifest
entry is corrected (`breaking: true`) anyway. Reading the manifest does not
apply the policy, so the correction never breaks the upgrade tooling. A
repository test holds every listed release to the policy, and a release
that breaks it stays only as an exception with its reason, in
[`upgrade/policy.go`](../upgrade/policy.go) (`policyExceptions`): today,
v0.7.1.

## The security exception

A change that closes a vulnerability may break in any release, a patch
included, rather than wait for the next minor (or major). Such a change is
in the manifest's `security` area and marked `breaking`. The upgrade notes
(and the release body) list it as **Breaking.**, tagged `security`, and
`gombit upgrade --dry-run` lists it among the breaking changes the
application faces (`[breaking]`; its `area` in `--json`), like any breaking
change. It never ships unannounced. The exception covers only what closing
the vulnerability needs.

## Deprecations

An API is deprecated before it is removed, with the Go convention: its doc
comment gets a `Deprecated:` paragraph, which `go doc`, gopls and
pkg.go.dev show, and which `gombit upgrade check` reports at every use in an
application. The paragraph names:

- **the release that deprecated it**, as `Deprecated: since vX.Y.Z, …`. For
  a deprecation not released yet, that is the upcoming release: the next
  patch, minor or major of the latest stable release. That release declares
  the deprecation in the manifest: a change lists the API in `deprecates`
  (under `unreleased` until it is cut, or in a release candidate of it), so
  the window cannot start before the deprecation was announced;
- **what to use instead.**

```go
// Deprecated: since v0.8.0, use ScopeEdit with StoredValues, which also
// keeps the row's stored text editable.
```

```yaml
      - id: keep-stored-zeros-deprecated
        kind: informational
        area: api
        summary: "`database.KeepStoredZeros` is deprecated: use ..."
        deprecates: [database.KeepStoredZeros]
```

A repository test holds every deprecation in the framework to all of this.

**The deprecation window** is how long a deprecated API stays:

- **before 1.0**: through the next minor release. Deprecated in v0.8.x, it
  is still there in every v0.9.x, and may be removed from v0.10.0;
- **from 1.0**: until the next major. Deprecated in v1.4.0, it may be
  removed from v2.0.0;
- **across 1.0**: v1.0.0 is a major release, and may remove an API any 0.y
  release deprecated. It follows a minor line that deprecates nothing: a
  0.y line that deprecates an API is followed by another 0.y before 1.0, and
  the release gate refuses v1.0.0 when the latest stable minor line, or a
  0.y pre-release after it, deprecates anything (a deprecation in v1.0.0's
  own pre-release is a 1.x one). So every pre-1.0 deprecation still gets its next
  minor line, and an API deprecated in v0.9.x may be removed from v0.11.0
  or from v1.0.0, whichever comes first.

`gombit upgrade check` shows the first release that may remove each API it
reports (`upgrade.RemovableFrom`; for a pre-1.0 deprecation, "or v1.0.0, if
it comes first", `upgrade.RemovableFromMajor`). `--json` carries both, as
`removable_from` and `removable_from_major`. Removing an API is a breaking change, declared in the
manifest of the release that removes it.

## Upgrade rules and codemods

- **The compatibility manifest lists every release** from the first one it
  covers (v0.6.1), and never drops one. An application on any listed
  release has an upgrade path to the newest. A test checks every published
  tag is listed.
- **A codemod** (`gombit upgrade fix`) that rewrites the uses of a
  deprecated or changed API ships with the release that makes the change:
  the release whose manifest change names it (`action: codemod:<id>`, an
  `automatic` change). The manifest is the one record of that release, and
  a test requires every shipped codemod to be named there. The manifest
  never drops a release, and an automatic change's action must exist, so a
  codemod stays registered as long as that change names it, which keeps it
  for an application that skips releases. Removing one, which waits at
  least until `RemovableFrom` of its release and never happens in a patch
  release, rewrites that old change: `automatic` becomes `manual`, the
  `action` goes, and `details` say how to make the rewrite by hand. The
  upgrade notes and plans from that old release then ask the developer
  instead of offering `gombit upgrade fix`.
- **Upgrade tooling behavior** (the plan, the checks, their exit status) is
  itself covered by this policy. A change that breaks a script relying on
  it is declared in the `cli` area.

## Where it is checked

| Rule | Check |
| --- | --- |
| A release is at least the version change its changes need (its own and every pre-release's since the last stable release) | the release gate (`Manifest.CheckRelease`, through `CheckPolicy`), and `TestEmbeddedManifestFollowsThePolicy` for every listed release, with `policyExceptions` |
| Breaking security changes may ship in any release | `RequiredBump` (the `security` area) |
| A deprecation names its release, which declares it (`deprecates`), and a replacement | `TestFrameworkDeprecationsNameTheirRelease`, `TestFrameworkDeprecationsNameAReplacement` |
| v1.0.0 follows a minor line that deprecates nothing | the release gate (`CheckRelease` for `v1.0.0`) |
| A shipped codemod is named by the manifest change of its release | `TestShippedCodemodsAreValid` |
| The window an application sees | `gombit upgrade check` (`RemovableFrom`) |
| Every published release stays in the manifest | `TestEmbeddedManifestListsPublishedTags` |
| Every changelog entry is classified for upgrades | `TestChangelogIsDeclared` |
