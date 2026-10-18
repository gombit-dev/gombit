# Compatibility and deprecation policy

What a Gombit release may change, and how much notice an application gets.
The upgrade tooling follows this policy rather than deciding release by
release: where a rule can be checked, the code checks it
([`upgrade/policy.go`](../upgrade/policy.go)), and the rest is reviewed when
a release is prepared ([releasing.md](releasing.md)).

The policy governs releases from **v0.8.0** on. Earlier releases predate it:
v0.7.1, a patch release, has breaking fixes.

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

**Enforced:** the manifest refuses a release that is a smaller version change
than its changes need, and the release gate (`gombit upgrade notes
--release-gate`, run by the release workflow) names the release it needs. A
non-security breaking change in a patch release is refused before 1.0 (it
needs the next minor), as is one in a minor from 1.0 on (it needs the next
major). See `upgrade.RequiredBump`.

## The security exception

A change that closes a vulnerability may break in any release, a patch
included, rather than wait for the next minor (or major). Such a change is
in the manifest's `security` area and marked `breaking`, so the notes, the
plan and `gombit upgrade config` call it out as a security review. It never
passes silently. The exception covers only what closing the vulnerability
needs.

## Deprecations

An API is deprecated before it is removed, with the Go convention: its doc
comment gets a `Deprecated:` paragraph, which `go doc`, gopls and
pkg.go.dev show, and which `gombit upgrade check` reports at every use in an
application. The paragraph names:

- **the release that deprecated it**, as `Deprecated: since vX.Y.Z, …`. For
  a deprecation not released yet, that is the upcoming release: the next
  patch, minor or major of the newest release;
- **what to use instead.**

```go
// Deprecated: since v0.8.0, use ScopeEdit with StoredValues, which also
// keeps the row's stored text editable.
```

A repository test holds every deprecation in the framework to both.

**The deprecation window** is how long a deprecated API stays:

- **before 1.0**: through the next minor release. Deprecated in v0.8.x, it
  is still there in every v0.9.x, and may be removed from v0.10.0;
- **from 1.0**: until the next major. Deprecated in v1.4.0, it may be
  removed from v2.0.0.

`gombit upgrade check` shows the first release that may remove each API it
reports (`upgrade.RemovableFrom`). Removing an API is a breaking change,
declared in the manifest of the release that removes it.

## Upgrade rules and codemods

- **The compatibility manifest lists every release** from the first one it
  covers (v0.6.1), and never drops one. An application on any listed
  release has an upgrade path to the newest. A test checks every published
  tag is listed.
- **A codemod** (`gombit upgrade fix`) that rewrites the uses of a
  deprecated or changed API ships with the release that makes the change,
  which it records (`Codemod.Since`; a test requires it of every shipped
  codemod). It stays registered at least until `RemovableFrom(Since)`, so
  an application can still upgrade with it after it skips releases.
  Removing one is reviewed against that release when a release is
  prepared. Codemods are not removed in a patch release.
- **Upgrade tooling behavior** (the plan, the checks, their exit status) is
  itself covered by this policy. A change that breaks a script relying on
  it is declared in the `cli` area.

## Where it is checked

| Rule | Check |
| --- | --- |
| A release is at least the version change its changes need | `ParseManifest` (from v0.8.0), and the release gate |
| Breaking security changes may ship in any release | `RequiredBump` (the `security` area) |
| A deprecation names its release and a replacement | `TestFrameworkDeprecationsNameTheirRelease`, `TestFrameworkDeprecationsNameAReplacement` |
| The window an application sees | `gombit upgrade check` (`RemovableFrom`) |
| Every published release stays in the manifest | `TestEmbeddedManifestListsPublishedTags` |
| Every changelog entry is classified for upgrades | `TestChangelogIsDeclared` |
