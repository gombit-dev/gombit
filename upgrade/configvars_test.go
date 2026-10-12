package upgrade_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/upgrade"
)

// configSnapshot is the configuration surface (config.Variables) as of the
// last time this test passed with -update: what the code's variables are
// compared with, so that a change to them is seen.
const configSnapshot = "testdata/config-variables.json"

// TestConfigVariablesAreDeclared: a configuration variable the code stops
// reading, or whose default changes, is a change an application must hear
// about, so the manifest's unreleased entry must declare it (a config
// entry: renamed or removed, or default with the old and new values). The
// snapshot of config.Variables() records what was declared; a change
// fails this test until it is declared and the snapshot updated (-update,
// which refuses while anything is undeclared). The unreleased declarations
// must also agree with the code: a removed or renamed variable is not read
// any more, a rename's new name is, and a default change ends at the
// code's default.
func TestConfigVariablesAreDeclared(t *testing.T) {
	data, err := os.ReadFile(configSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot []config.Variable
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	m, err := upgrade.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	var declared []upgrade.ConfigChange
	if r, err := m.Release(upgrade.Unreleased); err == nil {
		for _, c := range r.Changes {
			declared = append(declared, c.Config...)
		}
	}
	current := config.Variables()
	if problems := undeclaredConfigChanges(snapshot, current, declared); len(problems) > 0 {
		t.Fatalf("configuration changes the manifest's unreleased entry does not declare (upgrade/manifest.yaml, a change's config list):\n  %s", strings.Join(problems, "\n  "))
	}
	if reflect.DeepEqual(snapshot, current) {
		return
	}
	if !*update {
		t.Fatalf("%s is stale: the configuration changes are declared; run go test ./upgrade -run TestConfigVariablesAreDeclared -update", configSnapshot)
	}
	out, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.FromSlash(configSnapshot), append(out, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// declassified are the variables that stopped being secret on purpose, each
// with the reason: a secret's value is never printed, so turning that off
// is reviewed here, not just snapshotted.
var declassified = map[string]string{}

// undeclaredConfigChanges are the differences between the snapshot and the
// current variables that declared does not cover, and the declarations the
// current variables contradict.
func undeclaredConfigChanges(snapshot, current []config.Variable, declared []upgrade.ConfigChange) []string {
	now := map[string]config.Variable{}
	for _, v := range current {
		now[v.Name] = v
	}
	has := func(match func(upgrade.ConfigChange) bool) bool {
		for _, cc := range declared {
			if match(cc) {
				return true
			}
		}
		return false
	}
	var problems []string
	for _, old := range snapshot {
		v, still := now[old.Name]
		if !still {
			if !has(func(cc upgrade.ConfigChange) bool {
				return cc.Var == old.Name && (cc.Change == upgrade.ConfigRemoved || cc.Change == upgrade.ConfigRenamed)
			}) {
				problems = append(problems, fmt.Sprintf("%s is no longer read: declare it removed, or renamed (to its new name)", old.Name))
			}
			continue
		}
		if old.Secret && !v.Secret && declassified[old.Name] == "" {
			problems = append(problems, fmt.Sprintf("%s is no longer secret: the tooling would print its value; if that is right, add it to declassified with the reason", old.Name))
		}
		for _, d := range []struct{ env, from, to string }{
			{"development", old.Default, v.Default},
			{"test", old.TestDefault, v.TestDefault},
			{"production", old.ProductionDefault, v.ProductionDefault},
		} {
			if d.from == d.to {
				continue
			}
			if !has(func(cc upgrade.ConfigChange) bool {
				return cc.Var == old.Name && cc.Change == upgrade.ConfigDefault && (cc.Environment == "" || cc.Environment == d.env) && cc.From == d.from && cc.To == d.to
			}) {
				problems = append(problems, fmt.Sprintf("%s's %s default changed from %q to %q: declare it (change: default, from, to, environment: %s or none)", old.Name, d.env, d.from, d.to, d.env))
			}
		}
	}
	for _, cc := range declared {
		v, read := now[cc.Var]
		switch cc.Change {
		case upgrade.ConfigRemoved, upgrade.ConfigRenamed:
			if read {
				problems = append(problems, fmt.Sprintf("%s is declared %s but the code still reads it", cc.Var, cc.Change))
			}
			if _, new := now[cc.To]; cc.Change == upgrade.ConfigRenamed && !new {
				problems = append(problems, fmt.Sprintf("%s is declared renamed %s, which the code does not read", cc.Var, cc.To))
			}
		case upgrade.ConfigDefault:
			if !read {
				problems = append(problems, fmt.Sprintf("%s's default change is declared but the code does not read it", cc.Var))
				continue
			}
			if (cc.Environment == "" || cc.Environment == "development") && cc.To != v.Default ||
				(cc.Environment == "" || cc.Environment == "test") && cc.To != v.TestDefault ||
				(cc.Environment == "" || cc.Environment == "production") && cc.To != v.ProductionDefault {
				problems = append(problems, fmt.Sprintf("%s is declared to default to %q, but the code's default is %q (test %q, production %q)", cc.Var, cc.To, v.Default, v.TestDefault, v.ProductionDefault))
			}
		case upgrade.ConfigRequired, upgrade.ConfigRefused:
			if !read {
				problems = append(problems, fmt.Sprintf("%s is declared %s but the code does not read it", cc.Var, cc.Change))
			}
		}
	}
	return problems
}

// TestUndeclaredConfigChanges: the rule itself, on made-up surfaces.
func TestUndeclaredConfigChanges(t *testing.T) {
	v := func(name, dev, prod string) config.Variable {
		return config.Variable{Name: name, Default: dev, TestDefault: dev, ProductionDefault: prod}
	}
	secret := func(v config.Variable) config.Variable { v.Secret = true; return v }
	testOnly := v("GOMBIT_T", "a", "a")
	testOnly.TestDefault = "b"
	snapshot := []config.Variable{v("GOMBIT_A", "1", "1"), v("GOMBIT_B", "x", "x"), v("GOMBIT_C", "on", "off"), secret(v("GOMBIT_S", "", "")), v("GOMBIT_T", "a", "a")}
	current := []config.Variable{v("GOMBIT_A2", "1", "1"), v("GOMBIT_C", "on", "on"), v("GOMBIT_S", "", ""), testOnly}
	for name, tc := range map[string]struct {
		declared []upgrade.ConfigChange
		want     []string
	}{
		"nothing declared": {nil, []string{"GOMBIT_A is no longer read", "GOMBIT_B is no longer read", `GOMBIT_C's production default changed from "off" to "on"`, "GOMBIT_S is no longer secret", `GOMBIT_T's test default changed from "a" to "b"`}},
		"all declared": {[]upgrade.ConfigChange{
			{Var: "GOMBIT_A", Change: upgrade.ConfigRenamed, To: "GOMBIT_A2"},
			{Var: "GOMBIT_B", Change: upgrade.ConfigRemoved},
			{Var: "GOMBIT_C", Change: upgrade.ConfigDefault, From: "off", To: "on", Environment: "production"},
			{Var: "GOMBIT_T", Change: upgrade.ConfigDefault, From: "a", To: "b", Environment: "test"},
		}, []string{"GOMBIT_S is no longer secret"}},
		"wrong default values": {[]upgrade.ConfigChange{
			{Var: "GOMBIT_A", Change: upgrade.ConfigRemoved},
			{Var: "GOMBIT_B", Change: upgrade.ConfigRemoved},
			{Var: "GOMBIT_C", Change: upgrade.ConfigDefault, From: "on", To: "off", Environment: "production"},
			{Var: "GOMBIT_T", Change: upgrade.ConfigDefault, From: "a", To: "b", Environment: "test"},
		}, []string{`GOMBIT_C's production default changed`, "GOMBIT_S is no longer secret", `GOMBIT_C is declared to default to "off"`}},
		"contradicted": {[]upgrade.ConfigChange{
			{Var: "GOMBIT_A", Change: upgrade.ConfigRenamed, To: "GOMBIT_Z"},
			{Var: "GOMBIT_B", Change: upgrade.ConfigRemoved},
			{Var: "GOMBIT_C", Change: upgrade.ConfigRemoved},
			{Var: "GOMBIT_C", Change: upgrade.ConfigDefault, From: "off", To: "on", Environment: "production"},
			{Var: "GOMBIT_T", Change: upgrade.ConfigDefault, From: "a", To: "b", Environment: "test"},
			{Var: "GOMBIT_Q", Change: upgrade.ConfigRequired},
		}, []string{"GOMBIT_S is no longer secret", "GOMBIT_A is declared renamed GOMBIT_Z, which the code does not read", "GOMBIT_C is declared removed but the code still reads it", "GOMBIT_Q is declared required but the code does not read it"}},
	} {
		got := undeclaredConfigChanges(snapshot, current, tc.declared)
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
