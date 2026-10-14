package config

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseDotEnvLine(t *testing.T) {
	// Written as a code point rather than an escape so the byte order mark stays
	// visible to whoever reads this test later.
	bom := string(rune(0xFEFF))
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
		{name: "inline comment", line: "GOMBIT_APP_NAME=my app # trailing note", wantKey: "GOMBIT_APP_NAME", wantValue: "my app", wantOK: true},
		{name: "hash without a space is part of the value", line: "GOMBIT_APP_NAME=abc#def", wantKey: "GOMBIT_APP_NAME", wantValue: "abc#def", wantOK: true},
		{name: "hash inside double quotes", line: `GOMBIT_APP_NAME="a # b"`, wantKey: "GOMBIT_APP_NAME", wantValue: "a # b", wantOK: true},
		{name: "hash inside single quotes", line: `GOMBIT_APP_NAME='a # b'`, wantKey: "GOMBIT_APP_NAME", wantValue: "a # b", wantOK: true},
		{name: "export prefix", line: "export GOMBIT_HTTP_ADDR=:8080", wantKey: "GOMBIT_HTTP_ADDR", wantValue: ":8080", wantOK: true},
		{name: "export prefix with comment", line: "export GOMBIT_DOCS_ENABLED=true # on", wantKey: "GOMBIT_DOCS_ENABLED", wantValue: "true", wantOK: true},
		{name: "byte order mark", line: bom + "GOMBIT_APP_NAME=afterbom", wantKey: "GOMBIT_APP_NAME", wantValue: "afterbom", wantOK: true},
		{name: "only a comment after the value", line: "GOMBIT_DOCS_ENABLED= # empty plus comment", wantKey: "GOMBIT_DOCS_ENABLED", wantValue: "", wantOK: true},
		{name: "apostrophe inside the value opens no quote", line: "GOMBIT_APP_NAME=O'Malley app # prod", wantKey: "GOMBIT_APP_NAME", wantValue: "O'Malley app", wantOK: true},
		{name: "comment after a quoted value that holds a hash", line: `GOMBIT_APP_NAME="a # b" # prod`, wantKey: "GOMBIT_APP_NAME", wantValue: "a # b", wantOK: true},
		{name: "quote that is not the first character", line: `GOMBIT_APP_NAME=say "hi" # loud`, wantKey: "GOMBIT_APP_NAME", wantValue: `say "hi"`, wantOK: true},
		{name: "unterminated leading quote is plain text", line: `GOMBIT_APP_NAME='unclosed # tail`, wantKey: "GOMBIT_APP_NAME", wantValue: "'unclosed", wantOK: true},
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

	if err := loadDotEnv(); err != nil {
		t.Fatalf("loadDotEnv() = %v, want nil", err)
	}

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

	if err := loadDotEnv(); err != nil {
		t.Fatalf("loadDotEnv() = %v, want nil", err)
	}

	if got := os.Getenv(envAppName); got != "fromprocess" {
		t.Fatalf("GOMBIT_APP_NAME = %q, want %q (process env must win)", got, "fromprocess")
	}
}

func TestLoadDotEnvNoFileIsNoop(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	// Should not panic or error when .env does not exist.
	if err := loadDotEnv(); err != nil {
		t.Fatalf("loadDotEnv() with no .env = %v, want nil", err)
	}
}

func TestDotEnvValuesMissingFileIsNotAnError(t *testing.T) {
	values, err := dotEnvValues(filepath.Join(t.TempDir(), dotEnvFile))
	if err != nil {
		t.Fatalf("dotEnvValues() = %v, want nil for a missing file", err)
	}
	if values != nil {
		t.Fatalf("dotEnvValues() = %v, want nil map", values)
	}
}

func TestDotEnvValuesReadsKeysAfterALongLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), dotEnvFile)
	// Over bufio.Scanner's 64 KiB token default: the old scanner stopped here and
	// every key after it — including GOMBIT_JWT_SECRET — silently vanished.
	long := strings.Repeat("x", 70*1024)
	content := "GOMBIT_APP_NAME=first\nGOMBIT_LONG_VALUE=" + long + "\nGOMBIT_HTTP_ADDR=:8080\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	values, err := dotEnvValues(path)
	if err != nil {
		t.Fatalf("dotEnvValues() = %v, want nil", err)
	}
	if values["GOMBIT_LONG_VALUE"] != long {
		t.Fatalf("GOMBIT_LONG_VALUE length = %d, want %d", len(values["GOMBIT_LONG_VALUE"]), len(long))
	}
	if got := values["GOMBIT_HTTP_ADDR"]; got != ":8080" {
		t.Fatalf("GOMBIT_HTTP_ADDR = %q, want :8080 (keys after a long line must still load)", got)
	}
}

func TestDotEnvValuesReportsALineOverTheCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), dotEnvFile)
	content := "GOMBIT_APP_NAME=ok\nGOMBIT_LONG_VALUE=" + strings.Repeat("y", maxDotEnvLineBytes+1) + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	_, err := dotEnvValues(path)
	if err == nil {
		t.Fatal("dotEnvValues() = nil, want an error instead of silently dropping the rest")
	}
	if !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("dotEnvValues() error = %v, want it to wrap bufio.ErrTooLong", err)
	}
}

func TestLoadFailsOnATruncatedDotEnv(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	content := "GOMBIT_APP_NAME=ok\nGOMBIT_LONG_VALUE=" + strings.Repeat("z", maxDotEnvLineBytes+1) + "\n"
	if err := os.WriteFile(filepath.Join(dir, dotEnvFile), []byte(content), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	if _, err := Load(); err == nil {
		t.Fatal("Load() with a .env it cannot read = nil error, want failure")
	} else if !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("Load() error = %v, want it to come from the truncated .env", err)
	}
}

func TestLoadFromDirReportsATruncatedDotEnv(t *testing.T) {
	dir := t.TempDir()
	content := "GOMBIT_LONG_VALUE=" + strings.Repeat("w", maxDotEnvLineBytes+1) + "\n"
	if err := os.WriteFile(filepath.Join(dir, dotEnvFile), []byte(content), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	if _, err := LoadFromDir(dir); err == nil {
		t.Fatal("LoadFromDir() with a truncated .env = nil error, want failure")
	} else if !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("LoadFromDir() error = %v, want it to come from the truncated .env", err)
	}
}

// A .env that exists but cannot be read — here a directory in its place — is an
// error, unlike the missing file that stays a silent no-op. This is the
// non-ErrNotExist os.Open/scan path the issue lists.
func TestDotEnvValuesReportsAnUnreadableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), dotEnvFile)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("mkdir .env: %v", err)
	}

	values, err := dotEnvValues(path)
	if err == nil {
		t.Fatal("dotEnvValues() = nil error, want failure for a .env that cannot be read")
	}
	if values != nil {
		t.Fatalf("dotEnvValues() = %v, want no values alongside the error", values)
	}
}

// Repeated keys take the first value, which is what dotEnvValues has always
// done. Note this diverges from `set -a; . ./.env`, where the last assignment
// wins; a .env should not repeat a key.
func TestDotEnvValuesFirstOccurrenceWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), dotEnvFile)
	content := "GOMBIT_APP_NAME=first\nGOMBIT_APP_NAME=second\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	values, err := dotEnvValues(path)
	if err != nil {
		t.Fatalf("dotEnvValues() = %v, want nil", err)
	}
	if got := values["GOMBIT_APP_NAME"]; got != "first" {
		t.Fatalf("GOMBIT_APP_NAME = %q, want first (the first occurrence wins)", got)
	}
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
