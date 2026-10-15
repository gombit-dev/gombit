package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseDotEnvLine(t *testing.T) {
	tests := []struct {
		name      string
		line      string
		wantKey   string
		wantValue string
		wantOK    bool
	}{
		{name: "simple", line: "GOMBIT_JWT_SECRET=abc123", wantKey: "GOMBIT_JWT_SECRET", wantValue: "abc123", wantOK: true},
		{name: "blank", line: "", wantOK: false},
		{name: "comment", line: "# a comment", wantOK: false},
		{name: "indented comment", line: "  # indented", wantOK: false},
		{name: "no equals", line: "not an assignment", wantOK: false},
		{name: "empty value", line: "GOMBIT_DOCS_ENABLED=", wantKey: "GOMBIT_DOCS_ENABLED", wantValue: "", wantOK: true},
		{name: "double quoted", line: `GOMBIT_APP_NAME="my app"`, wantKey: "GOMBIT_APP_NAME", wantValue: "my app", wantOK: true},
		{name: "single quoted", line: `GOMBIT_APP_NAME='my app'`, wantKey: "GOMBIT_APP_NAME", wantValue: "my app", wantOK: true},
		{name: "surrounding whitespace", line: "  GOMBIT_HTTP_ADDR = :8080  ", wantKey: "GOMBIT_HTTP_ADDR", wantValue: ":8080", wantOK: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			key, value, ok := parseDotEnvLine(tc.line)
			if ok != tc.wantOK {
				t.Fatalf("parseDotEnvLine(%q) ok = %v, want %v", tc.line, ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if key != tc.wantKey || value != tc.wantValue {
				t.Fatalf("parseDotEnvLine(%q) = (%q, %q), want (%q, %q)", tc.line, key, value, tc.wantKey, tc.wantValue)
			}
		})
	}
}

func TestLoadDotEnvAppliesUnsetVariables(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	content := "GOMBIT_APP_NAME=fromdotenv\n# comment\n\nGOMBIT_HTTP_ADDR=:9999\n"
	if err := os.WriteFile(filepath.Join(dir, dotEnvFile), []byte(content), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	_ = os.Unsetenv(envAppName)
	_ = os.Unsetenv(envHTTPAddr)
	t.Cleanup(func() {
		_ = os.Unsetenv(envAppName)
		_ = os.Unsetenv(envHTTPAddr)
	})

	loadDotEnv()

	if got := os.Getenv(envAppName); got != "fromdotenv" {
		t.Fatalf("GOMBIT_APP_NAME = %q, want %q", got, "fromdotenv")
	}
	if got := os.Getenv(envHTTPAddr); got != ":9999" {
		t.Fatalf("GOMBIT_HTTP_ADDR = %q, want %q", got, ":9999")
	}
}

func TestLoadDotEnvDoesNotOverwriteProcessEnvironment(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	content := "GOMBIT_APP_NAME=fromdotenv\n"
	if err := os.WriteFile(filepath.Join(dir, dotEnvFile), []byte(content), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	t.Setenv(envAppName, "fromprocess")

	loadDotEnv()

	if got := os.Getenv(envAppName); got != "fromprocess" {
		t.Fatalf("GOMBIT_APP_NAME = %q, want %q (process env must win)", got, "fromprocess")
	}
}

func TestLoadDotEnvNoFileIsNoop(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	// Should not panic or error when .env does not exist.
	loadDotEnv()
}

func TestLoadAppliesDotEnvJWTSecret(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	content := "GOMBIT_JWT_SECRET=from-dotenv-secret-32-bytes-min!!\n"
	if err := os.WriteFile(filepath.Join(dir, dotEnvFile), []byte(content), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	_ = os.Unsetenv(envJWTSecret)
	t.Cleanup(func() { _ = os.Unsetenv(envJWTSecret) })

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if cfg.Auth.JWTSecret != "from-dotenv-secret-32-bytes-min!!" {
		t.Fatalf("Auth.JWTSecret = %q, want value from .env", cfg.Auth.JWTSecret)
	}
}

// TestDotEnvKeyIsTheLoadersGrammar: DotEnvKey names exactly the key the
// loader assigns from a line, or none when the loader assigns nothing.
func TestDotEnvKeyIsTheLoadersGrammar(t *testing.T) {
	for _, line := range []string{
		"GOMBIT_X=1", "  GOMBIT_X = 1 ", "export GOMBIT_X=1", "# GOMBIT_X=1", "#GOMBIT_X=1", "", "   ",
		"GOMBIT_X", "=1", " = 1", "GOMBIT_X=", "GOMBIT_X='a=b'", "lower=1", "\tGOMBIT_X=1\r",
	} {
		key, value, ok := parseDotEnvLine(line)
		got, gotOK := DotEnvKey([]byte(line))
		if gotOK != ok || got != key {
			t.Errorf("%q: DotEnvKey = %q, %v; the loader assigns %q (%q), %v", line, got, gotOK, key, value, ok)
		}
	}
}

// TestDotEnvGrammar: what the grammar itself assigns, pinned.
func TestDotEnvGrammar(t *testing.T) {
	for line, want := range map[string]string{
		"GOMBIT_X=1":        "GOMBIT_X",
		"  GOMBIT_X = 1 ":   "GOMBIT_X",
		"export GOMBIT_X=1": "export GOMBIT_X",
		"# GOMBIT_X=1":      "",
		"#GOMBIT_X=1":       "",
		"":                  "",
		"GOMBIT_X":          "",
		"=1":                "",
	} {
		got, ok := DotEnvKey([]byte(line))
		if got != want || ok != (want != "") {
			t.Errorf("%q: DotEnvKey = %q, %v; want %q", line, got, ok, want)
		}
	}
}
