package upgrade

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const applyFixture = `format: 1
releases:
  - version: v0.6.1
  - version: v0.7.0
    changes:
      - {id: c1, kind: informational, area: api, summary: s}
  - version: v0.8.0
    changes:
      - {id: c2, kind: manual, area: config, summary: s, details: d}
`

// fakeGo plays the go command on go.mod: go mod edit -dropreplace drops
// the replace it names, and go get MODULE@V moves the require and writes a
// go.sum line; fail makes the command whose first argument it names fail
// (after it changed the files, as a real failure may).
type fakeGo struct {
	t     *testing.T
	calls [][]string
	fail  string
	noop  bool // go get changes nothing
}

func (f *fakeGo) run(_ context.Context, dir string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	path := filepath.Join(dir, "go.mod")
	data, err := os.ReadFile(path) // #nosec G304 -- under t.TempDir()
	if err != nil {
		f.t.Fatal(err)
	}
	s := string(data)
	switch {
	case len(args) == 3 && args[0] == "mod" && strings.HasPrefix(args[2], "-dropreplace="):
		old := strings.Replace(strings.TrimPrefix(args[2], "-dropreplace="), "@", " ", 1)
		s = regexp.MustCompile(`(?m)^replace `+regexp.QuoteMeta(old)+` =>.*\n`).ReplaceAllString(s, "")
	case args[0] == "get" && !f.noop:
		mod, v, _ := strings.Cut(args[1], "@")
		s = regexp.MustCompile(`(?m)^require `+regexp.QuoteMeta(mod)+` \S+`).ReplaceAllString(s, "require "+mod+" "+v)
		if err := os.WriteFile(filepath.Join(dir, "go.sum"), []byte(mod+" "+v+" h1:fake=\n"), 0o600); err != nil {
			f.t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil { // #nosec G703 -- under t.TempDir()
		f.t.Fatal(err)
	}
	if f.fail != "" && args[0] == f.fail {
		return []byte("go: github.com/gombit-dev/gombit@v0.8.0: reading file:///proxy: 404 Not Found\n"), errors.New("exit status 1")
	}
	return []byte("go: upgraded\n"), nil
}

func applyApp(t *testing.T, goMod string) string {
	t.Helper()
	t.Setenv("GOWORK", "off")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func withFakeGo(t *testing.T, f *fakeGo) {
	t.Helper()
	f.t = t
	prev := runGo
	runGo = f.run
	t.Cleanup(func() { runGo = prev })
}

const applyGoMod = "module example.com/demo\n\ngo 1.26\n\nrequire github.com/gombit-dev/gombit v0.6.1\n"

// TestApplyDependenciesRunsTheReplaceEditsThenGoGet: the replaces the plan
// names are edited first, so that go get, last, resolves the target; the
// result is verified and every changed line reported.
func TestApplyDependenciesRunsTheReplaceEditsThenGoGet(t *testing.T) {
	m, err := ParseManifest([]byte(applyFixture))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeGo{}
	withFakeGo(t, f)
	dir := applyApp(t, applyGoMod+"replace github.com/gombit-dev/gombit v0.8.0 => ../gombit\n")
	a, err := ApplyDependencies(context.Background(), dir, m, "v0.8.0")
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"mod", "edit", "-dropreplace=github.com/gombit-dev/gombit@v0.8.0"},
		{"get", "github.com/gombit-dev/gombit@v0.8.0"},
	}
	if len(f.calls) != len(want) || strings.Join(f.calls[0], " ") != strings.Join(want[0], " ") || strings.Join(f.calls[1], " ") != strings.Join(want[1], " ") {
		t.Fatalf("go commands = %v; want %v", f.calls, want)
	}
	if len(a.Commands) != 2 || a.Commands[1].Output != "go: upgraded\n" {
		t.Fatalf("Commands = %+v", a.Commands)
	}
	if strings.Join(a.GoMod.Removed, "|") != "require github.com/gombit-dev/gombit v0.6.1|replace github.com/gombit-dev/gombit v0.8.0 => ../gombit" ||
		strings.Join(a.GoMod.Added, "|") != "require github.com/gombit-dev/gombit v0.8.0" || len(a.GoSum.Added) != 1 {
		t.Fatalf("GoMod = %+v, GoSum = %+v", a.GoMod, a.GoSum)
	}
	var out strings.Builder
	if err := a.Render(&out); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		"Gombit upgrade v0.6.1 -> v0.8.0\n",
		"  go mod edit -dropreplace=github.com/gombit-dev/gombit@v0.8.0\n  go get github.com/gombit-dev/gombit@v0.8.0\n",
		"  - require github.com/gombit-dev/gombit v0.6.1\n",
		"  + require github.com/gombit-dev/gombit v0.8.0\n",
		"go.sum: 1 line(s) added, 0 removed\n",
		"Manual actions: 1\n",
	} {
		if !strings.Contains(out.String(), s) {
			t.Fatalf("Render lacks %q:\n%s", s, out.String())
		}
	}
}

// TestApplyDependenciesRestoresOnFailure: a go command that fails is
// reported with its full output, and go.mod and go.sum get back the bytes
// they had (a go.sum that did not exist is removed); so does a result that
// does not build the target.
func TestApplyDependenciesRestoresOnFailure(t *testing.T) {
	m, err := ParseManifest([]byte(applyFixture))
	if err != nil {
		t.Fatal(err)
	}
	for name, f := range map[string]*fakeGo{"go get fails": {fail: "get"}, "go get changes nothing": {noop: true}} {
		t.Run(name, func(t *testing.T) {
			withFakeGo(t, f)
			original := applyGoMod + "replace github.com/gombit-dev/gombit v0.8.0 => ../gombit\n"
			dir := applyApp(t, original)
			_, err := ApplyDependencies(context.Background(), dir, m, "v0.8.0")
			if err == nil || !strings.Contains(err.Error(), "(go.mod and go.sum are restored)") {
				t.Fatalf("ApplyDependencies = %v; want a failure, restored", err)
			}
			if f.fail != "" {
				var goErr *GoCommandError
				if !errors.As(err, &goErr) || !strings.Contains(err.Error(), "go get github.com/gombit-dev/gombit@v0.8.0: exit status 1\ngo: github.com/gombit-dev/gombit@v0.8.0: reading file:///proxy: 404 Not Found") {
					t.Fatalf("error = %v; want the go command and its output, verbatim", err)
				}
			} else if !strings.Contains(err.Error(), `builds against "v0.6.1", not v0.8.0`) {
				t.Fatalf("error = %v; want the result's build named", err)
			}
			if got, _ := os.ReadFile(filepath.Join(dir, "go.mod")); string(got) != original { // #nosec G304 -- under t.TempDir()
				t.Fatalf("go.mod = %q; want it restored", got)
			}
			if _, err := os.Stat(filepath.Join(dir, "go.sum")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("go.sum = %v; want it removed, as it did not exist", err)
			}
		})
	}
}

// TestApplyDependenciesNeedsATargetAndAPath: no target is refused, the
// plan's refusals stand, and at the target nothing runs.
func TestApplyDependenciesNeedsATargetAndAPath(t *testing.T) {
	m, err := ParseManifest([]byte(applyFixture))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeGo{}
	withFakeGo(t, f)
	dir := applyApp(t, applyGoMod)
	if _, err := ApplyDependencies(context.Background(), dir, m, ""); !errors.Is(err, ErrNoTarget) {
		t.Fatalf("no target = %v", err)
	}
	if _, err := ApplyDependencies(context.Background(), dir, m, "v0.9.0"); !errors.Is(err, ErrNoUpgradePath) {
		t.Fatalf("unlisted target = %v", err)
	}
	at := applyApp(t, strings.Replace(applyGoMod, "v0.6.1", "v0.8.0", 1))
	a, err := ApplyDependencies(context.Background(), at, m, "v0.8.0")
	if err != nil || len(a.Commands) != 0 || len(f.calls) != 0 {
		t.Fatalf("at the target = %+v, %v, %d go calls; want nothing run", a, err, len(f.calls))
	}
}
