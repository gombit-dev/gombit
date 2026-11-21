package frontend

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDetectPackageManager(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		lockfile  bool
		available map[string]string
		want      string
		wantErr   bool
	}{
		{name: "npm for fresh scaffold", available: map[string]string{"npm": "/usr/bin/npm", "pnpm": "/usr/bin/pnpm"}, want: "npm"},
		{name: "pnpm project uses lockfile", lockfile: true, available: map[string]string{"npm": "/usr/bin/npm", "pnpm": "/usr/bin/pnpm"}, want: "pnpm"},
		{name: "pnpm fallback when npm unavailable", available: map[string]string{"pnpm": "/usr/bin/pnpm"}, want: "pnpm"},
		{name: "npm when pnpm unavailable", available: map[string]string{"npm": "/usr/bin/npm"}, want: "npm"},
		{name: "missing package managers", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			frontendDir := t.TempDir()
			if tt.lockfile {
				if err := os.WriteFile(filepath.Join(frontendDir, "pnpm-lock.yaml"), []byte("lockfileVersion: '9.0'\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			name, _, err := DetectPackageManager(frontendDir, func(name string) (string, error) {
				if path, ok := tt.available[name]; ok {
					return path, nil
				}
				return "", errors.New("missing")
			})
			if tt.wantErr {
				if !errors.Is(err, ErrNoPackageManager) {
					t.Fatalf("DetectPackageManager() error = %v, want %v", err, ErrNoPackageManager)
				}
				return
			}
			if err != nil {
				t.Fatalf("DetectPackageManager() error = %v", err)
			}
			if name != tt.want {
				t.Fatalf("manager = %q, want %q", name, tt.want)
			}
		})
	}
}
