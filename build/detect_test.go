package build

import (
	"errors"
	"testing"
)

func TestDetectPackageManagerPrefersNpmWithoutLockfile(t *testing.T) {
	t.Parallel()
	name, path, err := detectPackageManager(t.TempDir(), func(file string) (string, error) {
		switch file {
		case "pnpm":
			return "/usr/bin/pnpm", nil
		case "npm":
			return "/usr/bin/npm", nil
		default:
			return "", errors.New("missing")
		}
	})
	if err != nil {
		t.Fatalf("detectPackageManager() error = %v", err)
	}
	if name != "npm" {
		t.Fatalf("manager = %q, want npm", name)
	}
	if path != "/usr/bin/npm" {
		t.Fatalf("path = %q, want /usr/bin/npm", path)
	}
}
