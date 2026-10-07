package upgrade

import (
	"fmt"
	"io"
	"slices"
	"strings"
)

// RenderNotes writes the upgrade notes of releases (of m), in the order
// given, as Markdown: per release, its changes grouped by kind (manual
// first), breaking ones marked. This is what docs/upgrade-notes.md and the
// GitHub release body carry, rendered from the manifest rather than written
// separately.
func (m *Manifest) RenderNotes(w io.Writer, releases []Release) error {
	var b strings.Builder
	for i, r := range releases {
		if i > 0 {
			b.WriteString("\n")
		}
		if r.Version == m.Releases[0].Version {
			// The baseline: what moving to it took is not described here.
			fmt.Fprintf(&b, "## %s\n\nThe first release the compatibility manifest covers: upgrades are\ndescribed from it. For its own changes, and earlier releases, see the\n[changelog](https://github.com/gombit-dev/gombit/blob/main/CHANGELOG.md).\n", r.Version)
			continue
		}
		renderRelease(&b, r)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

var kindHeadings = map[Kind]string{
	KindManual:        "Manual: action required",
	KindAutomatic:     "Automatic: applied by the upgrade tooling",
	KindInformational: "Informational",
}

func renderRelease(b *strings.Builder, r Release) {
	title := r.Version
	if title == Unreleased {
		title = "Unreleased"
	}
	fmt.Fprintf(b, "## %s\n\n", title)
	if len(r.Changes) == 0 {
		b.WriteString("No upgrade-relevant changes.\n")
		return
	}
	var sections []string
	for _, kind := range kindOrder {
		var items []string
		loose := false
		for _, c := range r.Changes {
			if c.Kind == kind {
				items = append(items, renderChange(c))
				loose = loose || strings.TrimSpace(c.Details) != ""
			}
		}
		if len(items) == 0 {
			continue
		}
		sep := ""
		if loose {
			sep = "\n" // a blank line between items that carry details
		}
		sections = append(sections, fmt.Sprintf("### %s\n\n%s", kindHeadings[kind], strings.Join(items, sep)))
	}
	b.WriteString(strings.Join(sections, "\n"))
}

// renderChange is one list item, ending in a newline.
func renderChange(c Change) string {
	var b strings.Builder
	b.WriteString("- ")
	if c.Breaking {
		b.WriteString("**Breaking.** ")
	}
	b.WriteString(strings.TrimSpace(c.Summary))
	tags := []string{"`" + c.ID + "`", string(c.Area)}
	if c.Action != "" {
		tags = append(tags, "action `"+c.Action+"`")
	}
	for _, ref := range c.Refs {
		tags = append(tags, fmt.Sprintf("[#%d](https://github.com/gombit-dev/gombit/issues/%d)", ref, ref))
	}
	fmt.Fprintf(&b, " (%s)\n", strings.Join(tags, ", "))
	if details := strings.TrimSpace(c.Details); details != "" {
		for _, line := range strings.Split(details, "\n") {
			b.WriteString("\n")
			if strings.TrimSpace(line) != "" {
				b.WriteString("  " + line)
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// RenderNotesDoc writes docs/upgrade-notes.md: the notes of every release in
// m, newest first.
func RenderNotesDoc(w io.Writer, m *Manifest) error {
	releases := slices.Clone(m.Releases)
	slices.Reverse(releases)
	header := `# Upgrade notes

<!-- Generated from upgrade/manifest.yaml by
     go test ./upgrade -run TestUpgradeNotesDoc -update
     Do not edit by hand: change the manifest. -->

What each release asks of an application moving to it, newest first. This
page, the release notes, and the upgrade tooling all come from the same
compatibility manifest (see [upgrade.md](upgrade.md)).

`
	if _, err := io.WriteString(w, header); err != nil {
		return err
	}
	return m.RenderNotes(w, releases)
}
