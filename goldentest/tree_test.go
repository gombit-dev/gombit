package goldentest

import (
	"bytes"
	"fmt"
	"go/format"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

var gombitRequirePattern = regexp.MustCompile(`(?m)^(require\s+github\.com/gombit-dev/gombit\s+)v\S+`)

const (
	goldenRoot     = "testdata/golden"
	gombitModule   = "github.com/gombit-dev/gombit"
	fixtureName    = "demo"
	fixtureModule  = "github.com/example/demo"
	fixtureBook    = "Book"
	fixtureFields  = "title:string:required"
	missingAtlas   = "gombit-golden-atlas-not-installed"
	maxDiffPreview = 80
)

var skipSnapshotDirs = map[string]struct{}{
	"node_modules": {},
	".git":         {},
	".gombit":      {},
	".vite":        {},
	"dist":         {},
}

type fileMap map[string][]byte

func assertFrontendInvariants(t *testing.T, files fileMap) {
	t.Helper()
	var sawFrontend bool
	for rel, data := range files {
		if !strings.HasPrefix(rel, "frontend/") {
			continue
		}
		ext := strings.ToLower(filepath.Ext(rel))
		switch ext {
		case ".ts", ".tsx", ".js", ".jsx", ".json", ".html", ".mjs":
		default:
			continue
		}
		sawFrontend = true
		lower := strings.ToLower(string(data))
		if strings.Contains(lower, "localstorage") || strings.Contains(lower, "sessionstorage") {
			t.Errorf("%s contains localStorage/sessionStorage", rel)
		}
	}
	if !sawFrontend {
		t.Fatal("generated tree has no frontend/ source files")
	}
	formErrors := string(files["frontend/src/api/formErrors.ts"])
	if formErrors == "" {
		t.Fatal("missing frontend/src/api/formErrors.ts")
	}
	for _, want := range []string{"setError", "fields", "ContractError", "isD10ErrorBody"} {
		if !strings.Contains(formErrors, want) {
			t.Errorf("formErrors.ts missing %q", want)
		}
	}
	session := string(files["frontend/src/auth/session.ts"])
	if session == "" {
		t.Fatal("missing frontend/src/auth/session.ts")
	}
	if !strings.Contains(session, "getAccessToken") || !strings.Contains(session, "clearSession") {
		t.Error("session.ts missing in-memory token helpers")
	}
	assertNumberInputEmptyIsZero(t, string(files["frontend/src/pages/ProductFormPage.tsx"]))
	login := string(files["frontend/src/pages/LoginPage.tsx"])
	if login == "" {
		t.Fatal("missing frontend/src/pages/LoginPage.tsx")
	}
	if !strings.Contains(login, "/api/v1/auth/login") {
		t.Error("LoginPage.tsx missing login path")
	}
	if strings.Contains(login, "bootstrapCSRF") {
		t.Error("jwt LoginPage.tsx must not import bootstrapCSRF")
	}
	client := string(files["frontend/src/api/client.ts"])
	if client == "" {
		t.Fatal("missing frontend/src/api/client.ts")
	}
	if !strings.Contains(client, "refreshInFlight") {
		t.Error("client.ts missing shared refresh promise")
	}
	retry := string(files["frontend/src/api/retry.ts"])
	if retry == "" {
		t.Fatal("missing frontend/src/api/retry.ts")
	}
	assert401RetryReusesBufferedBody(t, client, retry, []string{`headers.set("Authorization", ` + "`Bearer ${access}`" + `)`})
	if strings.Contains(client, "csrfInFlight") || strings.Contains(client, "bootstrapCSRF") {
		t.Error("jwt client.ts must not include cookie CSRF bootstrap")
	}
	assertRuntimeAPIPrefix(t, files, "jwt")
}

// assertCookieFrontendInvariants is assertFrontendInvariants' counterpart for
// `--auth cookie` (M5-3): no web-storage tokens, and the cookie/CSRF wiring
// (X-CSRF-Token double-submit, GET /auth/csrf bootstrap) is present instead
// of the Bearer in-memory token helpers.
func assertCookieFrontendInvariants(t *testing.T, files fileMap) {
	t.Helper()
	var sawFrontend bool
	for rel, data := range files {
		if !strings.HasPrefix(rel, "frontend/") {
			continue
		}
		ext := strings.ToLower(filepath.Ext(rel))
		switch ext {
		case ".ts", ".tsx", ".js", ".jsx", ".json", ".html", ".mjs":
		default:
			continue
		}
		sawFrontend = true
		lower := strings.ToLower(string(data))
		if strings.Contains(lower, "localstorage") || strings.Contains(lower, "sessionstorage") {
			t.Errorf("%s contains localStorage/sessionStorage", rel)
		}
	}
	if !sawFrontend {
		t.Fatal("generated tree has no frontend/ source files")
	}
	session := string(files["frontend/src/auth/session.ts"])
	if session == "" {
		t.Fatal("missing frontend/src/auth/session.ts")
	}
	if !strings.Contains(session, "isAuthenticated") || !strings.Contains(session, "getCSRFToken") {
		t.Error("session.ts missing cookie-mode session/CSRF helpers")
	}
	if strings.Contains(session, "getAccessToken") {
		t.Error("cookie-mode session.ts must not expose getAccessToken")
	}
	if !strings.Contains(session, "csrfToken = undefined") {
		t.Error("cookie-mode clearSession must drop the in-memory CSRF token")
	}
	login := string(files["frontend/src/pages/LoginPage.tsx"])
	if login == "" {
		t.Fatal("missing frontend/src/pages/LoginPage.tsx")
	}
	if strings.Count(login, "await bootstrapCSRF()") < 2 {
		t.Error("cookie-mode LoginPage.tsx must await bootstrapCSRF() on both login and register")
	}
	if !strings.Contains(login, "void bootstrapCSRF()") {
		t.Error("cookie-mode LoginPage.tsx missing eager CSRF bootstrap on mount")
	}
	client := string(files["frontend/src/api/client.ts"])
	if client == "" {
		t.Fatal("missing frontend/src/api/client.ts")
	}
	if !strings.Contains(client, "X-CSRF-Token") {
		t.Error("cookie-mode client.ts missing X-CSRF-Token double-submit")
	}
	retry := string(files["frontend/src/api/retry.ts"])
	if retry == "" {
		t.Fatal("missing frontend/src/api/retry.ts")
	}
	assert401RetryReusesBufferedBody(t, client, retry, []string{`headers.set("X-CSRF-Token", token)`})
	if !strings.Contains(client, "csrfInFlight") {
		t.Error("cookie-mode client.ts missing csrfInFlight lock")
	}
	if !strings.Contains(client, "if (!force && readCSRFCookie())") {
		t.Error("cookie-mode client.ts must skip bootstrap when the gombit_csrf cookie is already present (#250)")
	}
	if !strings.Contains(client, "response.status === 403") {
		t.Error("cookie-mode client.ts missing 403 CSRF recovery retry (#250)")
	}
	providers := string(files["frontend/src/app/providers.tsx"])
	if !strings.Contains(providers, "void bootstrapCSRF()") {
		t.Error("cookie-mode AppProviders must bootstrap CSRF on mount")
	}
	if !strings.Contains(client, "await bootstrapCSRF()") {
		t.Error("cookie-mode client.ts must await bootstrapCSRF before unsafe requests")
	}
	assertNumberInputEmptyIsZero(t, string(files["frontend/src/pages/ProductFormPage.tsx"]))
	assertRuntimeAPIPrefix(t, files, "cookie")
}

func assertMUIFrontendInvariants(t *testing.T, files fileMap) {
	t.Helper()
	pkg := string(files["frontend/package.json"])
	if pkg == "" {
		t.Fatal("missing frontend/package.json")
	}
	if !strings.Contains(pkg, `"@mui/material"`) {
		t.Error("MUI package.json missing @mui/material")
	}
	if _, ok := files["frontend/src/theme.ts"]; !ok {
		t.Error("MUI tree missing frontend/src/theme.ts")
	}
	providers := string(files["frontend/src/app/providers.tsx"])
	if !strings.Contains(providers, "ThemeProvider") || !strings.Contains(providers, "CssBaseline") {
		t.Error("MUI providers.tsx missing ThemeProvider/CssBaseline")
	}
	layout := string(files["frontend/src/layouts/AppLayout.tsx"])
	login := string(files["frontend/src/pages/LoginPage.tsx"])
	list := string(files["frontend/src/pages/ProductListPage.tsx"])
	if !strings.Contains(layout, "AppBar") && !strings.Contains(list, "Table") {
		t.Error("MUI screens missing AppBar and Table")
	}
	if !strings.Contains(layout, "AppBar") {
		t.Error("MUI AppLayout.tsx missing AppBar")
	}
	if !strings.Contains(list, "Table") {
		t.Error("MUI ProductListPage.tsx missing Table")
	}
	if !strings.Contains(login, "TextField") && !strings.Contains(login, "Paper") {
		t.Error("MUI LoginPage.tsx missing Paper/TextField")
	}
	assertNumberInputEmptyIsZero(t, string(files["frontend/src/pages/ProductFormPage.tsx"]))
	assertRuntimeAPIPrefix(t, files, "jwt")
}

// assert401RetryReusesBufferedBody is the #106 contract: 401 retries must
// not clone a consumed Request, must not gate on Request.body (Firefox),
// and must resend the buffered body.
func assert401RetryReusesBufferedBody(t *testing.T, client, retry string, extra []string) {
	t.Helper()
	if strings.Contains(client, "new Request(request") {
		t.Error("client.ts 401 retry clones a consumed Request; rebuild from buffered body bytes instead")
	}
	for _, src := range []string{client, retry} {
		if strings.Contains(src, "request.body !=") || strings.Contains(src, "request.body !==") {
			t.Error("401 retry must not gate on Request.body (undefined in Firefox); gate on method")
		}
	}
	for _, want := range append([]string{
		"refreshInFlight",
		"isAuthURL",
		`from "./retry"`,
		"bufferRetryBody",
		"fetch(request.url",
		"retryInit(",
	}, extra...) {
		if !strings.Contains(client, want) {
			t.Errorf("client.ts missing %q", want)
		}
	}
	for _, want := range []string{
		`request.method !== "GET" && request.method !== "HEAD"`,
		"request.clone().arrayBuffer()",
		"init.body = body",
		"signal: request.signal",
		"mode: request.mode",
		"cache: request.cache",
		"redirect: request.redirect",
		"referrer: request.referrer",
		"referrerPolicy: request.referrerPolicy",
		"integrity: request.integrity",
		"keepalive: request.keepalive",
	} {
		if !strings.Contains(retry, want) {
			t.Errorf("retry.ts missing %q", want)
		}
	}
}

func assertRuntimeAPIPrefix(t *testing.T, files fileMap, auth string) {
	t.Helper()
	index := string(files["frontend/index.html"])
	if !strings.Contains(index, "__GOMBIT_API_PREFIX__") {
		t.Error("index.html missing __GOMBIT_API_PREFIX__ placeholder")
	}
	if _, ok := files["frontend/src/api/apiPrefix.ts"]; !ok {
		t.Fatal("missing frontend/src/api/apiPrefix.ts")
	}
	prefix := string(files["frontend/src/api/apiPrefix.ts"])
	for _, want := range []string{"export function apiPrefix()", "rewriteAPIRequest", "__GOMBIT_API_PREFIX__"} {
		if !strings.Contains(prefix, want) {
			t.Errorf("apiPrefix.ts missing %q", want)
		}
	}
	client := string(files["frontend/src/api/client.ts"])
	if !strings.Contains(client, `from "./apiPrefix"`) || !strings.Contains(client, "rewriteAPIRequest(") {
		t.Error("client.ts must rewrite typed /api/v1 paths through apiPrefix")
	}
	if strings.Contains(client, `const CSRF_PATH = "/api/v1/`) || strings.Contains(client, `const REFRESH_PATH = "/api/v1/`) {
		t.Error("client.ts hardcodes CSRF/REFRESH paths to /api/v1")
	}
	if auth == "cookie" {
		if !strings.Contains(client, `apiPath("/auth/csrf")`) || !strings.Contains(client, `apiPath("/auth/refresh")`) {
			t.Error("cookie client.ts must build CSRF/refresh URLs with apiPath()")
		}
	}
	vite := string(files["frontend/vite.config.ts"])
	if !strings.Contains(vite, "GOMBIT_API_PREFIX") || !strings.Contains(vite, "injectAPIPrefix") {
		t.Error("vite.config.ts must inject GOMBIT_API_PREFIX during vite dev")
	}
	if !strings.Contains(vite, `"/admin"`) {
		t.Error("vite.config.ts must proxy /admin to the Go origin")
	}
	readme := string(files["frontend/README.md"])
	if !strings.Contains(readme, "A CDN must replace `__GOMBIT_API_PREFIX__`") {
		t.Error("frontend/README.md must document split-deploy placeholder substitution")
	}
	if !strings.Contains(readme, "`/admin`") {
		t.Error("frontend/README.md must mention the Vite /admin proxy")
	}
	envExample := string(files[".env.example"])
	if !strings.Contains(envExample, "A CDN must replace __GOMBIT_API_PREFIX__") {
		t.Error(".env.example must document split-deploy placeholder substitution")
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root %s: %v", root, err)
	}
	return root
}

func goldenDir(name string) string {
	return filepath.Join(goldenRoot, name)
}

func snapshotTree(t *testing.T, root string) fileMap {
	t.Helper()
	fsys := os.DirFS(root)
	out := make(fileMap)
	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if _, skip := skipSnapshotDirs[d.Name()]; skip {
				return fs.SkipDir
			}
			return nil
		}
		if d.Name() == ".env" {
			return nil
		}
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(path)
		out[rel] = normalizeContent(rel, data)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	if len(out) == 0 {
		t.Fatalf("snapshot %s: no files", root)
	}
	return out
}

func normalizeContent(rel string, data []byte) []byte {
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	if filepath.Base(rel) == "go.mod" {
		// Keep goldens free of version churn; compile tests pin the local module
		// with a replace in a temp copy, never in committed trees.
		data = gombitRequirePattern.ReplaceAll(data, []byte("${1}v0.0.0"))
	}
	return data
}

func assertNoReplace(t *testing.T, files fileMap) {
	t.Helper()
	for rel, data := range files {
		if filepath.Base(rel) != "go.mod" {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "replace ") {
				t.Errorf("%s contains a replace directive; goldens must not bake machine-specific paths", rel)
			}
		}
	}
}

func assertNumberInputEmptyIsZero(t *testing.T, form string) {
	t.Helper()
	if form == "" {
		t.Fatal("missing frontend/src/pages/ProductFormPage.tsx")
	}
	if strings.Contains(form, "valueAsNumber") {
		t.Error("ProductFormPage.tsx uses valueAsNumber; empty number inputs become NaN and JSON.stringify emits null")
	}
	if !strings.Contains(form, `=== "" ? 0`) {
		t.Error("ProductFormPage.tsx number input does not coerce empty to 0")
	}
}

func loadGolden(t *testing.T, name string) fileMap {
	t.Helper()
	dir := goldenDir(name)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("missing golden %s (%s); run go test ./goldentest -update", name, dir)
	}
	return snapshotTree(t, dir)
}

func writeGolden(t *testing.T, name string, files fileMap) {
	t.Helper()
	dir := goldenDir(name)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("remove golden %s: %v", dir, err)
	}
	rels := make([]string, 0, len(files))
	for rel := range files {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, files[rel], 0o600); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
	}
}

func compareTrees(t *testing.T, got, want fileMap) {
	t.Helper()
	var extra, missing []string
	for rel := range got {
		if _, ok := want[rel]; !ok {
			extra = append(extra, rel)
		}
	}
	for rel := range want {
		if _, ok := got[rel]; !ok {
			missing = append(missing, rel)
		}
	}
	sort.Strings(extra)
	sort.Strings(missing)
	if len(extra) > 0 {
		t.Errorf("generated tree has unexpected files: %s", strings.Join(extra, ", "))
	}
	if len(missing) > 0 {
		t.Errorf("generated tree is missing files: %s", strings.Join(missing, ", "))
	}
	var diffs []string
	for rel, wantData := range want {
		gotData, ok := got[rel]
		if !ok {
			continue
		}
		if bytes.Equal(gotData, wantData) {
			continue
		}
		diffs = append(diffs, rel+": "+describeDiff(gotData, wantData))
	}
	sort.Strings(diffs)
	for _, d := range diffs {
		t.Error(d)
	}
}

func describeDiff(got, want []byte) string {
	gotLines := strings.Split(string(got), "\n")
	wantLines := strings.Split(string(want), "\n")
	n := min(len(gotLines), len(wantLines))
	for i := 0; i < n; i++ {
		if gotLines[i] == wantLines[i] {
			continue
		}
		return fmt.Sprintf("line %d:\n  got:  %s\n  want: %s", i+1, preview(gotLines[i]), preview(wantLines[i]))
	}
	if len(gotLines) != len(wantLines) {
		return fmt.Sprintf("line count got %d want %d", len(gotLines), len(wantLines))
	}
	return fmt.Sprintf("bytes got %d want %d", len(got), len(want))
}

func preview(s string) string {
	if len(s) > maxDiffPreview {
		return fmt.Sprintf("%q...", s[:maxDiffPreview])
	}
	return fmt.Sprintf("%q", s)
}

func treesEqual(a, b fileMap) bool {
	if len(a) != len(b) {
		return false
	}
	for rel, data := range a {
		other, ok := b[rel]
		if !ok || !bytes.Equal(data, other) {
			return false
		}
	}
	return true
}

func treeDiffSummary(a, b fileMap) string {
	var parts []string
	for rel := range a {
		if _, ok := b[rel]; !ok {
			parts = append(parts, "- extra "+rel)
			continue
		}
		if !bytes.Equal(a[rel], b[rel]) {
			parts = append(parts, "- changed "+rel)
		}
	}
	for rel := range b {
		if _, ok := a[rel]; !ok {
			parts = append(parts, "- missing "+rel)
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}

func assertGoFormatted(t *testing.T, files fileMap) {
	t.Helper()
	for rel, data := range files {
		if !strings.HasSuffix(rel, ".go") {
			continue
		}
		formatted, err := format.Source(data)
		if err != nil {
			t.Errorf("%s is not valid Go: %v", rel, err)
			continue
		}
		if !bytes.Equal(data, formatted) {
			t.Errorf("%s is not gofmt-clean", rel)
		}
	}
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(dst, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", dst, err)
	}
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		t.Fatalf("copy %s -> %s: %v", src, dst, err)
	}
}
