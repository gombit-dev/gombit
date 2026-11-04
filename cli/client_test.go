package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestClientCheckURLFetchesLiveSpecInsteadOfSampleApp exercises the path a
// generated app uses: gombit is a separately compiled binary with no
// Go-level access to the app's huma.API, so `client check --url` must fetch
// the live /openapi.json and compare/write against that, never against the
// framework's own SampleApp.
func TestClientCheckURLFetchesLiveSpecInsteadOfSampleApp(t *testing.T) {
	const liveSpec = `{"openapi":"3.1.0","info":{"title":"generated-app-from-url-test","version":"0.0.0"},"paths":{}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(liveSpec))
	}))
	defer srv.Close()

	dir := t.TempDir()
	t.Chdir(dir)

	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	err := ExecuteRoot(context.Background(), NewRoot(stdout, stderr), []string{
		"client", "check", "--write", "--url", srv.URL,
	})
	if err != nil {
		t.Fatalf("gombit client check --write --url: %v\nstderr: %s", err, stderr.String())
	}

	// #nosec G304 -- test-controlled path under t.TempDir()
	written, err := os.ReadFile(filepath.Join(dir, "openapi.json"))
	if err != nil {
		t.Fatalf("read written spec: %v", err)
	}
	if !strings.Contains(string(written), "generated-app-from-url-test") {
		t.Fatalf("written spec = %s, want content fetched from --url", written)
	}
}

// The prefix the typed client keys are rewritten from is the application's own
// configuration: --api-prefix for a caller that passes one, else what the
// project directory's .env says (#506). Nothing here runs openapi-typescript,
// so it needs no Node.js.
func TestLiveAPIPrefixComesFromTheApplicationConfiguration(t *testing.T) {
	unsetEnv(t, "GOMBIT_API_PREFIX")
	dir := t.TempDir()
	// #nosec G304 -- test-controlled path under t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("GOMBIT_API_PREFIX=/svc\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	if got, err := liveAPIPrefix("", dir); err != nil || got != "/svc" {
		t.Fatalf(`liveAPIPrefix("") = (%q, %v), want /svc from the project's .env`, got, err)
	}
	if got, err := liveAPIPrefix("/flag", dir); err != nil || got != "/flag" {
		t.Fatalf(`liveAPIPrefix("/flag") = (%q, %v), want the flag to win over .env`, got, err)
	}
}

// Configuration that does not mention the prefix is reported as none, not as
// D8's default: the generator then infers the prefix from the document, which
// is what it did before the prefix became an input. A defaulted Config would
// read /api/v1 here and refuse a document under /svc that used to rewrite
// correctly (#506).
func TestLiveAPIPrefixIsUnsetWhenTheConfigurationDoesNotSetIt(t *testing.T) {
	unsetEnv(t, "GOMBIT_API_PREFIX")
	dir := t.TempDir()
	if got, err := liveAPIPrefix("", dir); err != nil || got != "" {
		t.Fatalf("liveAPIPrefix() = (%q, %v), want the empty string for a project that never sets the prefix", got, err)
	}
}

// The variable is read wherever configuration is read from: the process
// environment, or the project's .env when the environment does not set it.
func TestLiveAPIPrefixReadsTheVariableNotTheDefaultedConfig(t *testing.T) {
	unsetEnv(t, "GOMBIT_API_PREFIX")
	dir := t.TempDir()
	// #nosec G304 -- test-controlled path under t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("GOMBIT_API_PREFIX=/svc\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	t.Setenv("GOMBIT_API_PREFIX", "/env")
	if got, err := liveAPIPrefix("", dir); err != nil || got != "/env" {
		t.Fatalf(`liveAPIPrefix("") = (%q, %v), want /env from the process environment`, got, err)
	}
}

// A .env that cannot be read is reported, not skipped: answering from an
// unreadable file would look exactly like a project that never set the prefix,
// and the generator would go back to guessing the one #506 stopped guessing.
func TestLiveAPIPrefixReportsAnUnreadableDotEnv(t *testing.T) {
	unsetEnv(t, "GOMBIT_API_PREFIX")
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".env"), 0o700); err != nil {
		t.Fatalf("create .env directory: %v", err)
	}
	if got, err := liveAPIPrefix("", dir); err == nil {
		t.Fatalf(`liveAPIPrefix("") = (%q, nil), want the .env read error`, got)
	}
}

func TestClientCommandsTakeAnAPIPrefixFlag(t *testing.T) {
	tests := []struct {
		name string
		cmd  []string
	}{
		{name: "client generate", cmd: []string{"client", "generate", "--help"}},
		{name: "client check", cmd: []string{"client", "check", "--help"}},
		{name: "db check", cmd: []string{"db", "check", "--help"}},
		{name: "upgrade contract", cmd: []string{"upgrade", "contract", "--help"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout := new(bytes.Buffer)
			stderr := new(bytes.Buffer)
			if err := ExecuteRoot(context.Background(), NewRoot(stdout, stderr), tt.cmd); err != nil {
				t.Fatalf("gombit %v: %v", tt.cmd, err)
			}
			out := stdout.String() + stderr.String()
			if !strings.Contains(out, "--api-prefix") {
				t.Fatalf("gombit %s --help has no --api-prefix flag:\n%s", tt.name, out)
			}
		})
	}
}

// unsetEnv removes key from the process environment for the test's duration,
// restoring whatever the developer's shell had (t.Setenv cannot unset).
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	value, had := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, value)
			return
		}
		_ = os.Unsetenv(key)
	})
}

func TestClientCheckDefaultsMatchGenerateNotExamplesClient(t *testing.T) {
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	err := ExecuteRoot(context.Background(), NewRoot(stdout, stderr), []string{"client", "check", "--help"})
	if err != nil {
		t.Fatalf("gombit client check --help: %v", err)
	}
	out := stdout.String() + stderr.String()
	if strings.Contains(out, `default "examples/client`) {
		t.Fatalf("client check --help still defaults to framework-repo-only examples/client paths:\n%s", out)
	}
	if !strings.Contains(out, `default "openapi.json"`) {
		t.Fatalf("client check --help missing openapi.json default:\n%s", out)
	}
	if !strings.Contains(out, `default "frontend/src/api/generated"`) {
		t.Fatalf("client check --help missing frontend/src/api/generated default:\n%s", out)
	}
	if !strings.Contains(out, "--url") {
		t.Fatalf("client check --help missing --url flag:\n%s", out)
	}
}
