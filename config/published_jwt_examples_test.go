package config

import (
	"io/fs"
	"os"
	"regexp"
	"strings"
	"testing"
)

// Catch new copy-pasteable signing keys as they are added to public examples.
// Source fixtures are scanned, not copied into a hardcoded test-only blacklist.
func TestProductionRejectsAllPublishedExampleJWTSecrets(t *testing.T) {
	assignEnv := regexp.MustCompile("(?m)\\bGOMBIT_JWT_SECRET\\s*=\\s*['\\\"]?([A-Za-z0-9_-]{4,})")
	assignGo := regexp.MustCompile("(?m)\\bAuth\\.JWTSecret\\s*=\\s*\\\"([^\\\"]+)\\\"")
	roots := []string{"../docs", "../examples", "../scaffold/templates", "../goldentest/testdata/golden"}
	occurrences := 0
	for _, root := range roots {
		if _, err := os.Stat(root); err != nil {
			t.Fatalf("published-example root %q unavailable: %v", root, err)
		}
		rootFS := os.DirFS(root)
		err := fs.WalkDir(rootFS, ".", func(path string, d fs.DirEntry, readErr error) error {
			if readErr != nil {
				return readErr
			}
			if d.IsDir() {
				switch d.Name() {
				case "node_modules", ".git", "dist", ".next":
					return fs.SkipDir
				}
				return nil
			}
			name := d.Name()
			if !strings.HasSuffix(name, ".md") && !strings.HasSuffix(name, ".tmpl") &&
				!strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, ".env") &&
				!strings.HasSuffix(name, ".env.example") {
				return nil
			}
			raw, err := fs.ReadFile(rootFS, path)
			if err != nil {
				return err
			}
			text := string(raw)
			for _, re := range []*regexp.Regexp{assignEnv, assignGo} {
				for _, match := range re.FindAllStringSubmatch(text, -1) {
					secret := strings.TrimSpace(match[1])
					occurrences++
					cfg := DefaultFor(EnvironmentProduction)
					cfg.Auth.JWTSecret = secret
					if err := cfg.Validate(); err == nil {
						t.Errorf("%s publishes a JWT secret accepted in production", path)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scan %s: %v", root, err)
		}
	}
	if occurrences == 0 {
		t.Fatal("no public JWT assignments found; recheck the repository scanning paths and patterns")
	}
}
