package upgrade

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/gombit-dev/gombit/internal/frameworkmod"
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
	// Config is what the upgrade's configuration changes mean for the
	// application's environment files (AnalyzeConfig): names only, never
	// a value.
	Config ConfigReport `json:"config"`
	// Contract are the changes on the path to the generated contract (the
	// OpenAPI document and TypeScript client: area client). Whatever they
	// are, the framework renders the generated artifacts, so after the
	// dependency change gombit upgrade contract checks them.
	Contract []Change `json:"contract"`
}

// DependencyChange is a go.mod directive about a module that the upgrade
// changes.
type DependencyChange struct {
	Module string `json:"module"`
	// Directive is the directive that changes: "require" or "replace".
	Directive string `json:"directive"`
	// ForVersion is, for a replace, the version it replaces (its left
	// side); empty for a replace of every version.
	ForVersion string `json:"for_version,omitempty"`
	// From is the directive's right side now: the required version, or the
	// replacement as go.mod writes it ("path version", or a directory).
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
		// The baseline keeps only the replace active today; moving the
		// require needs every one (frameworkDirectives).
		d, err := frameworkmod.Read(workDir)
		if err != nil {
			return Plan{}, fmt.Errorf("upgrade: %w", err)
		}
		p.Dependencies = frameworkDirectives(d, p.Target)
	}
	c := Classify(releases)
	p.Manual, p.Informational = c.Manual, c.Informational
	p.Contract = []Change{}
	for _, r := range releases {
		for _, ch := range r.Changes {
			if ch.Area == "client" {
				p.Contract = append(p.Contract, ch)
			}
		}
	}
	if p.Config, err = AnalyzeConfig(workDir, releases); err != nil {
		return Plan{}, err
	}
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
// moving d's app to target changes, so that the upgraded build is the
// target release. The require moves. Then every replace of the framework,
// active or not, is resolved at the target as the go command would, once
// the require has moved: a replace that would decide the build there and is
// not the target release itself is moved (a replace of every version by
// the framework module, which would keep pinning the build) or dropped
// (anything else: a replace of the target version itself, a local
// checkout, a fork), and the set is resolved again, since dropping an
// exact replace can expose one of every version it shadowed. Last, a
// replace of the current required version only, active today, stops
// applying once the require moves, and is dropped as dead.
func frameworkDirectives(d frameworkmod.Declaration, target string) []DependencyChange {
	var changes []DependencyChange
	if d.Required != target {
		changes = append(changes, DependencyChange{Module: FrameworkModulePath, Directive: "require", From: d.Required, To: target})
	}
	replaces := slices.Clone(d.Replaces)
	reported := map[frameworkmod.Replacement]bool{}
	for {
		r := frameworkmod.Resolve(replaces, target)
		if r == nil || (r.Path == FrameworkModulePath && r.Version == target) {
			break
		}
		was := *r
		reported[was] = true
		if was.ForVersion == "" && was.Path == FrameworkModulePath && !was.Local() {
			changes = append(changes, DependencyChange{Module: FrameworkModulePath, Directive: "replace", From: was.String(), To: target,
				Note: "it replaces every version of the framework, so it would keep the build on " + was.Version + " (move it, or drop it)"})
			r.Version = target
			continue
		}
		for_ := "every version of the framework"
		if was.ForVersion != "" {
			for_ = was.ForVersion
		}
		changes = append(changes, DependencyChange{Module: FrameworkModulePath, Directive: "replace", ForVersion: was.ForVersion, From: was.String(),
			Note: "it replaces " + for_ + ", so once the require is at " + target + " the build would use " + was.String() + ", not the release: drop it"})
		replaces = slices.DeleteFunc(replaces, func(x frameworkmod.Replacement) bool { return x == was })
	}
	if a := frameworkmod.Resolve(d.Replaces, d.Required); a != nil && a.ForVersion == d.Required && d.Required != target && !reported[*a] {
		changes = append(changes, DependencyChange{Module: FrameworkModulePath, Directive: "replace", ForVersion: a.ForVersion, From: a.String(),
			Note: "it replaces " + a.ForVersion + " only, so it stops applying once the require moves: drop it"})
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
	p.renderDependencies(&b)
	p.renderToDo(&b)
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

// renderDependencies writes the plan's dependency changes, one a line.
func (p Plan) renderDependencies(b *strings.Builder) {
	for _, d := range p.Dependencies {
		switch {
		case d.Directive == "require":
			fmt.Fprintf(b, "  %s %s -> %s (require)\n", d.Module, d.From, d.To)
		case d.To == "":
			fmt.Fprintf(b, "  replace %s => %s: drop it\n", strings.TrimSpace(d.Module+" "+d.ForVersion), d.From)
		default:
			fmt.Fprintf(b, "  replace %s => %s -> %s\n", strings.TrimSpace(d.Module+" "+d.ForVersion), d.From, d.To)
		}
		if d.Note != "" {
			fmt.Fprintf(b, "    (%s)\n", d.Note)
		}
	}
}

// renderToDo writes the plan's changes beyond its dependencies: the
// automatic, manual, breaking and informational ones.
func (p Plan) renderToDo(b *strings.Builder) {
	fmt.Fprintf(b, "\nAutomatic changes available: %d\n", len(p.Automatic))
	for _, a := range p.Automatic {
		fmt.Fprintf(b, "  - %s: %s\n    (%s)\n", a.Change.ID, a.Change.Summary, a.Description)
	}
	fmt.Fprintf(b, "\nManual actions: %d\n", len(p.Manual))
	for _, c := range p.Manual {
		renderPlannedChange(b, c, true)
	}
	fmt.Fprintf(b, "\nBreaking behavior changes: %d\n", len(p.Breaking))
	for _, c := range p.Breaking {
		fmt.Fprintf(b, "  - %s (%s): %s\n", c.ID, c.Kind, c.Summary)
	}
	fmt.Fprintf(b, "\nInformational: %d\n", len(p.Informational))
	for _, c := range p.Informational {
		renderPlannedChange(b, c, false)
	}
	b.WriteString("\n")
	p.Config.render(b)
	b.WriteString("\nGenerated contract artifacts (resource *.gen.go, OpenAPI document, TypeScript client):\n")
	b.WriteString("  After the dependency change, check them: gombit upgrade contract [--url <the running app>/openapi.json]; --write regenerates them.\n")
	if len(p.Contract) > 0 {
		var ids []string
		for _, c := range p.Contract {
			ids = append(ids, c.ID)
		}
		fmt.Fprintf(b, "  Contract changes on this path: %s.\n", strings.Join(ids, ", "))
	}
}
