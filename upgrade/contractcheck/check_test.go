package contractcheck_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/client"
	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/upgrade/contractcheck"
)

func needNPX(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("npx openapi-typescript path handling differs on Windows")
	}
	if _, err := exec.LookPath("npx"); err != nil {
		t.Skip("npx not available")
	}
}

// sampleSpec is the framework's sample API document.
func sampleSpec(t *testing.T) []byte {
	t.Helper()
	app, err := client.SampleApp()
	if err != nil {
		t.Fatal(err)
	}
	spec, err := contract.OpenAPIJSON(app.API())
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

// clientApp is a directory with a committed document (spec) and the client
// gombit client generate makes from it, at gombit new's paths.
func clientApp(t *testing.T, spec []byte) string {
	t.Helper()
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"openapi.json": string(spec)})
	if err := client.Generate(context.Background(), client.Options{WorkDir: dir, SpecPath: filepath.Join(dir, "openapi.json"), OutDir: filepath.Join(dir, client.DefaultOutDir)}); err != nil {
		t.Fatal(err)
	}
	return dir
}

func check(r contractcheck.Report, a contractcheck.Artifact) contractcheck.Result {
	for _, c := range r.Checks {
		if c.Artifact == a {
			return c
		}
	}
	return contractcheck.Result{}
}

// TestCheckContractClient: the client is checked against what the
// committed document generates; a changed file is stale, --write
// regenerates exactly the generated files (a handwritten file beside them
// is untouched), and the document is unchecked without the live one.
func TestCheckContractClient(t *testing.T) {
	needNPX(t)
	dir := clientApp(t, sampleSpec(t))
	out := filepath.Join(dir, filepath.FromSlash(client.DefaultOutDir))
	r, err := contractcheck.Check(context.Background(), dir, contractcheck.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if c := check(r, contractcheck.Client); c.Status != contractcheck.Current {
		t.Fatalf("client = %+v", c)
	}
	if c := check(r, contractcheck.OpenAPI); c.Status != contractcheck.Unchecked || !strings.Contains(c.Reason, "--url") {
		t.Fatalf("openapi = %+v", c)
	}
	if c := check(r, contractcheck.Resources); c.Status != contractcheck.Absent || r.Stale() {
		t.Fatalf("resources = %+v", c)
	}
	// --write with nothing stale writes nothing (no client from nothing
	// either: an absent client stays absent).
	empty := t.TempDir()
	writeTree(t, empty, map[string]string{"openapi.json": string(sampleSpec(t))})
	if r, err := contractcheck.Check(context.Background(), empty, contractcheck.Options{Write: true}); err != nil || check(r, contractcheck.Client).Status != contractcheck.Absent {
		t.Fatalf("--write with no client: %+v, %v", r, err)
	}
	if _, err := os.Stat(filepath.Join(empty, filepath.FromSlash(client.DefaultOutDir))); !os.IsNotExist(err) {
		t.Fatal("--write created a client that was not there")
	}

	// An upgrade changed what the generator renders: here, a file is off.
	clientTS := filepath.Join(out, "client.ts")
	original := read(t, clientTS)
	writeTree(t, out, map[string]string{"client.ts": original + "// edited\n", "custom.ts": "export const mine = 1;\n"})
	r, err = contractcheck.Check(context.Background(), dir, contractcheck.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if c := check(r, contractcheck.Client); c.Status != contractcheck.Stale || strings.Join(c.Stale, ",") != client.DefaultOutDir+"/client.ts" || !r.Stale() {
		t.Fatalf("client = %+v", c)
	}
	r, err = contractcheck.Check(context.Background(), dir, contractcheck.Options{Write: true})
	if err != nil {
		t.Fatal(err)
	}
	if c := check(r, contractcheck.Client); c.Status != contractcheck.Written || r.Stale() {
		t.Fatalf("client after --write = %+v", c)
	}
	if read(t, clientTS) != original || read(t, filepath.Join(out, "custom.ts")) != "export const mine = 1;\n" {
		t.Fatal("--write did not regenerate client.ts, or touched custom.ts")
	}
}

// TestCheckContractNeverOverwritesHandwritten: a file in the client
// directory gombit did not generate (no banner) is reported, not
// overwritten, and stays stale.
func TestCheckContractNeverOverwritesHandwritten(t *testing.T) {
	needNPX(t)
	dir := clientApp(t, sampleSpec(t))
	out := filepath.Join(dir, filepath.FromSlash(client.DefaultOutDir))
	handwritten := "// my own error helpers\nexport type E = string;\n"
	writeTree(t, out, map[string]string{"error.ts": handwritten})
	r, err := contractcheck.Check(context.Background(), dir, contractcheck.Options{Write: true})
	if err != nil {
		t.Fatal(err)
	}
	c := check(r, contractcheck.Client)
	if c.Status != contractcheck.Stale || !strings.Contains(c.Reason, "refuse to overwrite") || !r.Stale() {
		t.Fatalf("client = %+v", c)
	}
	if read(t, filepath.Join(out, "error.ts")) != handwritten {
		t.Fatal("a handwritten file was overwritten")
	}
}

// TestCheckContractLiveDocument: with the running app's document, the
// committed one is compared (semantically) and --write replaces it; the
// client is generated from the live one. An app that does not commit the
// document has none to report.
func TestCheckContractLiveDocument(t *testing.T) {
	needNPX(t)
	spec := sampleSpec(t)
	dir := clientApp(t, spec)
	live := strings.Replace(string(spec), `"title":`, `"x-live": true, "title":`, 1)
	if live == string(spec) {
		t.Fatal("could not alter the document")
	}
	r, err := contractcheck.Check(context.Background(), dir, contractcheck.Options{LiveSpec: spec})
	if err != nil {
		t.Fatal(err)
	}
	if c := check(r, contractcheck.OpenAPI); c.Status != contractcheck.Current {
		t.Fatalf("openapi with the same document = %+v", c)
	}
	r, err = contractcheck.Check(context.Background(), dir, contractcheck.Options{LiveSpec: []byte(live), Write: true})
	if err != nil {
		t.Fatal(err)
	}
	if c := check(r, contractcheck.OpenAPI); c.Status != contractcheck.Written || strings.TrimSuffix(read(t, filepath.Join(dir, "openapi.json")), "\n") != strings.TrimSuffix(live, "\n") {
		t.Fatalf("openapi after --write = %+v", c)
	}
	// The client was current (the change is an extension): left alone.
	if c := check(r, contractcheck.Client); c.Status != contractcheck.Current {
		t.Fatalf("a current client after --write = %+v; only stale artifacts are regenerated", c)
	}
	if err := os.Remove(filepath.Join(dir, "openapi.json")); err != nil {
		t.Fatal(err)
	}
	r, err = contractcheck.Check(context.Background(), dir, contractcheck.Options{LiveSpec: []byte(live)})
	if err != nil {
		t.Fatal(err)
	}
	if c := check(r, contractcheck.OpenAPI); c.Status != contractcheck.Absent {
		t.Fatalf("an uncommitted document = %+v", c)
	}
	if c := check(r, contractcheck.Client); c.Status != contractcheck.Current {
		t.Fatalf("client from the live document = %+v", c)
	}
	// With no committed document, --write regenerates a stale client from
	// the live one.
	clientTS := filepath.Join(dir, filepath.FromSlash(client.DefaultOutDir), "client.ts")
	good := read(t, clientTS)
	writeTree(t, filepath.Dir(clientTS), map[string]string{"client.ts": good + "// edited\n"})
	r, err = contractcheck.Check(context.Background(), dir, contractcheck.Options{LiveSpec: []byte(live), Write: true})
	if err != nil {
		t.Fatal(err)
	}
	if c := check(r, contractcheck.Client); c.Status != contractcheck.Written || read(t, clientTS) != good {
		t.Fatalf("client --write from the live document = %+v", c)
	}
}

// TestCheckMissingClientFile: a generated client file that is not there is
// stale (missing), not an error, and --write brings it back.
func TestCheckMissingClientFile(t *testing.T) {
	needNPX(t)
	dir := clientApp(t, sampleSpec(t))
	errorTS := filepath.Join(dir, filepath.FromSlash(client.DefaultOutDir), "error.ts")
	want := read(t, errorTS)
	if err := os.Remove(errorTS); err != nil {
		t.Fatal(err)
	}
	r, err := contractcheck.Check(context.Background(), dir, contractcheck.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if c := check(r, contractcheck.Client); c.Status != contractcheck.Stale || strings.Join(c.Stale, ",") != client.DefaultOutDir+"/error.ts (missing)" {
		t.Fatalf("client = %+v", c)
	}
	if _, err := contractcheck.Check(context.Background(), dir, contractcheck.Options{Write: true}); err != nil || read(t, errorTS) != want {
		t.Fatalf("--write: %v", err)
	}
}

// TestCheckResourceLoaderFailure: when the resource loader cannot run (the
// models do not compile, as after a breaking upgrade), the resources are
// unchecked, with the reason, and the other checks still report.
func TestCheckResourceLoaderFailure(t *testing.T) {
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "-mod=mod")
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"go.mod":                         "module testapp\n\ngo 1.23\n",
		"cmd/server/main.go":             "package main\n\nfunc main() {}\n",
		"internal/platform/database.go":  "package platform\n\nimport \"testapp/internal/book\"\n\nfunc AutoMigrate(db anyDB) error { return db.AutoMigrate(&book.Book{}) }\n",
		"internal/book/.gombit-resource": "",
		"internal/book/book.go":          "package book\n\ntype Book struct{ Title string }\n",
	})
	r, err := contractcheck.Check(context.Background(), dir, contractcheck.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if c := check(r, contractcheck.Resources); c.Status != contractcheck.Unchecked || !strings.Contains(c.Reason, "resource loader failed") {
		t.Fatalf("resources = %+v", c)
	}
	if len(r.Checks) != 3 {
		t.Fatalf("checks = %+v", r.Checks)
	}
}

// TestCheckContractAgreesWithTheDriftGate: on this repository's contract
// fixture, checked against the sample API, the check is clean exactly as
// CI's contract-drift job (gombit client check) requires: the same
// comparison, so upgrades and the drift gate agree.
func TestCheckContractAgreesWithTheDriftGate(t *testing.T) {
	needNPX(t)
	r, err := contractcheck.Check(context.Background(), filepath.Join("..", ".."), contractcheck.Options{
		SpecPath: client.SampleSpecPath, OutDir: client.SampleOutDir, LiveSpec: sampleSpec(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []contractcheck.Artifact{contractcheck.OpenAPI, contractcheck.Client} {
		if c := check(r, a); c.Status != contractcheck.Current {
			t.Errorf("%s = %+v; the contract-drift gate passes on this fixture", a, c)
		}
	}
	if err := client.CheckDrift(context.Background(), client.DriftOptions{WorkDir: filepath.Join("..", "..")}); err != nil {
		t.Fatalf("the drift gate itself: %v", err)
	}
}

// TestCheckContractWithoutDocumentOrNPX: a client with no document to
// generate it from, or no npx, is unchecked, with the reason.
func TestCheckContractWithoutDocumentOrNPX(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{client.DefaultOutDir + "/schema.ts": "// Code generated by gombit client generate. DO NOT EDIT.\n"})
	r, err := contractcheck.Check(context.Background(), dir, contractcheck.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if c := check(r, contractcheck.Client); c.Status != contractcheck.Unchecked || !strings.Contains(c.Reason, "no OpenAPI document") {
		t.Fatalf("client = %+v", c)
	}
	writeTree(t, dir, map[string]string{"openapi.json": "{}"})
	r, err = contractcheck.Check(context.Background(), dir, contractcheck.Options{NPXBinary: "no-such-npx-binary"})
	if err != nil {
		t.Fatal(err)
	}
	if c := check(r, contractcheck.Client); c.Status != contractcheck.Unchecked || !strings.Contains(c.Reason, "npx") {
		t.Fatalf("client = %+v", c)
	}
}

// writeTree writes files (path -> content) under dir.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil { // #nosec G703 -- under t.TempDir()
			t.Fatal(err)
		}
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- under t.TempDir()
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
