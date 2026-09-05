package appcontract

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFrameworkVersionFromModfile(t *testing.T) {
	tests := map[string]struct {
		content string
		want    string
		wantErr error // nil = any / none; sentinel = errors.Is match
		ok      bool
	}{
		"block require": {
			content: "module x\n\ngo 1.25\n\nrequire (\n\tgithub.com/gombit-dev/gombit v0.5.0\n\tgithub.com/gin-gonic/gin v1.10.0 // indirect\n)\n",
			want:    "v0.5.0",
			ok:      true,
		},
		"single-line require": {
			content: "module x\n\nrequire github.com/gombit-dev/gombit v0.4.2\n",
			want:    "v0.4.2",
			ok:      true,
		},
		"pseudo-version": {
			content: "module x\nrequire github.com/gombit-dev/gombit v0.0.0-20260818193315-9abb3c6ecc8c\n",
			want:    "v0.0.0-20260818193315-9abb3c6ecc8c",
			ok:      true,
		},
		"local replace is unresolved": {
			content: "module x\n\nrequire github.com/gombit-dev/gombit v0.5.0\n\nreplace github.com/gombit-dev/gombit => ../gombit\n",
			wantErr: ErrFrameworkVersionUnresolved,
		},
		"block replace is unresolved": {
			content: "module x\nrequire github.com/gombit-dev/gombit v0.5.0\nreplace (\n\tgithub.com/gombit-dev/gombit => ../gombit\n)\n",
			wantErr: ErrFrameworkVersionUnresolved,
		},
		"missing framework require": {
			content: "module x\n\nrequire github.com/gin-gonic/gin v1.10.0\n",
			wantErr: errors.New("not a gombit app"), // any error
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := frameworkVersionFromModfile(tc.content)
			if tc.ok {
				if err != nil {
					t.Fatalf("error = %v, want nil", err)
				}
				if got != tc.want {
					t.Fatalf("version = %q, want %q", got, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("error = nil, want error")
			}
			if errors.Is(tc.wantErr, ErrFrameworkVersionUnresolved) && !errors.Is(err, ErrFrameworkVersionUnresolved) {
				t.Fatalf("error = %v, want ErrFrameworkVersionUnresolved", err)
			}
		})
	}
}

func TestFrameworkVersionReadsGoMod(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module x\nrequire github.com/gombit-dev/gombit v0.5.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := FrameworkVersion(dir)
	if err != nil {
		t.Fatalf("FrameworkVersion() error = %v", err)
	}
	if got != "v0.5.1" {
		t.Fatalf("version = %q, want v0.5.1", got)
	}
}

func TestFrameworkVersionMissingGoMod(t *testing.T) {
	if _, err := FrameworkVersion(t.TempDir()); err == nil {
		t.Fatal("FrameworkVersion() error = nil, want error for missing go.mod")
	}
}
