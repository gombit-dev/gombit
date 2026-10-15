package upgrade_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/upgrade"
)

// configSnapshot is the configuration surface (config.Variables) of a
// release the manifest lists: what the code's variables are compared with,
// through the configuration changes the releases after it declare.
const configSnapshot = "testdata/config-variables.json"

type configSnapshotFile struct {
	// Version is the release whose variables these are.
	Version   string            `json:"version"`
	Variables []config.Variable `json:"variables"`
}

// declassified are the variables that stopped being secret on purpose, each
// with the reason: a secret's value is never printed, so turning that off
// is reviewed here, not just declared.
var declassified = map[string]string{}

// TestConfigVariablesAreDeclared: the configuration surface moves from the
// snapshot's release to the code's only through what the manifest's later
// releases (unreleased included) declare. A variable the code stops
// reading needs a removal or a rename; a default that changes, in any
// environment, needs default declarations whose from is the snapshot's
// value and whose to is the code's (several releases chain, each from the
// last one's to). And every declaration must describe a real transition:
// one whose from is not the old value, or that changes nothing, fails, as
// does a removal of a variable still read. -update moves the snapshot to
// the newest release when it is cut (unreleased declares nothing left).
func TestConfigVariablesAreDeclared(t *testing.T) {
	data, err := os.ReadFile(configSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	var snap configSnapshotFile
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatal(err)
	}
	m, err := upgrade.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	after := -1
	for i, r := range m.Releases {
		if r.Version == snap.Version {
			after = i
		}
	}
	if after < 0 {
		t.Fatalf("%s is of release %q, which the manifest does not list", configSnapshot, snap.Version)
	}
	var declared []upgrade.ConfigChange
	unreleasedDeclares := false
	for _, r := range m.Releases[after+1:] {
		for _, c := range r.Changes {
			declared = append(declared, c.Config...)
			unreleasedDeclares = unreleasedDeclares || r.Version == upgrade.Unreleased && len(c.Config) > 0
		}
	}
	current := config.Variables()
	if problems := configProblems(snap.Variables, current, declared); len(problems) > 0 {
		t.Fatalf("the configuration changes since %s and the manifest's declarations after it (upgrade/manifest.yaml, a change's config list) disagree:\n  %s", snap.Version, strings.Join(problems, "\n  "))
	}
	if !*update || snap.Version == m.Latest() {
		return
	}
	if unreleasedDeclares {
		t.Fatalf("not moving %s to %s: the unreleased entry declares configuration changes, so the code is not that release's; update it when the release is cut", configSnapshot, m.Latest())
	}
	out, err := json.MarshalIndent(configSnapshotFile{Version: m.Latest(), Variables: current}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.FromSlash(configSnapshot), append(out, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

var configEnvs = []string{"development", "test", "production"}

func envDefault(v config.Variable, env string) string {
	switch env {
	case "test":
		return v.TestDefault
	case "production":
		return v.ProductionDefault
	}
	return v.Default
}

// configProblems are where moving from the old variables to the new ones
// and the declarations (in release order) disagree: a change not declared,
// or a declaration that is not a change.
func configProblems(old, current []config.Variable, declared []upgrade.ConfigChange) []string {
	was, now := map[string]config.Variable{}, map[string]config.Variable{}
	for _, v := range old {
		was[v.Name] = v
	}
	for _, v := range current {
		now[v.Name] = v
	}
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	// What changed must be declared.
	for _, o := range old {
		n, still := now[o.Name]
		if !still {
			found := false
			for _, cc := range declared {
				found = found || cc.Var == o.Name && (cc.Change == upgrade.ConfigRemoved || cc.Change == upgrade.ConfigRenamed)
			}
			if !found {
				add("%s is no longer read: declare it removed, or renamed (to its new name)", o.Name)
			}
			continue
		}
		if o.Secret && !n.Secret && declassified[o.Name] == "" {
			add("%s is no longer secret: the tooling would print its value; if that is right, add it to declassified with the reason", o.Name)
		}
	}

	// Default changes: per variable and environment, the declared
	// transitions must chain from the old default to the new one, and
	// there must be some exactly when the default moved.
	type key struct{ name, env string }
	chains := map[key][]upgrade.ConfigChange{}
	for _, cc := range declared {
		if cc.Change != upgrade.ConfigDefault {
			continue
		}
		for _, env := range configEnvs {
			if cc.Environment == "" || cc.Environment == env {
				chains[key{cc.Var, env}] = append(chains[key{cc.Var, env}], cc)
			}
		}
	}
	for _, o := range old {
		n, still := now[o.Name]
		if !still {
			continue
		}
		for _, env := range configEnvs {
			from, to := envDefault(o, env), envDefault(n, env)
			chain := chains[key{o.Name, env}]
			if len(chain) == 0 {
				if from != to {
					add("%s's %s default changed from %q to %q: declare it (change: default, from, to, environment: %s or none)", o.Name, env, from, to, env)
				}
				continue
			}
			at := from
			for _, cc := range chain {
				if cc.From != at {
					add("%s's %s default is declared to change from %q, but it was %q", o.Name, env, cc.From, at)
				}
				at = cc.To
			}
			if at != to {
				add("%s's %s default is declared to end at %q, but the code's is %q", o.Name, env, at, to)
			}
		}
	}

	// Every declaration must be about a variable in the right state.
	for _, cc := range declared {
		_, before := was[cc.Var]
		_, read := now[cc.Var]
		switch cc.Change {
		case upgrade.ConfigRemoved, upgrade.ConfigRenamed:
			if !before {
				add("%s is declared %s, but the snapshot's release does not read it", cc.Var, cc.Change)
			}
			if read {
				add("%s is declared %s but the code still reads it", cc.Var, cc.Change)
			}
			if _, new := now[cc.To]; cc.Change == upgrade.ConfigRenamed && !new {
				add("%s is declared renamed %s, which the code does not read", cc.Var, cc.To)
			}
		case upgrade.ConfigDefault:
			if !before || !read {
				add("%s's default change is declared, but the variable is not read both before and now (a new variable needs no declaration)", cc.Var)
			}
		case upgrade.ConfigRequired, upgrade.ConfigRefused:
			if !read {
				add("%s is declared %s but the code does not read it", cc.Var, cc.Change)
			}
		}
	}
	return problems
}

// TestConfigProblems: the rule itself, on made-up surfaces.
func TestConfigProblems(t *testing.T) {
	v := func(name, dev, prod string) config.Variable {
		return config.Variable{Name: name, Default: dev, TestDefault: dev, ProductionDefault: prod}
	}
	secret := func(v config.Variable) config.Variable { v.Secret = true; return v }
	testOnly := v("GOMBIT_T", "a", "a")
	testOnly.TestDefault = "b"
	old := []config.Variable{v("GOMBIT_A", "1", "1"), v("GOMBIT_B", "x", "x"), v("GOMBIT_C", "on", "off"), secret(v("GOMBIT_S", "", "")), v("GOMBIT_T", "a", "a"), v("GOMBIT_U", "on", "on")}
	current := []config.Variable{v("GOMBIT_A2", "1", "1"), v("GOMBIT_C", "on", "on"), v("GOMBIT_S", "", ""), testOnly, v("GOMBIT_U", "on", "on"), v("GOMBIT_NEW", "z", "z")}
	all := []upgrade.ConfigChange{
		{Var: "GOMBIT_A", Change: upgrade.ConfigRenamed, To: "GOMBIT_A2"},
		{Var: "GOMBIT_B", Change: upgrade.ConfigRemoved},
		{Var: "GOMBIT_C", Change: upgrade.ConfigDefault, From: "off", To: "on", Environment: "production"},
		{Var: "GOMBIT_T", Change: upgrade.ConfigDefault, From: "a", To: "b", Environment: "test"},
	}
	with := func(extra ...upgrade.ConfigChange) []upgrade.ConfigChange {
		return append(append([]upgrade.ConfigChange{}, all...), extra...)
	}
	for name, tc := range map[string]struct {
		declared []upgrade.ConfigChange
		want     []string
	}{
		"nothing declared": {nil, []string{"GOMBIT_A is no longer read", "GOMBIT_B is no longer read", "GOMBIT_S is no longer secret", `GOMBIT_C's production default changed from "off" to "on"`, `GOMBIT_T's test default changed from "a" to "b"`}},
		"all declared":     {all, []string{"GOMBIT_S is no longer secret"}},
		// The reviewer's case: no change happened, a transition is declared.
		"fabricated transition": {with(upgrade.ConfigChange{Var: "GOMBIT_U", Change: upgrade.ConfigDefault, From: "off", To: "on", Environment: "production"}),
			[]string{"GOMBIT_S is no longer secret", `GOMBIT_U's production default is declared to change from "off", but it was "on"`}},
		"wrong from": {[]upgrade.ConfigChange{all[0], all[1], all[3], {Var: "GOMBIT_C", Change: upgrade.ConfigDefault, From: "maybe", To: "on", Environment: "production"}},
			[]string{"GOMBIT_S is no longer secret", `GOMBIT_C's production default is declared to change from "maybe", but it was "off"`}},
		"wrong to": {[]upgrade.ConfigChange{all[0], all[1], all[3], {Var: "GOMBIT_C", Change: upgrade.ConfigDefault, From: "off", To: "auto", Environment: "production"}},
			[]string{"GOMBIT_S is no longer secret", `GOMBIT_C's production default is declared to end at "auto", but the code's is "on"`}},
		// Unscoped: it claims every environment moved, but development
		// and test were already "on".
		"unscoped where one environment moved": {[]upgrade.ConfigChange{all[0], all[1], all[3], {Var: "GOMBIT_C", Change: upgrade.ConfigDefault, From: "off", To: "on"}},
			[]string{"GOMBIT_S is no longer secret", `GOMBIT_C's development default is declared to change from "off", but it was "on"`, `GOMBIT_C's test default is declared to change from "off", but it was "on"`}},
		"chained across releases": {[]upgrade.ConfigChange{all[0], all[1], all[3],
			{Var: "GOMBIT_C", Change: upgrade.ConfigDefault, From: "off", To: "auto", Environment: "production"},
			{Var: "GOMBIT_C", Change: upgrade.ConfigDefault, From: "auto", To: "on", Environment: "production"},
		}, []string{"GOMBIT_S is no longer secret"}},
		"contradicted": {with(
			upgrade.ConfigChange{Var: "GOMBIT_U", Change: upgrade.ConfigRemoved},
			upgrade.ConfigChange{Var: "GOMBIT_ZZ", Change: upgrade.ConfigRenamed, To: "GOMBIT_Z"},
			upgrade.ConfigChange{Var: "GOMBIT_NEW", Change: upgrade.ConfigDefault, From: "y", To: "z"},
			upgrade.ConfigChange{Var: "GOMBIT_Q", Change: upgrade.ConfigRequired},
		), []string{
			"GOMBIT_S is no longer secret",
			`GOMBIT_U is declared removed but the code still reads it`,
			`GOMBIT_ZZ is declared renamed, but the snapshot's release does not read it`,
			`GOMBIT_ZZ is declared renamed GOMBIT_Z, which the code does not read`,
			`GOMBIT_NEW's default change is declared, but the variable is not read both before and now`,
			`GOMBIT_Q is declared required but the code does not read it`,
		}},
	} {
		got := configProblems(old, current, tc.declared)
		if len(got) != len(tc.want) {
			t.Errorf("%s: problems %q, want %d matching %q", name, got, len(tc.want), tc.want)
			continue
		}
		for i, w := range tc.want {
			if !strings.Contains(got[i], w) {
				t.Errorf("%s: problem %d = %q, want it to mention %q", name, i, got[i], w)
			}
		}
	}
}
