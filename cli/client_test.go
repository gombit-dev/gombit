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
	dir := t.TempDir()
	// #nosec G304 -- test-controlled path under t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("GOMBIT_API_PREFIX=/svc\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	if got := liveAPIPrefix("", dir); got != "/svc" {
		t.Fatalf(`liveAPIPrefix("") = %q, want /svc from the project's .env`, got)
	}
	if got := liveAPIPrefix("/flag", dir); got != "/flag" {
		t.Fatalf(`liveAPIPrefix("/flag") = %q, want the flag to win over .env`, got)
	}
}

// Configuration that cannot be read is reported as none rather than as
// D8's default: the generator then infers the prefix from the document, which
// is what it did before the prefix became an input.
func TestLiveAPIPrefixIsUnsetWhenTheConfigurationIsInvalid(t *testing.T) {
	dir := t.TempDir()
	// #nosec G304 -- test-controlled path under t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("GOMBIT_HTTP_REQUEST_TIMEOUT=not-a-duration\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	if got := liveAPIPrefix("", dir); got != "" {
		t.Fatalf("liveAPIPrefix() = %q, want the empty string for unreadable configuration", got)
	}
}

func TestClientCommandsTakeAnAPIPrefixFlag(t *testing.T) {
	tests := []struct {
		name string
		cmd  []string
	}{
		{name: "generate", cmd: []string{"client", "generate", "--help"}},
		{name: "check", cmd: []string{"client", "check", "--help"}},
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
