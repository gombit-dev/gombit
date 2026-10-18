package config

import (
	"bytes"
	"os"
	"path/filepath"
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
// occurrence winning. A missing (or unreadable) file yields nil. It reads
// the whole file, lines of any length (a line scanner would stop at a long
// one and drop every assignment after it), and never touches the process
// environment.
func dotEnvValues(path string) map[string]string {
	data, err := os.ReadFile(path) // #nosec G304 -- .env at a caller-provided project dir
	if err != nil {
		return nil
	}
	values := map[string]string{}
	scanDotEnv(data, func(key, value []byte) {
		if _, exists := values[string(key)]; !exists {
			values[string(key)] = string(unquoteDotEnv(value))
		}
	})
	return values
}

// scanDotEnv calls fn for each assignment of a .env file's content, in
// order: every line (split at '\n', a trailing '\r' trimmed with the
// rest of the space), by splitDotEnvLine. The loader and DotEnvKeys read
// files through it, so they agree on the whole file, not only on a line.
func scanDotEnv(data []byte, fn func(key, value []byte)) {
	for _, line := range bytes.Split(data, []byte("\n")) {
		if k, v, ok := splitDotEnvLine(line); ok {
			fn(k, v)
		}
	}
}

// DotEnvKeys are the variables a .env file's content assigns when Load
// reads it, in order (repeats included): the loader's own traversal and
// grammar, the keys copied out and the values never turned into strings.
func DotEnvKeys(data []byte) []string {
	var keys []string
	scanDotEnv(data, func(key, _ []byte) { keys = append(keys, string(key)) })
	return keys
}

// splitDotEnvLine is the .env grammar, shared by the loader and DotEnvKey:
// a line, trimmed, that is not blank, does not start with '#', and has an
// '=', assigns its trimmed left side (non-empty) the rest. A key is taken
// as written: "export KEY" is not KEY.
func splitDotEnvLine(line []byte) (key, value []byte, ok bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] == '#' {
		return nil, nil, false
	}
	k, v, found := bytes.Cut(line, []byte("="))
	if !found {
		return nil, nil, false
	}
	k = bytes.TrimSpace(k)
	if len(k) == 0 {
		return nil, nil, false
	}
	return k, bytes.TrimSpace(v), true
}

// parseDotEnvLine parses a single KEY=VALUE line, skipping blanks and `#`
// comments. It strips one layer of matching single or double quotes from
// the value, the same convention every gombit-generated .env file follows.
func parseDotEnvLine(line string) (key, value string, ok bool) {
	k, v, ok := splitDotEnvLine([]byte(line))
	if !ok {
		return "", "", false
	}
	return string(k), string(unquoteDotEnv(v)), true
}

// unquoteDotEnv strips one layer of matching single or double quotes.
func unquoteDotEnv(v []byte) []byte {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// DotEnvKey is the variable a line of a .env file sets when Load reads it,
// by the loader's own grammar, and whether it sets one: never for a blank
// line or a comment, and "export KEY=..." sets "export KEY", not KEY. It
// reads the line's bytes and copies out only the key, so tooling can learn
// what a file sets without its values becoming strings.
func DotEnvKey(line []byte) (string, bool) {
	k, _, ok := splitDotEnvLine(line)
	if !ok {
		return "", false
	}
	return string(k), true
}
