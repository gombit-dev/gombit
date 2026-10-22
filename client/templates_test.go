package client

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestClientTemplateCopiesAgree checks every checked-in copy of the generated
// client.ts against clientTS, the string gombit client generate writes: the
// scaffold template gombit new installs, plus the example and golden trees.
// Editing the template and leaving a copy behind is how the two drift apart,
// and an app only notices when it runs gombit client check.
func TestClientTemplateCopiesAgree(t *testing.T) {
	root := moduleRoot(t)
	paths := []string{
		filepath.Join("scaffold", "templates", "frontend", "src", "api", "generated", "client.ts.tmpl"),
		filepath.Join(SampleOutDir, "client.ts"),
	}
	for _, name := range []string{"client", "new", "new-mui", "new-cookie", "make-resource", "make-command"} {
		p := filepath.Join("goldentest", "testdata", "golden", name, "client.ts")
		if name != "client" {
			p = filepath.Join("goldentest", "testdata", "golden", name, "frontend", "src", "api", "generated", "client.ts")
		}
		paths = append(paths, p)
	}

	for _, rel := range paths {
		// #nosec G304 -- fixed repo-relative path under the module root
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Errorf("read %s: %v", rel, err)
			continue
		}
		if string(data) != clientTS {
			t.Errorf("%s differs from clientTS; regenerate it or update the template", rel)
		}
	}
}

// TestUnwrapResolvesAnySuccess runs the generated client's unwrap in node
// (#505). The bytes are clientTS and errorTS written into a temporary
// directory, so what executes is exactly what `gombit client generate` emits:
// a successful response resolves whatever openapi-fetch parsed, no body
// included, and only a non-2xx — or a result carrying an error — throws the
// D10 ContractError. openapi-fetch itself is stubbed with an empty package
// next to them; unwrap never calls createClient, the module graph only has to
// resolve.
func TestUnwrapResolvesAnySuccess(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	testdata := filepath.Join(filepath.Dir(thisFile), "testdata")

	dir := t.TempDir()
	pkg := filepath.Join(dir, "node_modules", "openapi-fetch")
	writeFile(t, filepath.Join(dir, "client.ts"), clientTS)
	writeFile(t, filepath.Join(dir, "error.ts"), errorTS)
	// openapi-fetch@0.17.0 reports { data: undefined, response } for a 204, a
	// HEAD and a zero-length 2xx; the stub only stands in for its module.
	writeFile(t, filepath.Join(pkg, "package.json"),
		`{"name":"openapi-fetch","version":"0.0.0-stub","type":"module","main":"index.js","exports":"./index.js"}`+"\n")
	writeFile(t, filepath.Join(pkg, "index.js"),
		"export default function createClient() {\n  throw new Error('openapi-fetch stub: unwrap does not call it');\n}\n")

	// register.mjs resolves the generated tree's extensionless sibling
	// imports ("./error"), which node's ESM loader would otherwise refuse.
	cmd := exec.Command("node", //nolint:gosec // fixed harness argv, temp dir only
		"--experimental-strip-types", "--no-warnings",
		"--import", "./register.mjs",
		"./unwrap_harness.mjs",
		filepath.Join(dir, "client.ts"),
	)
	cmd.Dir = testdata
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("unwrap harness: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "ok") {
		t.Fatalf("unwrap harness stdout = %q, want ok", out)
	}
}

// TestUnwrapAgainstPinnedOpenAPIFetch runs the same emitted client against the
// real openapi-fetch, the version gombit client generate pins, through a custom
// fetch (#505). The premise the fix rests on — that a successful response with
// no body reaches the caller as data: undefined — belongs to that library, so
// here it is measured rather than asserted, and unwrap then has to resolve it
// while still throwing for every non-2xx. openapi-fetch is not committed; the
// test skips until the example's lockfile is installed, and CI installs it in
// the test job:
//
//	cd examples/client && npm ci
func TestUnwrapAgainstPinnedOpenAPIFetch(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	pkg := filepath.Join(moduleRoot(t), "examples", "client", "node_modules", "openapi-fetch")
	// #nosec G304 -- fixed repo-relative path under the module root
	meta, err := os.ReadFile(filepath.Join(pkg, "package.json"))
	if err != nil {
		t.Skipf("openapi-fetch is not installed: run npm ci in examples/client (%v)", err)
	}
	var installed struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Main    string `json:"main"`
	}
	if err := json.Unmarshal(meta, &installed); err != nil {
		t.Fatalf("parse %s: %v", filepath.Join(pkg, "package.json"), err)
	}
	pinned := strings.TrimPrefix(openapiFetchPackage, "openapi-fetch@")
	if installed.Name != "openapi-fetch" || installed.Version != pinned {
		t.Skipf("examples/client has %s@%s, the generator pins %s: run npm ci in examples/client",
			installed.Name, installed.Version, openapiFetchPackage)
	}
	entry := filepath.Join(pkg, filepath.FromSlash(strings.TrimPrefix(installed.Main, "./")))
	if _, err := os.Stat(entry); err != nil {
		t.Fatalf("openapi-fetch entry %s: %v", entry, err)
	}

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "client.ts"), clientTS)
	writeFile(t, filepath.Join(dir, "error.ts"), errorTS)
	// The library in the tree node resolves imports from, so the generated
	// client.ts's own `import createClient from "openapi-fetch"` lands on it.
	if err := os.CopyFS(filepath.Join(dir, "node_modules", "openapi-fetch"), os.DirFS(pkg)); err != nil {
		t.Fatalf("copy openapi-fetch: %v", err)
	}

	cmd := exec.Command("node", //nolint:gosec // fixed harness argv, temp dir only
		"--experimental-strip-types", "--no-warnings",
		"--import", "./register.mjs",
		"./unwrap_pinned_harness.mjs",
		filepath.Join(dir, "client.ts"),
		entry,
	)
	cmd.Dir = filepath.Join(filepath.Dir(thisFile), "testdata")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("unwrap against openapi-fetch@%s: %v\n%s", pinned, err, out)
	}
	if !strings.Contains(string(out), "ok") {
		t.Fatalf("unwrap harness stdout = %q, want ok", out)
	}
}
