package upgrade_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
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

// configProblems are where the declarations (in release order) and the
// move from the old variables to the current ones disagree. The
// declarations are replayed on a copy of the old catalog: a rename moves a
// variable's descriptor (defaults, secrecy) to its new name, a removal
// drops it, and a default change moves one default, from the value it
// has at that point. Each must find its variable in the state it claims.
// The replayed catalog must then be the code's: a variable it still has
// that the code does not read, a default it has otherwise, or a secret
// the code no longer treats as one is a change nothing declares.
func configProblems(old, current []config.Variable, declared []upgrade.ConfigChange) []string {
	now := map[string]config.Variable{}
	for _, v := range current {
		now[v.Name] = v
	}
	replay := map[string]config.Variable{}
	for _, v := range old {
		replay[v.Name] = v
	}
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	gone := map[string]bool{} // removed or renamed away
	for _, cc := range declared {
		v, has := replay[cc.Var]
		switch cc.Change {
		case upgrade.ConfigRenamed, upgrade.ConfigRemoved:
			if !has {
				add("%s is declared %s, but it is not read at that point", cc.Var, cc.Change)
				continue
			}
			delete(replay, cc.Var)
			gone[cc.Var] = true
			if cc.Change == upgrade.ConfigRenamed {
				if _, taken := replay[cc.To]; taken {
					add("%s is declared renamed %s, which is already read at that point", cc.Var, cc.To)
				}
				v.Name = cc.To
				replay[cc.To] = v
			}
		case upgrade.ConfigDefault:
			if !has {
				add("%s's default change is declared, but it is not read at that point (a new variable needs no declaration)", cc.Var)
				continue
			}
			for _, env := range configEnvs {
				if cc.Environment != "" && cc.Environment != env {
					continue
				}
				if was := envDefault(v, env); was != cc.From {
					add("%s's %s default is declared to change from %q, but it was %q", cc.Var, env, cc.From, was)
				}
				setEnvDefault(&v, env, cc.To)
			}
			replay[cc.Var] = v
		case upgrade.ConfigRequired, upgrade.ConfigRefused:
			if _, read := now[cc.Var]; !read {
				add("%s is declared %s but the code does not read it", cc.Var, cc.Change)
			}
		}
	}
	for _, name := range sortedNames(replay) {
		v := replay[name]
		n, read := now[name]
		if !read {
			if gone[name] {
				continue
			}
			add("%s is no longer read: declare it removed, or renamed (to its new name)", name)
			continue
		}
		for _, env := range configEnvs {
			if from, to := envDefault(v, env), envDefault(n, env); from != to {
				add("%s's %s default changed from %q to %q: declare it (change: default, from, to, environment: %s or none)", name, env, from, to, env)
			}
		}
		if v.Secret && !n.Secret && declassified[name] == "" {
			add("%s is no longer secret: the tooling would print its value; if that is right, add it to declassified with the reason", name)
		}
	}
	for name := range gone {
		if _, back := replay[name]; !back {
			if _, read := now[name]; read {
				add("%s is declared removed or renamed but the code still reads it", name)
			}
		}
	}
	for _, cc := range declared {
		if _, read := now[cc.To]; cc.Change == upgrade.ConfigRenamed && !read {
			add("%s is declared renamed %s, which the code does not read", cc.Var, cc.To)
		}
	}
	return problems
}

func setEnvDefault(v *config.Variable, env, value string) {
	switch env {
	case "test":
		v.TestDefault = value
	case "production":
		v.ProductionDefault = value
	default:
		v.Default = value
	}
}

func sortedNames(m map[string]config.Variable) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TestConfigProblems: the rule itself, on made-up surfaces. Problems are
// matched as a set (each wanted text in one problem, and no others).
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
	without := func(i int, extra ...upgrade.ConfigChange) []upgrade.ConfigChange {
		out := append(append([]upgrade.ConfigChange{}, all[:i]...), all[i+1:]...)
		return append(out, extra...)
	}
	with := func(extra ...upgrade.ConfigChange) []upgrade.ConfigChange {
		return append(append([]upgrade.ConfigChange{}, all...), extra...)
	}
	const secretS = "GOMBIT_S is no longer secret" // #nosec G101 -- a problem message, not a credential
	for name, tc := range map[string]struct {
		old, current []config.Variable
		declared     []upgrade.ConfigChange
		want         []string
	}{
		"nothing declared": {old, current, nil, []string{"GOMBIT_A is no longer read", "GOMBIT_B is no longer read", secretS, `GOMBIT_C's production default changed from "off" to "on"`, `GOMBIT_T's test default changed from "a" to "b"`}},
		"all declared":     {old, current, all, []string{secretS}},
		"fabricated transition": {old, current, with(upgrade.ConfigChange{Var: "GOMBIT_U", Change: upgrade.ConfigDefault, From: "off", To: "on", Environment: "production"}),
			[]string{secretS, `GOMBIT_U's production default is declared to change from "off", but it was "on"`}},
		"wrong from": {old, current, without(2, upgrade.ConfigChange{Var: "GOMBIT_C", Change: upgrade.ConfigDefault, From: "maybe", To: "on", Environment: "production"}),
			[]string{secretS, `GOMBIT_C's production default is declared to change from "maybe", but it was "off"`}},
		"wrong to": {old, current, without(2, upgrade.ConfigChange{Var: "GOMBIT_C", Change: upgrade.ConfigDefault, From: "off", To: "auto", Environment: "production"}),
			[]string{secretS, `GOMBIT_C's production default changed from "auto" to "on"`}},
		"unscoped where one environment moved": {old, current, without(2, upgrade.ConfigChange{Var: "GOMBIT_C", Change: upgrade.ConfigDefault, From: "off", To: "on"}),
			[]string{secretS, `GOMBIT_C's development default is declared to change from "off", but it was "on"`, `GOMBIT_C's test default is declared to change from "off", but it was "on"`}},
		"chained across releases": {old, current, without(2,
			upgrade.ConfigChange{Var: "GOMBIT_C", Change: upgrade.ConfigDefault, From: "off", To: "auto", Environment: "production"},
			upgrade.ConfigChange{Var: "GOMBIT_C", Change: upgrade.ConfigDefault, From: "auto", To: "on", Environment: "production"},
		), []string{secretS}},
		"contradicted": {old, current, with(
			upgrade.ConfigChange{Var: "GOMBIT_U", Change: upgrade.ConfigRemoved},
			upgrade.ConfigChange{Var: "GOMBIT_ZZ", Change: upgrade.ConfigRenamed, To: "GOMBIT_Z"},
			upgrade.ConfigChange{Var: "GOMBIT_NEW", Change: upgrade.ConfigDefault, From: "y", To: "z"},
			upgrade.ConfigChange{Var: "GOMBIT_Q", Change: upgrade.ConfigRequired},
		), []string{
			secretS,
			"GOMBIT_U is declared removed or renamed but the code still reads it",
			"GOMBIT_ZZ is declared renamed, but it is not read at that point",
			"GOMBIT_ZZ is declared renamed GOMBIT_Z, which the code does not read",
			"GOMBIT_NEW's default change is declared, but it is not read at that point",
			"GOMBIT_Q is declared required but the code does not read it",
		}},
		// A rename keeps the variable's identity: what else changes with it
		// is still compared, and can be declared.
		"rename with an undeclared default and declassification": {
			[]config.Variable{secret(v("GOMBIT_OLD", "off", "off"))}, []config.Variable{v("GOMBIT_NEW", "on", "on")},
			[]upgrade.ConfigChange{{Var: "GOMBIT_OLD", Change: upgrade.ConfigRenamed, To: "GOMBIT_NEW"}},
			[]string{`GOMBIT_NEW's development default changed from "off" to "on"`, `GOMBIT_NEW's test default changed from "off" to "on"`, `GOMBIT_NEW's production default changed from "off" to "on"`, "GOMBIT_NEW is no longer secret"},
		},
		"rename then a default on the new name": {
			[]config.Variable{v("GOMBIT_OLD", "off", "off")}, []config.Variable{v("GOMBIT_NEW", "on", "on")},
			[]upgrade.ConfigChange{{Var: "GOMBIT_OLD", Change: upgrade.ConfigRenamed, To: "GOMBIT_NEW"}, {Var: "GOMBIT_NEW", Change: upgrade.ConfigDefault, From: "off", To: "on"}},
			nil,
		},
		"a default on the old name, then the rename": {
			[]config.Variable{v("GOMBIT_OLD", "off", "off")}, []config.Variable{v("GOMBIT_NEW", "on", "on")},
			[]upgrade.ConfigChange{{Var: "GOMBIT_OLD", Change: upgrade.ConfigDefault, From: "off", To: "on"}, {Var: "GOMBIT_OLD", Change: upgrade.ConfigRenamed, To: "GOMBIT_NEW"}},
			nil,
		},
		"a rename onto a variable already read": {
			[]config.Variable{v("GOMBIT_OLD", "1", "1"), v("GOMBIT_NEW", "1", "1")}, []config.Variable{v("GOMBIT_NEW", "1", "1")},
			[]upgrade.ConfigChange{{Var: "GOMBIT_OLD", Change: upgrade.ConfigRenamed, To: "GOMBIT_NEW"}},
			[]string{"GOMBIT_OLD is declared renamed GOMBIT_NEW, which is already read at that point"},
		},
	} {
		got := configProblems(tc.old, tc.current, tc.declared)
		unmatched := append([]string{}, got...)
		for _, w := range tc.want {
			i := slices.IndexFunc(unmatched, func(p string) bool { return strings.Contains(p, w) })
			if i < 0 {
				t.Errorf("%s: no problem mentions %q; problems: %q", name, w, got)
				continue
			}
			unmatched = slices.Delete(unmatched, i, i+1)
		}
		if len(unmatched) > 0 {
			t.Errorf("%s: unexpected problems %q", name, unmatched)
		}
	}
}
