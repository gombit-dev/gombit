package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// dotEnvFile is the file Load looks for in the current working directory.
// gombit new writes this with a per-project random GOMBIT_JWT_SECRET; it is
// gitignored and never read by LoadFromEnv directly, only by Load.
const dotEnvFile = ".env"

// maxDotEnvLineBytes is the longest single .env line the scanner accepts. The
// bufio.Scanner default of 64 KiB is smaller than a realistic base64 secret
// bundle, and a line over the cap stops the whole scan, so the cap is both
// raised and turned into an error instead of a silent truncation.
const maxDotEnvLineBytes = 1 << 20

// loadDotEnv reads KEY=VALUE pairs from .env in the current working
// directory and applies them to the process environment, without
// overwriting a variable that is already set. A file that does not exist is a
// silent no-op, so it is safe to call unconditionally: a real
// deployment sets variables through its own environment and never ships a
// .env file (it is gitignored by every gombit new scaffold). A file that
// exists but cannot be read in full is an error: half-loaded configuration
// would mean a missing GOMBIT_JWT_SECRET, which must not boot silently.
func loadDotEnv() error {
	values, err := dotEnvValues(dotEnvFile)
	if err != nil {
		return err
	}
	for key, value := range values {
		if _, set := os.LookupEnv(key); set {
			continue
		}
		_ = os.Setenv(key, value)
	}
	return nil
}

// LoadFromDir reads configuration for the project rooted at dir. It applies
// dir/.env with the process environment taking precedence (as Load does for
// cwd), then reads and validates. Unlike Load it never mutates the process
// environment or the working directory, so it is safe under concurrency and
// for tooling that inspects a directory other than cwd — e.g. gombit contract
// app --dir.
func LoadFromDir(dir string) (Config, error) {
	fileEnv, err := dotEnvValues(filepath.Join(dir, dotEnvFile))
	if err != nil {
		return Config{}, err
	}
	lookup := func(key string) (string, bool) {
		if v, ok := os.LookupEnv(key); ok {
			return v, true
		}
		v, ok := fileEnv[key]
		return v, ok
	}
	return LoadFromEnv(lookup)
}

// dotEnvValues parses KEY=VALUE pairs from the .env file at path, first
// occurrence winning. A missing file yields a nil map and no error; a file that
// exists but cannot be read or is too long to scan yields an error. It never
// touches the process environment.
func dotEnvValues(path string) (map[string]string, error) {
	f, err := os.Open(path) // #nosec G304 -- .env at a caller-provided project dir
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	values := map[string]string{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), maxDotEnvLineBytes)
	for scanner.Scan() {
		key, value, ok := parseDotEnvLine(scanner.Text())
		if !ok {
			continue
		}
		if _, exists := values[key]; !exists {
			values[key] = value
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	return values, nil
}

// parseDotEnvLine parses a single KEY=VALUE line, skipping blanks and `#`
// comments. It accepts the shell conventions a hand-written .env normally
// uses: a leading UTF-8 BOM and an `export ` prefix are dropped, a `#` that
// starts an inline comment (outside quotes, preceded by whitespace) is trimmed,
// and one layer of matching single or double quotes is stripped from the
// value, the same convention every gombit-generated .env file follows.
func parseDotEnvLine(line string) (key, value string, ok bool) {
	line = strings.TrimSpace(strings.TrimPrefix(strings.TrimLeft(line, " \t"), "\ufeff"))
	line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	k, v, found := strings.Cut(line, "=")
	if !found {
		return "", "", false
	}
	k = strings.TrimSpace(k)
	if k == "" {
		return "", "", false
	}
	v = strings.TrimSpace(stripInlineComment(strings.TrimSpace(v)))
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
	}
	return k, v, true
}

// stripInlineComment removes a trailing `# ...` comment. A `#` only starts one
// when it is outside quotes and preceded by whitespace, so a value like
// `PASS=abc#def` keeps its hash while `PASS=abc # note` does not swallow the
// rest of the line into the secret.
func stripInlineComment(v string) string {
	var quote rune
	for i, r := range v {
		if quote != 0 {
			if r == quote {
				quote = 0
			}
			continue
		}
		switch r {
		case '"', '\'':
			quote = r
		case '#':
			if i == 0 || v[i-1] == ' ' || v[i-1] == '\t' {
				return strings.TrimRight(v[:i], " \t")
			}
		}
	}
	return v
}
