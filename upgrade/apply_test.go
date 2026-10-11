package upgrade_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/module"
	"golang.org/x/mod/zip"

	"github.com/gombit-dev/gombit/upgrade"
)

// goProxy writes a GOPROXY directory serving github.com/gombit-dev/gombit
// at versions, each a module with one package (fw), and returns its file
// URL.
func goProxy(t *testing.T, versions ...string) string {
	t.Helper()
	root := t.TempDir()
	at := filepath.Join(root, "github.com", "gombit-dev", "gombit", "@v")
	if err := os.MkdirAll(at, 0o750); err != nil {
		t.Fatal(err)
	}
	gomod := "module " + upgrade.FrameworkModulePath + "\n\ngo 1.21\n"
	for _, v := range versions {
		src := t.TempDir()
		if err := os.MkdirAll(filepath.Join(src, "fw"), 0o750); err != nil {
			t.Fatal(err)
		}
		for name, content := range map[string]string{"go.mod": gomod, "fw/fw.go": "package fw\n\n// Version is " + v + ".\nconst Version = \"" + v + "\"\n"} {
			if err := os.WriteFile(filepath.Join(src, name), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		var z bytes.Buffer
		if err := zip.CreateFromDir(&z, module.Version{Path: upgrade.FrameworkModulePath, Version: v}, src); err != nil {
			t.Fatal(err)
		}
		for name, data := range map[string][]byte{
			v + ".info": []byte(`{"Version":"` + v + `","Time":"2026-10-01T00:00:00Z"}`),
			v + ".mod":  []byte(gomod),
			v + ".zip":  z.Bytes(),
		} {
			if err := os.WriteFile(filepath.Join(at, name), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(at, "list"), []byte(strings.Join(versions, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A file URL's path starts with a slash, also before a Windows drive
	// (file:///C:/...).
	path := filepath.ToSlash(root)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return "file://" + path
}

// offlineGo points the go command at proxy only, with a module cache of
// the test's own and no checksum database, so it never reaches the network.
func offlineGo(t *testing.T, proxy string) {
	t.Helper()
	t.Setenv("GOPROXY", proxy)
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GONOSUMDB", "")
	t.Setenv("GOPRIVATE", "")
	t.Setenv("GOFLAGS", "-mod=mod -modcacherw")
	t.Setenv("GOMODCACHE", t.TempDir())
	t.Setenv("GOTOOLCHAIN", "local")
	t.Setenv("GOWORK", "off")
}

const appMain = "package main\n\nimport \"github.com/gombit-dev/gombit/fw\"\n\nfunc main() { println(fw.Version) }\n"

// goApp writes an app requiring the framework at version, tidied (go.sum
// written) with the go command against the test proxy.
func goApp(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	goMod := "module example.com/demo\n\ngo 1.21\n\nrequire github.com/gombit-dev/gombit " + version + "\n"
	for name, content := range map[string]string{"go.mod": goMod, "main.go": appMain} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("go", "mod", "tidy")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}
	return dir
}

const goFixture = `format: 1
releases:
  - version: v0.7.0
  - version: v0.7.1
    changes:
      - {id: c1, kind: manual, area: config, summary: Rename a setting., details: Rename it.}
  - version: v0.7.2
`

// TestApplyDependenciesWithTheGoCommand: the go command itself upgrades
// the requirement and go.sum, the result builds the target, and a version
// the module source does not have fails with Go's own message and leaves
// go.mod and go.sum as they were.
func TestApplyDependenciesWithTheGoCommand(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go command")
	}
	offlineGo(t, goProxy(t, "v0.7.0", "v0.7.1"))
	m, err := upgrade.ParseManifest([]byte(goFixture))
	if err != nil {
		t.Fatal(err)
	}
	dir := goApp(t, "v0.7.0")

	a, err := upgrade.ApplyDependencies(context.Background(), dir, m, "v0.7.1")
	if err != nil {
		t.Fatal(err)
	}
	if b, err := upgrade.Detect(dir); err != nil || b.Framework.Version != "v0.7.1" || b.Framework.Required != "v0.7.1" {
		t.Fatalf("after the upgrade: %+v, %v", b.Framework, err)
	}
	// go get records the target's sums and keeps the old ones (go mod tidy
	// would prune them); either way the change is reported.
	if strings.Join(a.GoMod.Added, "|") != "require github.com/gombit-dev/gombit v0.7.1" || len(a.GoSum.Added) != 2 {
		t.Fatalf("GoMod = %+v, GoSum = %+v", a.GoMod, a.GoSum)
	}
	build := exec.Command("go", "build", "./...")
	build.Dir = dir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("the upgraded app does not build: %v\n%s", err, out)
	}

	// v0.7.2 is in the manifest, but no module source has it.
	goMod, _ := os.ReadFile(filepath.Join(dir, "go.mod")) // #nosec G304 -- under t.TempDir()
	goSum, _ := os.ReadFile(filepath.Join(dir, "go.sum")) // #nosec G304 -- as above
	_, err = upgrade.ApplyDependencies(context.Background(), dir, m, "v0.7.2")
	var goErr *upgrade.GoCommandError
	if !errors.As(err, &goErr) || !strings.Contains(err.Error(), "go get -modfile=go.mod github.com/gombit-dev/gombit@v0.7.2") ||
		!strings.Contains(goErr.Output, "v0.7.2") || !strings.Contains(err.Error(), "(go.mod and go.sum are restored)") {
		t.Fatalf("upgrade to a version no source has = %v; want the go command's own failure, restored", err)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "go.mod")); !bytes.Equal(after, goMod) { // #nosec G304 -- as above
		t.Fatal("a failed upgrade changed go.mod")
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "go.sum")); !bytes.Equal(after, goSum) { // #nosec G304 -- as above
		t.Fatal("a failed upgrade changed go.sum")
	}
	t.Logf("go's own failure, as reported:\n%v", err)
}

// TestApplyDependenciesEditsReplacesWithTheGoCommand: the replace edits
// the plan makes are the go command's own (go mod edit): a dormant replace
// of the target version is dropped, and a self-replace of every version
// moves to the target, so the upgraded build is the target release.
func TestApplyDependenciesEditsReplacesWithTheGoCommand(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go command")
	}
	offlineGo(t, goProxy(t, "v0.7.0", "v0.7.1"))
	m, err := upgrade.ParseManifest([]byte(goFixture))
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		replace, wantGoMod, notGoMod string
	}{
		"a dormant replace of the target is dropped": {
			"replace github.com/gombit-dev/gombit v0.7.1 => ../next\n", "", "../next",
		},
		"a self-replace of every version moves": {
			"replace github.com/gombit-dev/gombit => github.com/gombit-dev/gombit v0.7.0\n",
			"replace github.com/gombit-dev/gombit => github.com/gombit-dev/gombit v0.7.1", "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := goApp(t, "v0.7.0")
			goMod := filepath.Join(dir, "go.mod")
			data, err := os.ReadFile(goMod) // #nosec G304 -- under t.TempDir()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(goMod, append(data, tc.replace...), 0o600); err != nil { // #nosec G703 -- under t.TempDir()
				t.Fatal(err)
			}
			if _, err := upgrade.ApplyDependencies(context.Background(), dir, m, "v0.7.1"); err != nil {
				t.Fatal(err)
			}
			after, _ := os.ReadFile(goMod) // #nosec G304 -- as above
			if tc.wantGoMod != "" && !strings.Contains(string(after), tc.wantGoMod) {
				t.Fatalf("go.mod lacks %q:\n%s", tc.wantGoMod, after)
			}
			if tc.notGoMod != "" && strings.Contains(string(after), tc.notGoMod) {
				t.Fatalf("go.mod still has %q:\n%s", tc.notGoMod, after)
			}
			if b, err := upgrade.Detect(dir); err != nil || b.Framework.Version != "v0.7.1" {
				t.Fatalf("after the upgrade: %+v, %v", b.Framework, err)
			}
		})
	}
}

// TestApplyDependenciesVendoredApp: in a vendored app the upgrade says
// vendor/ must be refreshed, rather than leave a build that fails.
func TestApplyDependenciesVendoredApp(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go command")
	}
	offlineGo(t, goProxy(t, "v0.7.0", "v0.7.1"))
	m, err := upgrade.ParseManifest([]byte(goFixture))
	if err != nil {
		t.Fatal(err)
	}
	dir := goApp(t, "v0.7.0")
	vendor := exec.Command("go", "mod", "vendor")
	vendor.Dir = dir
	if out, err := vendor.CombinedOutput(); err != nil {
		t.Fatalf("go mod vendor: %v\n%s", err, out)
	}
	a, err := upgrade.ApplyDependencies(context.Background(), dir, m, "v0.7.1")
	if err != nil {
		t.Fatal(err)
	}
	if len(a.FollowUp) != 1 || !strings.Contains(a.FollowUp[0], "go mod vendor") {
		t.Fatalf("FollowUp = %v; want the vendor refresh", a.FollowUp)
	}
	var out strings.Builder
	if err := a.Render(&out); err != nil || !strings.Contains(out.String(), "Next: vendor/ still holds the old modules") {
		t.Fatalf("Render = %q, %v", out.String(), err)
	}
}

// TestApplyDependenciesIgnoresAnInheritedModfile: with GOFLAGS naming an
// alternate -modfile, the go commands still edit the module's go.mod and
// go.sum (the ones snapshotted, verified and restored), and the alternate
// pair is left exactly as it was.
func TestApplyDependenciesIgnoresAnInheritedModfile(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go command")
	}
	offlineGo(t, goProxy(t, "v0.7.0", "v0.7.1"))
	m, err := upgrade.ParseManifest([]byte(goFixture))
	if err != nil {
		t.Fatal(err)
	}
	dir := goApp(t, "v0.7.0")
	// Both files carry a dormant replace of the target, so the plan edits a
	// replace (go mod edit) as well as the require (go get).
	goMod := filepath.Join(dir, "go.mod")
	alternate := filepath.Join(dir, "alternate.mod")
	data, err := os.ReadFile(goMod) // #nosec G304 -- under t.TempDir()
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, "replace github.com/gombit-dev/gombit v0.7.1 => ../next\n"...)
	for _, path := range []string{goMod, alternate} {
		if err := os.WriteFile(path, data, 0o600); err != nil { // #nosec G703 -- under t.TempDir()
			t.Fatal(err)
		}
	}
	altBefore, _ := os.ReadFile(alternate) // #nosec G304 -- as above
	t.Setenv("GOFLAGS", "-mod=mod -modcacherw -modfile=alternate.mod")

	if _, err := upgrade.ApplyDependencies(context.Background(), dir, m, "v0.7.1"); err != nil {
		t.Fatal(err)
	}
	if b, err := upgrade.Detect(dir); err != nil || b.Framework.Version != "v0.7.1" || b.Framework.Replace != nil {
		t.Fatalf("go.mod after the upgrade: %+v, %v; want v0.7.1 with the replace dropped", b.Framework, err)
	}
	if altAfter, _ := os.ReadFile(alternate); !bytes.Equal(altAfter, altBefore) { // #nosec G304 -- as above
		t.Fatalf("the upgrade changed alternate.mod:\n%s", altAfter)
	}
	if _, err := os.Stat(filepath.Join(dir, "alternate.sum")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the upgrade wrote alternate.sum (%v)", err)
	}
}
