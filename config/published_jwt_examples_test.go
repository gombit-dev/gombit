package config

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// These assignment shapes cover environment files, shell, YAML, Go fields
// and raw-string literals. Nonliteral expressions cannot be statically
// evaluated; any apparent long public signing key still fails the guard.
const publishedSigningAssignmentPattern = "(?m)\\b(GOMBIT_[A-Z0-9_]*_SECRET|(?:[A-Za-z_][A-Za-z0-9_]*\\.)*(?:JWTSecret|URLSecret)|devURLSecret)\\s*[:=]\\s*(?:'([^'\\r\\n]*)'|\"([^\"\\r\\n]*)\"|`([^`\\r\\n]*)`|([^'\"`\\s\\\\,;}#]+))"
var publishedSigningAssignment = regexp.MustCompile(publishedSigningAssignmentPattern)

type publishedSigningAssignmentMatch struct { name, secret string }

func publishedSigningAssignments(source string) []publishedSigningAssignmentMatch {
	var out []publishedSigningAssignmentMatch
	for _, m := range publishedSigningAssignment.FindAllStringSubmatch(source, -1) {
		secret := ""
		for _, part := range m[2:] { if part != "" { secret = part; break } }
		if secret != "" { out = append(out, publishedSigningAssignmentMatch{name: m[1], secret: secret}) }
	}
	return out
}

func TestPublicSigningAssignmentParserRecognizesCommonCopyPasteSyntax(t *testing.T) {
	cases := []struct{ line, name, want string }{
		{"GOMBIT_JWT_SECRET='examplexy!012345678901234567890123456789'", "GOMBIT_JWT_SECRET", "examplexy!012345678901234567890123456789"},
		{"GOMBIT_JWT_SECRET=\"public sample with embedded spaces 123456789!\"", "GOMBIT_JWT_SECRET", "public sample with embedded spaces 123456789!"},
		{"GOMBIT_JWT_SECRET=bare-value-012345", "GOMBIT_JWT_SECRET", "bare-value-012345"},
		{"GOMBIT_JWT_SECRET=\"a public key with apostrophe's and digits 0123456789\"", "GOMBIT_JWT_SECRET", "a public key with apostrophe's and digits 0123456789"},
		{"GOMBIT_JWT_SECRET: \"compose-example-signing-key-0123456789abcdef\"", "GOMBIT_JWT_SECRET", "compose-example-signing-key-0123456789abcdef"},
		{"cfg := config.Config{Auth: config.AuthConfig{JWTSecret: \"struct-literal-example-key-0123456789abcdef\"}}", "JWTSecret", "struct-literal-example-key-0123456789abcdef"},
		{"GOMBIT_JWT_SECRET=`echo backtick-example-signing-key-0123456789abcdef`", "GOMBIT_JWT_SECRET", "echo backtick-example-signing-key-0123456789abcdef"},
		{"JWTSecret: `raw-go-string-0123456789012345678901234`", "JWTSecret", "raw-go-string-0123456789012345678901234"},
		{"cfg.Auth.JWTSecret = \"published-go-assignment-value-012345678901\"", "cfg.Auth.JWTSecret", "published-go-assignment-value-012345678901"},
		{"GOMBIT_STORAGE_URL_SECRET: \"storage-url-signing-key-0123456789012345\"", "GOMBIT_STORAGE_URL_SECRET", "storage-url-signing-key-0123456789012345"},
		{"const devURLSecret = \"dev-only-storage-example-url-signing-secret\"", "devURLSecret", "dev-only-storage-example-url-signing-secret"},
	}
	for i, tc := range cases {
		found := publishedSigningAssignments(tc.line)
		if len(found) != 1 || found[0].name != tc.name || found[0].secret != tc.want {
			t.Errorf("case %d: found %#v, want %q / %q", i, found, tc.name, tc.want)
		}
	}
}

// This is a regression tripwire for *published* secrets, not a generic
// source-code secret detector. The known long JWT and storage values must
// both remain visible to the scan and rejected on their own config fields.
func TestProductionRejectsPublishedSigningExamples(t *testing.T) {
	foundJWT, foundStorage := false, false
	check := func(path string, data []byte) {
		if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) { return }
		for _, item := range publishedSigningAssignments(string(data)) {
			var field string
			switch {
			case item.name == "GOMBIT_JWT_SECRET" || strings.HasSuffix(item.name, "JWTSecret"):
				field = "Auth.JWTSecret"
			case item.name == "GOMBIT_STORAGE_URL_SECRET" || strings.HasSuffix(item.name, "URLSecret"):
				field = "Storage.URLSecret"
			case strings.HasPrefix(item.name, "GOMBIT_") && strings.HasSuffix(item.name, "_SECRET"):
				if len(item.secret) >= MinProductionJWTSecretLength { t.Errorf("%s: unknown published secret assignment needs classification", path) }
				continue
			default: continue
			}
			lengthFloor := MinProductionJWTSecretLength
			if field == "Storage.URLSecret" { lengthFloor = MinStorageURLSecretLength }
			if len(item.secret) < lengthFloor { continue }
			if item.secret == exampleJWTSecret { foundJWT = true }
			if item.secret == exampleStorageURLSecret { foundStorage = true }
			cfg := DefaultFor(EnvironmentProduction)
			cfg.Auth.JWTSecret = "unique-private-jwt-key-0123456789abcdef"
			if field == "Auth.JWTSecret" { cfg.Auth.JWTSecret = item.secret } else { cfg.Storage.URLSecret = item.secret }
			err := cfg.Validate()
			var fields FieldErrors
			if !errors.As(err, &fields) { t.Errorf("%s: %s accepted published signing key", path, field); continue }
			rejected := false
			for _, f := range fields {
				if f.Field == field { rejected = true; if f.Value != "" { t.Errorf("%s: %s leaked secret in FieldError.Value", path, field) } }
			}
			if !rejected { t.Errorf("%s: %s accepted published signing key", path, field) }
			if strings.Contains(err.Error(), item.secret) { t.Errorf("%s: %s leaked secret in error", path, field) }
		}
	}
	roots := []string{"../docs", "../examples", "../scaffold/templates", "../goldentest/testdata/golden"}
	for _, root := range roots {
		if _, err := os.Stat(root); err != nil { t.Fatalf("published source root missing %s: %v", root, err) }
		rootFS := os.DirFS(root)
		err := fs.WalkDir(rootFS, ".", func(path string, entry fs.DirEntry, readErr error) error {
			if readErr != nil { return readErr }
			if entry.IsDir() {
				switch entry.Name() { case ".git", "node_modules", "dist", ".next": return fs.SkipDir }
				return nil
			}
			if !entry.Type().IsRegular() { return nil }
			b, err := fs.ReadFile(rootFS, path)
			if err != nil { return err }
			check(root + "/" + path, b)
			return nil
		})
		if err != nil { t.Fatalf("scan %s: %v", root, err) }
	}
	for _, p := range []string{"README.md", "SECURITY.md", "CONTRIBUTING.md", "AGENTS.md"} {
		b, err := os.ReadFile("../" + p)
		if errors.Is(err, fs.ErrNotExist) { continue }
		if err != nil { t.Fatalf("read %s: %v", p, err) }
		check(p, b)
	}
	if !foundJWT { t.Error("long published JWT example key was not discovered by scanner") }
	if !foundStorage { t.Error("long published storage example key was not discovered by scanner") }
}
