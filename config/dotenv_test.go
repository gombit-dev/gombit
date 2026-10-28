package config

import (
	"os"
	"path/filepath"
	"strings"
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
		// A `#` that has whitespace before it starts a comment, and one that does
		// not is part of the value. These are the answers `set -a; . ./.env` gives
		// for the same lines, which is what issue #507 asks the loader to match.
		{name: "inline comment", line: "GOMBIT_APP_NAME=my app # trailing note", wantKey: "GOMBIT_APP_NAME", wantValue: "my app", wantOK: true},
		{name: "quoted value with an inline comment", line: `GOMBIT_LOG_LEVEL="debug" # local`, wantKey: "GOMBIT_LOG_LEVEL", wantValue: "debug", wantOK: true},
		{name: "hash with no space before it", line: "GOMBIT_APP_NAME=abc#def", wantKey: "GOMBIT_APP_NAME", wantValue: "abc#def", wantOK: true},
		{name: "value that starts with a hash", line: "GOMBIT_APP_NAME=#fff", wantKey: "GOMBIT_APP_NAME", wantValue: "#fff", wantOK: true},
		{name: "JWT secret that starts with a hash", line: "GOMBIT_JWT_SECRET=#s3cr3t", wantKey: "GOMBIT_JWT_SECRET", wantValue: "#s3cr3t", wantOK: true},
		{name: "a lone hash is a value", line: "GOMBIT_APP_NAME=#", wantKey: "GOMBIT_APP_NAME", wantValue: "#", wantOK: true},
		{name: "whitespace then a hash is a comment", line: "GOMBIT_APP_NAME= #fff", wantKey: "GOMBIT_APP_NAME", wantValue: "", wantOK: true},
		{name: "tab then a hash is a comment", line: "GOMBIT_APP_NAME=\t#fff", wantKey: "GOMBIT_APP_NAME", wantValue: "", wantOK: true},
		{name: "hash inside double quotes", line: `GOMBIT_APP_NAME="a # b"`, wantKey: "GOMBIT_APP_NAME", wantValue: "a # b", wantOK: true},
		{name: "hash inside single quotes", line: `GOMBIT_APP_NAME='a # b'`, wantKey: "GOMBIT_APP_NAME", wantValue: "a # b", wantOK: true},
		{name: "hash as the quoted value", line: `GOMBIT_APP_NAME="#fff" # note`, wantKey: "GOMBIT_APP_NAME", wantValue: "#fff", wantOK: true},
		{name: "only a comment after the value", line: "GOMBIT_DOCS_ENABLED= # empty plus comment", wantKey: "GOMBIT_DOCS_ENABLED", wantValue: "", wantOK: true},
		{name: "apostrophe inside the value opens no quote", line: "GOMBIT_APP_NAME=O'Malley app # prod", wantKey: "GOMBIT_APP_NAME", wantValue: "O'Malley app", wantOK: true},
		{name: "comment after a quoted value that holds a hash", line: `GOMBIT_APP_NAME="a # b" # prod`, wantKey: "GOMBIT_APP_NAME", wantValue: "a # b", wantOK: true},
		{name: "quote that is not the first character", line: `GOMBIT_APP_NAME=say "hi" # loud`, wantKey: "GOMBIT_APP_NAME", wantValue: `say "hi"`, wantOK: true},
		{name: "unterminated leading quote is plain text", line: `GOMBIT_APP_NAME='unclosed # tail`, wantKey: "GOMBIT_APP_NAME", wantValue: "'unclosed", wantOK: true},
		{name: "quoted value after leading whitespace", line: `GOMBIT_APP_NAME=  "a # b"`, wantKey: "GOMBIT_APP_NAME", wantValue: "a # b", wantOK: true},
		// Escapes are not part of this parser. The value below is read as a quote
		// closed by the escaped one, so the rest is a comment: pinned so the
		// limitation is visible here rather than only in docs/config.md.
		{name: "escaped quote is not supported", line: `GOMBIT_APP_NAME="a\" # b"`, wantKey: "GOMBIT_APP_NAME", wantValue: `a\`, wantOK: true},
		// Upstream's grammar (issue #585) takes a key as written, so `export`
		// stays part of it; #507 would like sh's answer, which is a separate
		// call to make. Pinned here so the divergence is a known edge, not a
		// surprise.
		{name: "export is part of the key", line: "export GOMBIT_HTTP_ADDR=:8080", wantKey: "export GOMBIT_HTTP_ADDR", wantValue: ":8080", wantOK: true},
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

// A UTF-8 BOM is a property of the file, not of a line: only the first line can
// carry one, and there it hides the first key, so the key `"\ufeffGOMBIT_APP_NAME"`
// matches no variable. The rule this test pins matches the dotenv loaders, not
// the shell: `set -a; . ./.env` fails on a marked file (`command not found` for
// the first line in bash and dash), so a marked file is something only the
// reader can rescue.
func TestDotEnvValuesStripsOnlyALeadingByteOrderMark(t *testing.T) {
	// Written as a code point rather than an escape so the byte order mark stays
	// visible to whoever reads this test later.
	bom := string(rune(0xFEFF))
	path := filepath.Join(t.TempDir(), dotEnvFile)
	content := bom + "GOMBIT_APP_NAME=afterbom\nGOMBIT_HTTP_ADDR=:8080\n" + bom + "GOMBIT_DOCS_ENABLED=true\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	values, err := dotEnvValues(path)
	if err != nil {
		t.Fatalf("dotEnvValues() = %v, want nil", err)
	}
	if got := values["GOMBIT_APP_NAME"]; got != "afterbom" {
		t.Fatalf("GOMBIT_APP_NAME = %q, want afterbom (the leading BOM must not become part of the key)", got)
	}
	if got := values["GOMBIT_DOCS_ENABLED"]; got != "" {
		t.Fatalf("GOMBIT_DOCS_ENABLED = %q, want unset (a BOM mid-file is not stripped)", got)
	}
	if _, marked := values[bom+"GOMBIT_DOCS_ENABLED"]; !marked {
		t.Fatalf("values = %v, want the mid-file BOM kept in its key (only line 1 is stripped)", values)
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
		t.Fatalf("loadDotEnv() = %v, want nil", err)
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

// A missing .env is the silent no-op a deployment relies on; a file that
// exists but cannot be read is not. Here a directory stands in for it, which
// every platform reports as a read error rather than ErrNotExist.
func TestDotEnvValuesMissingFileIsNotAnError(t *testing.T) {
	values, err := dotEnvValues(filepath.Join(t.TempDir(), dotEnvFile))
	if err != nil {
		t.Fatalf("dotEnvValues() = %v, want nil for a missing file", err)
	}
	if values != nil {
		t.Fatalf("dotEnvValues() = %v, want nil map", values)
	}
}

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

// Both entry points that read a file must surface it: swallowing it here is
// how an app boots on framework defaults with no GOMBIT_JWT_SECRET.
func TestLoadAndLoadFromDirReportAnUnreadableDotEnv(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, dotEnvFile), 0o700); err != nil {
		t.Fatalf("mkdir .env: %v", err)
	}

	if _, err := LoadFromDir(dir); err == nil {
		t.Fatal("LoadFromDir() with an unreadable .env = nil error, want failure")
	}

	t.Chdir(dir)
	if _, err := Load(); err == nil {
		t.Fatal("Load() with an unreadable .env = nil error, want failure")
	}
}

// The security-relevant half of the `#` rule, end to end: a secret that starts
// with a hash must reach AuthConfig intact, because an empty one does not fail
// Load — AuthConfig.Enabled() reads it as "auth off".
func TestLoadKeepsADotEnvSecretStartingWithAHash(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	content := "GOMBIT_JWT_SECRET=#s3cr3t-that-is-32-bytes-minimum-yes!!\n"
	if err := os.WriteFile(filepath.Join(dir, dotEnvFile), []byte(content), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	_ = os.Unsetenv(envJWTSecret)
	t.Cleanup(func() { _ = os.Unsetenv(envJWTSecret) })

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if cfg.Auth.JWTSecret != "#s3cr3t-that-is-32-bytes-minimum-yes!!" {
		t.Fatalf("Auth.JWTSecret = %q, want the leading hash kept", cfg.Auth.JWTSecret)
	}
	if !cfg.Auth.Enabled() {
		t.Fatal("Auth.Enabled() = false, want true (an empty secret fails open to auth off)")
	}
}

// TestDotEnvLongLine: a line longer than a line scanner's buffer (64 KiB)
// neither stops the loader nor hides the assignments after it, and
// DotEnvKeys, the tooling's view, agrees.
func TestDotEnvLongLine(t *testing.T) {
	dir := t.TempDir()
	content := "GOMBIT_LONG=" + strings.Repeat("x", 100<<10) + "\nGOMBIT_AFTER=yes\r\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	values, err := dotEnvValues(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatalf("dotEnvValues() = %v, want nil", err)
	}
	if values["GOMBIT_AFTER"] != "yes" || len(values["GOMBIT_LONG"]) != 100<<10 {
		t.Fatalf("loaded %d keys; GOMBIT_AFTER = %q", len(values), values["GOMBIT_AFTER"])
	}
	if got := strings.Join(DotEnvKeys([]byte(content)), ","); got != "GOMBIT_LONG,GOMBIT_AFTER" {
		t.Fatalf("DotEnvKeys = %s", got)
	}
}
