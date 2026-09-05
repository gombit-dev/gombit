package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// dotEnvFile is the file Load looks for in the current working directory.
// gombit new writes this with a per-project random GOMBIT_JWT_SECRET; it is
// gitignored and never read by LoadFromEnv directly, only by Load.
const dotEnvFile = ".env"

// loadDotEnv reads KEY=VALUE pairs from .env in the current working
// directory and applies them to the process environment, without
// overwriting a variable that is already set. It is a silent no-op when the
// file does not exist, so it is safe to call unconditionally: a real
// deployment sets variables through its own environment and never ships a
// .env file (it is gitignored by every gombit new scaffold).
func loadDotEnv() {
	for key, value := range dotEnvValues(dotEnvFile) {
		if _, set := os.LookupEnv(key); set {
			continue
		}
		_ = os.Setenv(key, value)
	}
}

// LoadFromDir reads configuration for the project rooted at dir. It applies
// dir/.env with the process environment taking precedence (as Load does for
// cwd), then reads and validates. Unlike Load it never mutates the process
// environment or the working directory, so it is safe under concurrency and
// for tooling that inspects a directory other than cwd — e.g. gombit contract
// app --dir.
func LoadFromDir(dir string) (Config, error) {
	fileEnv := dotEnvValues(filepath.Join(dir, dotEnvFile))
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
// occurrence winning. A missing file yields nil. It never touches the process
// environment.
func dotEnvValues(path string) map[string]string {
	f, err := os.Open(path) // #nosec G304 -- .env at a caller-provided project dir
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	values := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		key, value, ok := parseDotEnvLine(scanner.Text())
		if !ok {
			continue
		}
		if _, exists := values[key]; !exists {
			values[key] = value
		}
	}
	return values
}

// parseDotEnvLine parses a single KEY=VALUE line, skipping blanks and `#`
// comments. It strips one layer of matching single or double quotes from
// the value, the same convention every gombit-generated .env file follows.
func parseDotEnvLine(line string) (key, value string, ok bool) {
	line = strings.TrimSpace(line)
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
	v = strings.TrimSpace(v)
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
	}
	return k, v, true
}
