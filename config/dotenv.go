package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// dotEnvFile is the file Load looks for in the current working directory.
// gombit new writes this with a per-project random GOMBIT_JWT_SECRET; it is
// gitignored and never read by LoadFromEnv directly, only by Load.
const dotEnvFile = ".env"

// utf8BOM is what an editor on Windows can leave at the very start of a
// hand-written .env. See scanDotEnv.
var utf8BOM = []byte("\ufeff")

// loadDotEnv reads KEY=VALUE pairs from .env in the current working
// directory and applies them to the process environment, without
// overwriting a variable that is already set. A file that does not exist is a
// silent no-op, so it is safe to call unconditionally: a real
// deployment sets variables through its own environment and never ships a
// .env file (it is gitignored by every gombit new scaffold). A file that
// exists but cannot be read is an error: half-loaded configuration would
// mean a missing GOMBIT_JWT_SECRET, which must not boot silently.
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
// occurrence winning. A missing file yields a nil map and no error; a file
// that exists but cannot be read yields an error, because applying part of a
// configuration silently can mean an app that boots with no JWT secret. It
// reads the whole file, lines of any length (a line scanner would stop at a long
// one and drop every assignment after it), and never touches the process
// environment.
func dotEnvValues(path string) (map[string]string, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- .env at a caller-provided project dir
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	values := map[string]string{}
	scanDotEnv(data, func(key, value []byte) {
		if _, exists := values[string(key)]; !exists {
			values[string(key)] = string(unquoteDotEnv(value))
		}
	})
	return values, nil
}

// scanDotEnv calls fn for each assignment of a .env file's content, in
// order: every line (split at '\n', a trailing '\r' trimmed with the
// rest of the space), by splitDotEnvLine. The loader and DotEnvKeys read
// files through it, so they agree on the whole file, not only on a line.
// A UTF-8 byte order mark is dropped first: it can only ever be at the start
// of the file, and left there it makes the first key one that matches no
// variable.
func scanDotEnv(data []byte, fn func(key, value []byte)) {
	data = bytes.TrimPrefix(data, utf8BOM)
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
// '=', assigns its trimmed left side (non-empty) the rest, with a trailing
// comment removed. A key is taken as written: "export KEY" is not KEY.
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
	return k, bytes.TrimSpace(stripDotEnvComment(v)), true
}

// stripDotEnvComment removes a trailing `# ...` comment from the raw bytes
// after `=`. A `#` starts one only outside a quoted region and where whitespace
// precedes it, so `PASS=abc#def` and `COLOR=#fff` keep their hash — a `#` at the
// very start has no whitespace before it to give away a comment — while
// `PASS=abc # note` does not store the note inside the secret. That is what
// `set -a; . ./.env` answers for the same lines. A quoted region is what this
// grammar takes a value to be: a quote at the first byte after any leading
// whitespace, closed by the next matching quote (see unquoteDotEnv). An
// apostrophe anywhere else opens nothing, so `O'Malley app # prod` loses its
// comment. Escaped quotes inside a quoted value are not supported.
func stripDotEnvComment(v []byte) []byte {
	lead := len(v) - len(bytes.TrimLeft(v, " \t"))
	from := 0
	if lead < len(v) && (v[lead] == '"' || v[lead] == '\'') {
		if end := bytes.IndexByte(v[lead+1:], v[lead]); end >= 0 {
			from = lead + end + 2
		}
	}
	for i := from; i < len(v); i++ {
		if v[i] != '#' {
			continue
		}
		if i > 0 && (v[i-1] == ' ' || v[i-1] == '\t') {
			return bytes.TrimRight(v[:i], " \t")
		}
	}
	return v
}

// parseDotEnvLine parses a single KEY=VALUE line, skipping blanks and `#`
// comment lines. It drops a trailing inline comment and strips one layer of
// matching single or double quotes from the value, the same convention every
// gombit-generated .env file follows.
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
