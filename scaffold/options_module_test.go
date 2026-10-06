package scaffold

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateModuleUsesGoMainModulePathRules(t *testing.T) {
	invalid := []string{
		"example.com/demo@v2",
		"example.com/demo;x",
		"example.com/demo\tbad",
		"example.com/demo/v1",
		"example.com/con",
		"example.com/a//b",
		"go",
		"toolchain",
	}
	for _, modulePath := range invalid {
		t.Run("reject_"+modulePath, func(t *testing.T) {
			if err := validateModule(modulePath); err == nil {
				t.Fatalf("validateModule(%q) error = nil, want Go main-module-path rejection", modulePath)
			}
		})
	}

	valid := []string{
		"myapp",
		"my_app",
		"MyApp",
		"localhost/demo",
		"Example.com/demo",
		"example.com/a+b",
		"example.com/.hidden",
		"gopkg.in/demo.v1",
		"example.com/demo",
		"example.com/demo/v2",
	}
	for _, modulePath := range valid {
		t.Run("accept_"+modulePath, func(t *testing.T) {
			if err := validateModule(modulePath); err != nil {
				t.Fatalf("validateModule(%q) error = %v, want nil", modulePath, err)
			}
		})
	}
}

func TestValidateModuleErrorDoesNotDuplicatePath(t *testing.T) {
	modulePath := "example.com/demo@v2"
	err := validateModule(modulePath)
	if err == nil {
		t.Fatal("validateModule() error = nil")
	}
	if got := strings.Count(err.Error(), modulePath); got != 1 {
		t.Fatalf("validateModule() error contains module path %d times, want 1: %v", got, err)
	}
}

func TestGenerateRejectsMalformedModuleBeforeWritingDestination(t *testing.T) {
	workDir := t.TempDir()
	dest := filepath.Join(workDir, "demo")

	err := Generate(context.Background(), Options{
		Name:     "demo",
		Module:   "example.com/demo@v2",
		Database: "sqlite",
		WorkDir:  workDir,
	})
	if err == nil || !strings.Contains(err.Error(), "module path") {
		t.Fatalf("Generate() error = %v, want module path rejection", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("destination exists after validation failure: stat error = %v", statErr)
	}
}

func TestGenerateAcceptsSimpleMainModulePath(t *testing.T) {
	workDir := t.TempDir()

	err := Generate(context.Background(), Options{
		Name:     "demo",
		Module:   "myapp",
		Database: "sqlite",
		WorkDir:  workDir,
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	goMod, err := os.ReadFile(filepath.Join(workDir, "demo", "go.mod"))
	if err != nil {
		t.Fatalf("read generated go.mod: %v", err)
	}
	if !strings.HasPrefix(string(goMod), "module myapp\n") {
		t.Fatalf("generated go.mod = %q, want module myapp", goMod)
	}
}
