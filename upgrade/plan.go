package upgrade

import (
	"fmt"
	"io"
	"strings"
)

// Plan is what upgrading an application to a target framework release
// takes, worked out without changing anything (gombit upgrade --dry-run).
type Plan struct {
	// Baseline is where the application stands (Detect).
	Baseline Baseline `json:"baseline"`
	// Current is the framework release the application builds against.
	Current string `json:"current"`
	// Target is the framework release the plan moves it to.
	Target string `json:"target"`
	// Dependencies are the go.mod directives about the framework module the
	// upgrade changes: its require, and a replace of it by the framework
	// module itself, which decides the build too. None when the application
	// already builds against Target.
	Dependencies []DependencyChange `json:"dependencies"`
	// Releases are the releases the upgrade moves across, in order.
	Releases []Release `json:"releases"`
	// Automatic are the automatic changes the application needs, each with
	// the action that applies it. An automatic change whose action reports
	// the application does not need it is left out.
	Automatic []PlannedAction `json:"automatic"`
	// Manual are the changes the developer must act on: every one the
	// manifest declares between Current and Target, with what to do.
	Manual []Change `json:"manual"`
	// Breaking are the breaking changes this application faces: every
	// breaking manual and informational change, and the breaking automatic
	// changes it needs. An automatic change its action reports the app does
	// not need is left out here too, as in Automatic, so the plan never
	// counts a change the app has already absorbed.
	Breaking []Change `json:"breaking"`
	// Informational are the changes worth knowing, with nothing to do.
	Informational []Change `json:"informational"`
}

// DependencyChange is a go.mod directive about a module that the upgrade
// changes.
type DependencyChange struct {
	Module string `json:"module"`
	// Directive is the directive that changes: "require", or "replace" (a
	// replace of the framework by the framework module itself).
	Directive string `json:"directive"`
	// From is the directive's version now.
	From string `json:"from"`
	// To is the version it moves to; empty for a replace that must be
	// dropped instead.
	To string `json:"to,omitempty"`
	// Note says why the directive must change, when the reason is not
	// plain.
	Note string `json:"note,omitempty"`
}

// PlannedAction is an automatic change an application needs, and the
// action that applies it.
type PlannedAction struct {
	Change Change `json:"change"`
	// Description says what the action does.
	Description string `json:"description"`
}

// PlanUpgrade works out the plan for upgrading the application in workDir
// to the framework release to (the newest release m lists when to is
// empty). It writes nothing: it reads the baseline, plans with
// m.PathFrom, which refuses with the reason an application that names no
// framework release (a workspace, a local checkout, a fork) or a version
// m does not list, and asks each automatic action whether the
// application needs it.
func PlanUpgrade(workDir string, m *Manifest, to string) (Plan, error) {
	b, err := Detect(workDir)
	if err != nil {
		return Plan{}, err
	}
	if to == "" {
		to = m.Latest()
	}
	releases, err := m.PathFrom(b.Framework, to)
	if err != nil {
		return Plan{}, err
	}
	p := Plan{
		Baseline:     b,
		Current:      b.Framework.Version,
		Target:       to,
		Dependencies: []DependencyChange{},
		Releases:     releases,
		Automatic:    []PlannedAction{},
	}
	if p.Current != p.Target {
		p.Dependencies = frameworkDirectives(b.Framework, p.Target)
	}
	c := Classify(releases)
	p.Manual, p.Informational = c.Manual, c.Informational
	notNeeded := map[string]bool{}
	for _, ch := range c.Automatic {
		a, ok := LookupAction(ch.Action)
		if !ok {
			// The manifest is validated against this framework's actions,
			// so this is a bug, not a state to plan around.
			return Plan{}, fmt.Errorf("upgrade: change %s names action %q, which this gombit does not implement", ch.ID, ch.Action)
		}
		if a.Check != nil {
			needed, err := a.Check(workDir, b)
			if err != nil {
				return Plan{}, fmt.Errorf("upgrade: check %s: %w", ch.ID, err)
			}
			if !needed {
				notNeeded[ch.ID] = true
				continue
			}
		}
		p.Automatic = append(p.Automatic, PlannedAction{Change: ch, Description: a.Description})
	}
	p.Breaking = []Change{}
	for _, ch := range c.Breaking {
		if !notNeeded[ch.ID] {
			p.Breaking = append(p.Breaking, ch)
		}
	}
	return p, nil
}

// frameworkDirectives are the go.mod directives about the framework that
// moving fw to target changes. The require moves. A replace by the
// framework module itself decides the build as well: one of every version
// keeps pinning it after the require moves, so it moves too; one of the
// required version only stops applying once the require moves, so it is
// dropped. (PathFrom has refused a local checkout and a fork already.)
func frameworkDirectives(fw Framework, target string) []DependencyChange {
	var changes []DependencyChange
	if fw.Required != target {
		changes = append(changes, DependencyChange{Module: FrameworkModulePath, Directive: "require", From: fw.Required, To: target})
	}
	if r := fw.Replace; r != nil && r.Path == FrameworkModulePath && !r.Local() && r.Version != target {
		if r.ForVersion == "" {
			changes = append(changes, DependencyChange{Module: FrameworkModulePath, Directive: "replace", From: r.Version, To: target,
				Note: "it replaces every version of the framework, so it keeps the build on " + r.Version + " until it moves (or is dropped)"})
		} else {
			changes = append(changes, DependencyChange{Module: FrameworkModulePath, Directive: "replace", From: r.Version,
				Note: "it replaces " + r.ForVersion + " only, so it stops applying once the require moves: drop it"})
		}
	}
	return changes
}

// Render writes the plan as text, for a terminal or a CI log.
func (p Plan) Render(w io.Writer) error {
	var b strings.Builder
	b.WriteString("Gombit upgrade plan (dry run: nothing is written)\n\n")
	fmt.Fprintf(&b, "Current: %s\n", p.Current)
	fmt.Fprintf(&b, "Target:  %s\n", p.Target)
	if p.Current == p.Target {
		fmt.Fprintf(&b, "\nAlready at %s: nothing to upgrade.\n", p.Target)
		_, err := io.WriteString(w, b.String())
		return err
	}
	b.WriteString("\nDependency changes:\n")
	for _, d := range p.Dependencies {
		if d.To == "" {
			fmt.Fprintf(&b, "  %s %s: drop the %s\n", d.Module, d.From, d.Directive)
		} else {
			fmt.Fprintf(&b, "  %s %s -> %s (%s)\n", d.Module, d.From, d.To, d.Directive)
		}
		if d.Note != "" {
			fmt.Fprintf(&b, "    (%s)\n", d.Note)
		}
	}
	fmt.Fprintf(&b, "\nAutomatic changes available: %d\n", len(p.Automatic))
	for _, a := range p.Automatic {
		fmt.Fprintf(&b, "  - %s: %s\n    (%s)\n", a.Change.ID, a.Change.Summary, a.Description)
	}
	fmt.Fprintf(&b, "\nManual actions: %d\n", len(p.Manual))
	for _, c := range p.Manual {
		renderPlannedChange(&b, c, true)
	}
	fmt.Fprintf(&b, "\nBreaking behavior changes: %d\n", len(p.Breaking))
	for _, c := range p.Breaking {
		fmt.Fprintf(&b, "  - %s (%s): %s\n", c.ID, c.Kind, c.Summary)
	}
	fmt.Fprintf(&b, "\nInformational: %d\n", len(p.Informational))
	for _, c := range p.Informational {
		renderPlannedChange(&b, c, false)
	}
	b.WriteString("\nNo files changed.\n")
	_, err := io.WriteString(w, b.String())
	return err
}

// renderPlannedChange writes one change of the plan: its id and summary,
// and, with details, what to do, indented under it.
func renderPlannedChange(b *strings.Builder, c Change, details bool) {
	marker := ""
	if c.Breaking {
		marker = " [breaking]"
	}
	fmt.Fprintf(b, "  - %s%s: %s\n", c.ID, marker, c.Summary)
	if !details {
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(c.Details), "\n") {
		if strings.TrimSpace(line) == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString("      " + line + "\n")
	}
}
