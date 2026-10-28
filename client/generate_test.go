package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/contract"
)

func TestGenerateWritesCompilingClient(t *testing.T) {
	requireNode(t)

	workDir := t.TempDir()
	specPath := writeSampleSpec(t, workDir)
	outDir := filepath.Join(workDir, "frontend", "src", "api", "generated")
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)

	err := Generate(context.Background(), Options{
		WorkDir:  workDir,
		SpecPath: specPath,
		OutDir:   outDir,
		Stdout:   stdout,
		Stderr:   stderr,
	})
	if err != nil {
		t.Fatalf("Generate() error = %v; stderr=%s", err, stderr.String())
	}
	for _, name := range []string{"schema.ts", "client.ts", "error.ts"} {
		if _, err := os.Stat(filepath.Join(outDir, name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	if !strings.Contains(stdout.String(), "created") {
		t.Fatalf("stdout = %q, want created files", stdout.String())
	}
	if !strings.Contains(stdout.String(), openapiFetchPackage) {
		t.Fatalf("stdout = %q, want %s dependency hint", stdout.String(), openapiFetchPackage)
	}

	schema := readFile(t, filepath.Join(outDir, "schema.ts"))
	if !strings.Contains(schema, generatedBanner) {
		t.Fatal("schema.ts missing generated banner")
	}
	if !strings.Contains(schema, "/api/v1/widgets") {
		t.Fatalf("schema.ts missing sample path; head=%s", truncate(schema, 400))
	}
	if strings.Contains(schema, "/raw/ping") {
		t.Fatal("schema.ts unexpectedly contains raw Gin route")
	}

	errorSrc := readFile(t, filepath.Join(outDir, "error.ts"))
	for _, want := range []string{"D10ErrorBody", "request_id", "fields", "ContractError"} {
		if !strings.Contains(errorSrc, want) {
			t.Fatalf("error.ts missing %s", want)
		}
	}
	clientSrc := readFile(t, filepath.Join(outDir, "client.ts"))
	if strings.Contains(clientSrc, "localStorage") || strings.Contains(clientSrc, "sessionStorage") {
		t.Fatal("generated client must not use web storage for tokens")
	}
	if !strings.Contains(clientSrc, "getAccessToken") {
		t.Fatal("client.ts missing in-memory token hook")
	}

	typecheckGenerated(t, workDir, outDir)
}

func TestGenerateNormalizesCustomPrefixPathKeys(t *testing.T) {
	requireNode(t)

	workDir := t.TempDir()
	specPath := writeSpecWithPrefix(t, workDir, "/svc/v2")
	raw := readFile(t, specPath)
	if !strings.Contains(raw, `"/svc/v2/widgets"`) {
		t.Fatalf("precondition: spec missing live /svc/v2 path: %s", truncate(raw, 200))
	}
	if strings.Contains(raw, `"/api/v1/widgets"`) {
		t.Fatal("precondition: live spec unexpectedly already uses /api/v1 path keys")
	}

	outDir := filepath.Join(workDir, "frontend", "src", "api", "generated")
	stderr := new(bytes.Buffer)
	err := Generate(context.Background(), Options{
		WorkDir:  workDir,
		SpecPath: specPath,
		OutDir:   outDir,
		Stdout:   ioDiscard{},
		Stderr:   stderr,
	})
	if err != nil {
		t.Fatalf("Generate() error = %v; stderr=%s", err, stderr.String())
	}

	// The committed / on-disk OpenAPI document keeps live Huma paths
	// (contract-drift compares that document, not the typed client keys).
	if got := readFile(t, specPath); !strings.Contains(got, `"/svc/v2/widgets"`) {
		t.Fatal("Generate() rewrote the caller's OpenAPI document")
	}

	schema := readFile(t, filepath.Join(outDir, "schema.ts"))
	if !strings.Contains(schema, `"/api/v1/widgets"`) {
		t.Fatalf("schema.ts missing typed /api/v1/widgets key; head=%s", truncate(schema, 400))
	}
	if strings.Contains(schema, "/svc/v2/") {
		t.Fatalf("schema.ts still has live prefix /svc/v2: %s", truncate(schema, 400))
	}

	typecheckGenerated(t, workDir, outDir)
}

// The application's own prefix, not a reading of its paths, is what decides
// the typed keys (#506). This app's routes all share one first segment, so no
// path list could have told /svc from /svc/auth.
func TestGenerateNormalizesConfiguredPrefixPathKeys(t *testing.T) {
	requireNode(t)

	workDir := t.TempDir()
	specPath := filepath.Join(workDir, "openapi.json")
	writeFile(t, specPath, authOnlySpec(t, "/svc"))

	outDir := filepath.Join(workDir, "frontend", "src", "api", "generated")
	stderr := new(bytes.Buffer)
	err := Generate(context.Background(), Options{
		WorkDir:   workDir,
		SpecPath:  specPath,
		OutDir:    outDir,
		APIPrefix: "/svc",
		Stdout:    ioDiscard{},
		Stderr:    stderr,
	})
	if err != nil {
		t.Fatalf("Generate() error = %v; stderr=%s", err, stderr.String())
	}

	schema := readFile(t, filepath.Join(outDir, "schema.ts"))
	for _, want := range []string{`"/api/v1/auth/login"`, `"/api/v1/auth/me"`} {
		if !strings.Contains(schema, want) {
			t.Fatalf("schema.ts missing the typed key %s; head=%s", want, truncate(schema, 400))
		}
	}
	if strings.Contains(schema, "/svc/") {
		t.Fatalf("schema.ts still has the live prefix: %s", truncate(schema, 400))
	}
}

// A configured prefix the document does not use is refused before the
// generator runs and before anything is written: the alternative is a client
// whose keys belong to no route of the app (#506).
func TestGenerateRefusesAConfiguredPrefixTheDocumentDoesNotUse(t *testing.T) {
	workDir := t.TempDir()
	specPath := filepath.Join(workDir, "openapi.json")
	writeFile(t, specPath, authOnlySpec(t, "/svc"))
	outDir := filepath.Join(workDir, "out")

	err := Generate(context.Background(), Options{
		WorkDir:   workDir,
		SpecPath:  specPath,
		OutDir:    outDir,
		APIPrefix: "/other",
		Stdout:    ioDiscard{},
		Stderr:    ioDiscard{},
	})
	if err == nil {
		t.Fatal("Generate() error = nil, want the configured prefix reported")
	}
	if !strings.Contains(err.Error(), `matches no path`) {
		t.Fatalf("Generate() error = %v, want it to report the prefix matching no path", err)
	}
	if _, statErr := os.Stat(outDir); !os.IsNotExist(statErr) {
		t.Fatalf("Generate() wrote %s for a document it refused: %v", outDir, statErr)
	}
}

// authOnlySpec is an OpenAPI document whose operations all sit under one group
// of one prefix, the shape no path list can resolve (#506). Its int64 maximum is
// the largest int64, so the spec rewrite the generator runs has a number a
// float64 round trip could not keep; openapi-typescript emits no bounds at all
// (`id?: number`), so client/paths_test.go is where that fidelity is asserted.
func authOnlySpec(t *testing.T, prefix string) string {
	t.Helper()
	spec := `{
  "openapi": "3.1.0",
  "info": {"title": "auth only", "version": "0.0.0"},
  "paths": {
    "` + prefix + `/auth/login": {
      "post": {"operationId": "login", "responses": {"204": {"description": "no content"}}}
    },
    "` + prefix + `/auth/me": {
      "get": {
        "operationId": "me",
        "responses": {
          "200": {
            "description": "ok",
            "content": {
              "application/json": {
                "schema": {
                  "type": "object",
                  "properties": {
                    "id": {"type": "integer", "format": "int64", "maximum": 9223372036854775807}
                  }
                }
              }
            }
          }
        }
      }
    }
  }
}`
	if !json.Valid([]byte(spec)) {
		t.Fatalf("authOnlySpec(%q) is not valid JSON: %s", prefix, spec)
	}
	return spec
}

func TestGenerateDryRunDoesNotWrite(t *testing.T) {
	workDir := t.TempDir()
	specPath := writeSampleSpec(t, workDir)
	outDir := filepath.Join(workDir, "out")
	stdout := new(bytes.Buffer)

	err := Generate(context.Background(), Options{
		WorkDir:  workDir,
		SpecPath: specPath,
		OutDir:   outDir,
		DryRun:   true,
		Stdout:   stdout,
	})
	if err != nil {
		t.Fatalf("Generate(dry-run) error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "schema.ts")); !os.IsNotExist(err) {
		t.Fatal("dry-run wrote schema.ts")
	}
	if !strings.Contains(stdout.String(), "create") {
		t.Fatalf("dry-run stdout = %q, want create", stdout.String())
	}
}

func TestGenerateDryRunRefusesUserOwnedFile(t *testing.T) {
	workDir := t.TempDir()
	specPath := writeSampleSpec(t, workDir)
	outDir := filepath.Join(workDir, "out")
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	owned := filepath.Join(outDir, "client.ts")
	if err := os.WriteFile(owned, []byte("export const mine = true;\n"), 0o600); err != nil {
		t.Fatalf("write user file: %v", err)
	}

	stdout := new(bytes.Buffer)
	err := Generate(context.Background(), Options{
		WorkDir:  workDir,
		SpecPath: specPath,
		OutDir:   outDir,
		DryRun:   true,
		Stdout:   stdout,
	})
	if err == nil {
		t.Fatal("Generate(dry-run) error = nil, want refuse overwrite")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Fatalf("Generate(dry-run) error = %q, want --force", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("dry-run stdout = %q, want empty after refuse", stdout.String())
	}
	if got := readFile(t, owned); got != "export const mine = true;\n" {
		t.Fatal("dry-run changed user-owned client.ts")
	}

	stdout.Reset()
	err = Generate(context.Background(), Options{
		WorkDir:  workDir,
		SpecPath: specPath,
		OutDir:   outDir,
		DryRun:   true,
		Force:    true,
		Stdout:   stdout,
	})
	if err != nil {
		t.Fatalf("Generate(dry-run, force) error = %v", err)
	}
	if !strings.Contains(stdout.String(), "modify") {
		t.Fatalf("dry-run force stdout = %q, want modify", stdout.String())
	}
	if got := readFile(t, owned); got != "export const mine = true;\n" {
		t.Fatal("dry-run force wrote client.ts")
	}
}

func TestGenerateRefusesUserOwnedFile(t *testing.T) {
	requireNode(t)

	workDir := t.TempDir()
	specPath := writeSampleSpec(t, workDir)
	outDir := filepath.Join(workDir, "out")
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	owned := filepath.Join(outDir, "client.ts")
	if err := os.WriteFile(owned, []byte("export const mine = true;\n"), 0o600); err != nil {
		t.Fatalf("write user file: %v", err)
	}

	err := Generate(context.Background(), Options{
		WorkDir:  workDir,
		SpecPath: specPath,
		OutDir:   outDir,
	})
	if err == nil {
		t.Fatal("Generate() error = nil, want refuse overwrite")
	}
	if !errors.Is(err, ErrNotGenerated) {
		t.Fatalf("Generate() error = %v, want ErrNotGenerated", err)
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Fatalf("Generate() error = %q, want --force", err)
	}

	stdout := new(bytes.Buffer)
	err = Generate(context.Background(), Options{
		WorkDir:  workDir,
		SpecPath: specPath,
		OutDir:   outDir,
		DryRun:   true,
		Stdout:   stdout,
	})
	if err == nil {
		t.Fatal("Generate(dry-run) error = nil, want refuse overwrite")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Fatalf("Generate(dry-run) error = %q, want --force", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("dry-run stdout = %q, want empty after refuse", stdout.String())
	}
	if got := readFile(t, owned); got != "export const mine = true;\n" {
		t.Fatal("dry-run changed user-owned client.ts")
	}

	err = Generate(context.Background(), Options{
		WorkDir:  workDir,
		SpecPath: specPath,
		OutDir:   outDir,
		Force:    true,
		Stdout:   ioDiscard{},
		Stderr:   ioDiscard{},
	})
	if err != nil {
		t.Fatalf("Generate(force) error = %v", err)
	}
	got := readFile(t, owned)
	if !strings.Contains(got, generatedBanner) {
		t.Fatal("force did not replace user-owned client.ts")
	}
}

func TestGenerateIsIdempotent(t *testing.T) {
	requireNode(t)

	workDir := t.TempDir()
	specPath := writeSampleSpec(t, workDir)
	outDir := filepath.Join(workDir, "out")
	opts := Options{
		WorkDir:  workDir,
		SpecPath: specPath,
		OutDir:   outDir,
		Stdout:   ioDiscard{},
		Stderr:   ioDiscard{},
	}
	if err := Generate(context.Background(), opts); err != nil {
		t.Fatalf("first Generate() error = %v", err)
	}
	first := map[string]string{
		"schema.ts": readFile(t, filepath.Join(outDir, "schema.ts")),
		"client.ts": readFile(t, filepath.Join(outDir, "client.ts")),
		"error.ts":  readFile(t, filepath.Join(outDir, "error.ts")),
	}
	if err := Generate(context.Background(), opts); err != nil {
		t.Fatalf("second Generate() error = %v", err)
	}
	for _, name := range []string{"schema.ts", "client.ts", "error.ts"} {
		second := readFile(t, filepath.Join(outDir, name))
		if first[name] != second {
			t.Fatalf("re-generate changed %s", name)
		}
	}
}

func TestGenerateRequiresSpec(t *testing.T) {
	err := Generate(context.Background(), Options{
		WorkDir:  t.TempDir(),
		SpecPath: "missing.json",
	})
	if err == nil {
		t.Fatal("Generate() error = nil, want missing spec")
	}
}

func TestGenerateRejectsEmptySpec(t *testing.T) {
	workDir := t.TempDir()
	path := filepath.Join(workDir, "openapi.json")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("write empty spec: %v", err)
	}
	err := Generate(context.Background(), Options{
		WorkDir:  workDir,
		SpecPath: path,
	})
	if err == nil {
		t.Fatal("Generate() error = nil, want empty spec")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Fatalf("Generate() error = %q, want empty", err)
	}
}

func TestGenerateRejectsNon31Spec(t *testing.T) {
	workDir := t.TempDir()
	path := filepath.Join(workDir, "openapi.json")
	if err := os.WriteFile(path, []byte(`{"openapi":"3.0.3","info":{"title":"t","version":"0"},"paths":{}}`), 0o600); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	err := Generate(context.Background(), Options{
		WorkDir:  workDir,
		SpecPath: path,
	})
	if err == nil {
		t.Fatal("Generate() error = nil, want non-3.1 spec")
	}
	if !strings.Contains(err.Error(), "3.1") {
		t.Fatalf("Generate() error = %q, want 3.1", err)
	}
}

func writeSampleSpec(t *testing.T, workDir string) string {
	t.Helper()
	return writeSpecWithPrefix(t, workDir, "")
}

func writeSpecWithPrefix(t *testing.T, workDir, prefix string) string {
	t.Helper()
	app, err := sampleAppWithPrefix(prefix)
	if err != nil {
		t.Fatalf("sampleAppWithPrefix(%q) error = %v", prefix, err)
	}
	path := filepath.Join(workDir, "openapi.json")
	if err := contract.WriteOpenAPI(path, app.API()); err != nil {
		t.Fatalf("WriteOpenAPI: %v", err)
	}
	return path
}

func typecheckGenerated(t *testing.T, workDir, outDir string) {
	t.Helper()

	relOut, err := filepath.Rel(workDir, outDir)
	if err != nil {
		t.Fatalf("rel: %v", err)
	}
	relOut = filepath.ToSlash(relOut)

	writeFile(t, filepath.Join(workDir, "package.json"), `{
  "name": "gombit-client-compile",
  "private": true,
  "type": "module",
  "dependencies": {
    "openapi-fetch": "`+strings.TrimPrefix(openapiFetchPackage, "openapi-fetch@")+`"
  },
  "devDependencies": {
    "typescript": "5.9.3"
  }
}
`)
	writeFile(t, filepath.Join(workDir, "tsconfig.json"), `{
  "compilerOptions": {
    "target": "ES2022",
    "module": "ES2022",
    "moduleResolution": "bundler",
    "strict": true,
    "skipLibCheck": true,
    "noEmit": true
  },
  "include": ["usage.ts", "`+relOut+`/**/*.ts"]
}
`)
	writeFile(t, filepath.Join(workDir, "usage.ts"), `
import {
  ContractError,
  createGombitClient,
  isD10ErrorBody,
  unwrap,
} from "./`+relOut+`/client";

const client = createGombitClient({
  baseUrl: "http://127.0.0.1:8080",
  getAccessToken: () => undefined,
});

export async function listWidgets() {
  const result = await client.GET("/api/v1/widgets");
  if (result.error) {
    if (isD10ErrorBody(result.error)) {
      const code: string = result.error.error.code;
      const fields = result.error.error.fields;
      void code;
      void fields;
    }
    throw ContractError.fromResponse(result.response, result.error);
  }
  return unwrap(result);
}

export async function createWidget() {
  return unwrap(await client.POST("/api/v1/widgets", {
    body: { name: "Second widget", color: "green" },
  }));
}
`)

	install := exec.Command("npm", "install", "--no-fund", "--no-audit")
	install.Dir = workDir
	install.Env = append(os.Environ(), "npm_config_update_notifier=false")
	if out, err := install.CombinedOutput(); err != nil {
		t.Fatalf("npm install: %v\n%s", err, out)
	}
	//nolint:gosec // workDir is t.TempDir; tsc is invoked with fixed args
	tsc := exec.Command("npx", "--no-install", "tsc", "--noEmit", "-p", workDir)
	tsc.Dir = workDir
	if out, err := tsc.CombinedOutput(); err != nil {
		t.Fatalf("tsc --noEmit: %v\n%s", err, out)
	}
}

func requireNode(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("npx openapi-typescript path handling differs on Windows")
	}
	if _, err := exec.LookPath("npx"); err != nil {
		t.Skip("npx not available")
	}
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm not available")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	// #nosec G304 -- test fixture path
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) {
	return len(p), nil
}
