package upgrade

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/mod/semver"
)

// The compatibility policy (docs/compatibility.md), as code: the release
// gate and the upgrade tooling apply it, rather than deciding per release.

// policyExceptions are the listed releases that are a smaller version
// change than their changes need, each with the reason it stands. A
// published release is never retagged: when one turns out to have broken
// something, the manifest says so truthfully (breaking: true), and the
// exception records it here instead of the policy refusing the truth.
var policyExceptions = map[string]string{
	"v0.7.1": "a patch release with breaking fixes, cut before the compatibility policy (v0.8.0)",
}

// Bump is the size of a version change.
type Bump int

const (
	// BumpPatch: X.Y.Z+1.
	BumpPatch Bump = iota
	// BumpMinor: X.Y+1.0.
	BumpMinor
	// BumpMajor: X+1.0.0.
	BumpMajor
)

func (b Bump) String() string {
	switch b {
	case BumpMinor:
		return "minor"
	case BumpMajor:
		return "major"
	}
	return "patch"
}

// bumpBetween is the size of the change from prev to next (both canonical
// releases, next above prev).
func bumpBetween(prev, next string) Bump {
	switch {
	case semver.Major(prev) != semver.Major(next):
		return BumpMajor
	case semver.MajorMinor(prev) != semver.MajorMinor(next):
		return BumpMinor
	}
	return BumpPatch
}

// RequiredBump is the smallest version change a release with changes may
// be, released after prev: a breaking change needs a minor release before
// 1.0 (where minors may break) and a major one from 1.0 on. The security
// exception: a breaking change in the security area may ship in any
// release, so a vulnerability is closed without waiting for the next
// minor or major. It also returns the breaking changes that set the bump.
func RequiredBump(prev string, changes []Change) (Bump, []string) {
	var breaking []string
	for _, c := range changes {
		if c.Breaking && c.Area != "security" {
			breaking = append(breaking, c.ID)
		}
	}
	switch {
	case len(breaking) == 0:
		return BumpPatch, nil
	case semver.Major(prev) == "v0":
		return BumpMinor, breaking
	}
	return BumpMajor, breaking
}

// CheckPolicy reports whether the listed release version is at least the
// version change its changes need (RequiredBump) after the latest stable
// release before it (a pre-release is not one: v0.9.0 after v0.9.0-rc.1 is
// still a minor release after v0.8.x). The release gate (CheckRelease)
// holds the release being cut to it, and a repository test every listed
// release but the exceptions. Parsing the manifest does not: a correction
// to a published release must stay representable.
func (m *Manifest) CheckPolicy(version string) error {
	i := slices.IndexFunc(m.Releases, func(r Release) bool { return r.Version == version })
	if i < 0 || version == Unreleased {
		return fmt.Errorf("upgrade: manifest: %s is not a release it lists", version)
	}
	prev := m.stableBefore(i)
	if prev == "" || policyExceptions[version] != "" {
		return nil // the first release judges nothing; an exception stands
	}
	need, breaking := RequiredBump(prev, m.Releases[i].Changes)
	if got := bumpBetween(prev, version); got < need {
		return fmt.Errorf("upgrade: manifest: release %s is a %s release after %s, but declares breaking changes (%s): the compatibility policy needs a %s release (%s); see docs/compatibility.md",
			version, got, prev, strings.Join(breaking, ", "), need, nextRelease(prev, need))
	}
	return nil
}

// stableBefore is the latest stable release listed before the i-th: what
// the policy judges that release against.
func (m *Manifest) stableBefore(i int) string {
	for j := i - 1; j >= 0; j-- {
		if v := m.Releases[j].Version; v != Unreleased && semver.Prerelease(v) == "" {
			return v
		}
	}
	return ""
}

// latestStable is the latest stable release the manifest lists: the one the
// next release follows, under the policy.
func (m *Manifest) latestStable() string { return m.stableBefore(len(m.Releases)) }

// nextRelease is the release a bump of prev makes.
func nextRelease(prev string, b Bump) string {
	var major, minor, patch int
	_, _ = fmt.Sscanf(prev, "v%d.%d.%d", &major, &minor, &patch)
	switch b {
	case BumpMajor:
		return fmt.Sprintf("v%d.0.0", major+1)
	case BumpMinor:
		return fmt.Sprintf("v%d.%d.0", major, minor+1)
	}
	return fmt.Sprintf("v%d.%d.%d", major, minor, patch+1)
}

// UpcomingReleases are the releases that can follow latest: its next
// patch, minor and major.
func UpcomingReleases(latest string) []string {
	return []string{nextRelease(latest, BumpPatch), nextRelease(latest, BumpMinor), nextRelease(latest, BumpMajor)}
}

// RemovableFrom is the first release that may remove an API deprecated in
// since: before 1.0, a deprecated API stays through the next minor release
// (deprecated in v0.8.x, still there in v0.9.x, removable from v0.10.0);
// from 1.0 on, until the next major. Across 1.0: v1.0.0, a major release,
// may remove what an earlier 0.y deprecated, and it follows a minor line
// that deprecates nothing, so every pre-1.0 deprecation still gets its
// next minor line. For a pre-1.0 since, the release is the one returned or
// v1.0.0, whichever comes first.
func RemovableFrom(since string) string {
	var major, minor, patch int
	_, _ = fmt.Sscanf(since, "v%d.%d.%d", &major, &minor, &patch)
	if major == 0 {
		return fmt.Sprintf("v0.%d.0", minor+2)
	}
	return fmt.Sprintf("v%d.0.0", major+1)
}

// deprecatedSince matches the release a "Deprecated:" paragraph says the
// API was deprecated in: "Deprecated: since v0.8.0, use ...".
var deprecatedSince = regexp.MustCompile(`\bDeprecated:\s+since\s+(v\d+\.\d+\.\d+)\b`)

// DeprecationSince is the release a "Deprecated:" paragraph names as the
// one that deprecated the API, or "" when it names none.
func DeprecationSince(paragraph string) string {
	if m := deprecatedSince.FindStringSubmatch(paragraph); m != nil && semver.IsValid(m[1]) {
		return m[1]
	}
	return ""
}
