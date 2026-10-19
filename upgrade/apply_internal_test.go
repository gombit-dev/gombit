package upgrade

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/migrations"
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
	// unreadableSum makes go get leave go.sum unreadable (a directory), so
	// the final report cannot be read after the upgrade itself succeeded.
	unreadableSum bool
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
	case len(args) == 4 && args[0] == "mod" && strings.HasPrefix(args[2], "-dropreplace=") && args[3] == "go.mod":
		old := strings.Replace(strings.TrimPrefix(args[2], "-dropreplace="), "@", " ", 1)
		s = regexp.MustCompile(`(?m)^replace `+regexp.QuoteMeta(old)+` =>.*\n`).ReplaceAllString(s, "")
	case args[0] == "get" && !f.noop:
		if args[1] != "-modfile=go.mod" {
			f.t.Fatalf("go get is not pinned to go.mod: %v", args)
		}
		mod, v, _ := strings.Cut(args[2], "@")
		s = regexp.MustCompile(`(?m)^require `+regexp.QuoteMeta(mod)+` \S+`).ReplaceAllString(s, "require "+mod+" "+v)
		if f.unreadableSum {
			if err := os.Mkdir(filepath.Join(dir, "go.sum"), 0o750); err != nil {
				f.t.Fatal(err)
			}
		} else if err := os.WriteFile(filepath.Join(dir, "go.sum"), []byte(mod+" "+v+" h1:fake=\n"), 0o600); err != nil {
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
		{"mod", "edit", "-dropreplace=github.com/gombit-dev/gombit@v0.8.0", "go.mod"},
		{"get", "-modfile=go.mod", "github.com/gombit-dev/gombit@v0.8.0"},
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
		"  go mod edit -dropreplace=github.com/gombit-dev/gombit@v0.8.0 go.mod\n  go get -modfile=go.mod github.com/gombit-dev/gombit@v0.8.0\n",
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
	for name, f := range map[string]*fakeGo{
		"go get fails":                   {fail: "get"},
		"go get changes nothing":         {noop: true},
		"the result cannot be read back": {unreadableSum: true},
	} {
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
				if !errors.As(err, &goErr) || !strings.Contains(err.Error(), "go get -modfile=go.mod github.com/gombit-dev/gombit@v0.8.0: exit status 1\ngo: github.com/gombit-dev/gombit@v0.8.0: reading file:///proxy: 404 Not Found") {
					t.Fatalf("error = %v; want the go command and its output, verbatim", err)
				}
			} else if f.noop && !strings.Contains(err.Error(), `builds against "v0.6.1", not v0.8.0`) {
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

// TestApplyDependenciesVendorDirectory: a top-level vendor/ makes the go
// command build from it whether or not it has modules.txt (a missing one is
// just as stale), so either way the upgrade says to refresh it.
func TestApplyDependenciesVendorDirectory(t *testing.T) {
	m, err := ParseManifest([]byte(applyFixture))
	if err != nil {
		t.Fatal(err)
	}
	withFakeGo(t, &fakeGo{})
	dir := applyApp(t, applyGoMod)
	if err := os.Mkdir(filepath.Join(dir, "vendor"), 0o750); err != nil {
		t.Fatal(err)
	}
	a, err := ApplyDependencies(context.Background(), dir, m, "v0.8.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(a.FollowUp) != 1 || !strings.Contains(a.FollowUp[0], "go mod vendor") {
		t.Fatalf("FollowUp = %v; want the vendor refresh for a vendor/ without modules.txt", a.FollowUp)
	}
}

// TestApplyDependenciesNeverMigrates: an upgrade across a framework-schema
// change runs the go commands only, never a migration, and says the
// upgrade is not complete: the migration steps are listed, and
// migration_required is set, before anything counts the dependency change
// as done.
func TestApplyDependenciesNeverMigrates(t *testing.T) {
	m, err := ParseManifest([]byte(`format: 1
releases:
  - version: v0.6.1
  - version: v0.8.0
    changes:
      - id: refresh-device
        kind: manual
        area: schema
        summary: s
        details: d
        schema:
          - {model: auth.RefreshToken, table: refresh_tokens, change: altered}
`))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeGo{}
	withFakeGo(t, f)
	dir := applyApp(t, applyGoMod)
	migrationDir := filepath.Join(dir, "database", "migrations")
	if err := os.MkdirAll(migrationDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := migrations.SaveRegistry(migrationDir, []migrations.Model{{ImportPath: "github.com/gombit-dev/gombit/auth", TypeName: "RefreshToken"}}); err != nil {
		t.Fatal(err)
	}
	a, err := ApplyDependencies(context.Background(), dir, m, "v0.8.0")
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range f.calls {
		if call[0] != "mod" && call[0] != "get" {
			t.Fatalf("ran go %v; the upgrade runs only go mod edit and go get", call)
		}
	}
	if !a.MigrationRequired || !a.Plan.Schema.MigrationRequired() {
		t.Fatalf("Applied = %+v; want the migration required", a)
	}
	var out strings.Builder
	if err := a.Render(&out); err != nil {
		t.Fatal(err)
	}
	notDone := strings.Index(out.String(), "Not complete yet")
	steps := strings.Index(out.String(), "Required migration steps")
	if notDone < 0 || steps < notDone || !strings.Contains(out.String(), "No migration was run.") || !strings.Contains(out.String(), "gombit db migrate") {
		t.Fatalf("Render:\n%s", out.String())
	}
}

// TestApplyDependenciesReadsTheRegistryFirst: the framework-schema
// analysis, in the migration directory asked for, runs as part of the plan,
// before go.mod is touched: a registry that cannot be read stops the
// upgrade with nothing changed, and only the directory asked for is read.
func TestApplyDependenciesReadsTheRegistryFirst(t *testing.T) {
	m, err := ParseManifest([]byte(applyFixture))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeGo{}
	withFakeGo(t, f)
	dir := applyApp(t, applyGoMod)
	for name, content := range map[string]string{"db/migrations/models.json": "{", "database/migrations/models.json": "{"} {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ApplyDependenciesWith(context.Background(), dir, m, "v0.8.0", PlanOptions{MigrationDir: "db/migrations"}); err == nil || !strings.Contains(err.Error(), "model registry") {
		t.Fatalf("an unreadable registry = %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("go commands ran before the registry was read: %v", f.calls)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "go.mod")); string(data) != applyGoMod { // #nosec G304 -- under t.TempDir()
		t.Fatal("go.mod changed")
	}
	// The directory asked for is the one read: a broken default one does
	// not matter.
	if err := os.WriteFile(filepath.Join(dir, "db", "migrations", "models.json"), []byte(`{"models": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyDependenciesWith(context.Background(), dir, m, "v0.8.0", PlanOptions{MigrationDir: "db/migrations"}); err != nil {
		t.Fatalf("with db/migrations readable: %v", err)
	}
}
