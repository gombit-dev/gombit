package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/migrations"
)

const forgetTestDatabaseGo = `package platform

import (
	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/database"

	"github.com/example/app/internal/product"
	"github.com/example/app/internal/widget"
)

func AutoMigrate(db *database.DB) error {
	return db.AutoMigrate(
		&auth.User{},
		&product.Product{},
		&widget.Widget{},
	)
}
`

func writeForgetTestApp(t *testing.T) string {
	t.Helper()
	workDir := t.TempDir()
	dir := filepath.Join(workDir, "internal", "platform")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "database.go"), []byte(forgetTestDatabaseGo), 0o600); err != nil {
		t.Fatal(err)
	}
	return workDir
}

func TestEnsureForgetModelsRetiredRefusesAutoMigratedModel(t *testing.T) {
	workDir := writeForgetTestApp(t)
	err := ensureForgetModelsRetired(workDir, []migrations.Model{
		{ImportPath: "github.com/example/app/internal/widget", TypeName: "Widget"},
	})
	if err == nil {
		t.Fatal("ensureForgetModelsRetired() = nil, want refusal for an AutoMigrated model")
	}
	for _, want := range []string{"still registered in AutoMigrate", "Widget", "database.go"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to contain %q", err, want)
		}
	}
}

func TestEnsureForgetModelsRetiredAllowsRetiredModel(t *testing.T) {
	workDir := writeForgetTestApp(t)
	// Gizmo is not an AutoMigrate argument, so forgetting it is safe.
	err := ensureForgetModelsRetired(workDir, []migrations.Model{
		{ImportPath: "github.com/example/app/internal/gizmo", TypeName: "Gizmo"},
	})
	if err != nil {
		t.Fatalf("ensureForgetModelsRetired() = %v, want nil for a model absent from AutoMigrate", err)
	}
}

func TestEnsureForgetModelsRetiredSkipsWhenNoPlatformFile(t *testing.T) {
	// A non-scaffolded layout has no internal/platform/database.go; the check is
	// skipped rather than failing.
	err := ensureForgetModelsRetired(t.TempDir(), []migrations.Model{
		{ImportPath: "github.com/example/app/internal/widget", TypeName: "Widget"},
	})
	if err != nil {
		t.Fatalf("ensureForgetModelsRetired() = %v, want nil when there is no platform file", err)
	}
}

func TestMakeMigrationsRejectsInvalidForgetModel(t *testing.T) {
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	err := ExecuteRoot(context.Background(), NewRoot(stdout, stderr), []string{
		"db", "makemigrations", "create_products",
		"--model", "github.com/example/app/internal/product.Product",
		"--forget-model", "not-a-valid-spec",
	})
	if err == nil {
		t.Fatal("gombit db makemigrations --forget-model not-a-valid-spec: error = nil, want error")
	}
	if !strings.Contains(err.Error(), "import/path.TypeName") {
		t.Fatalf("error = %q, want it to explain the model spec format", err)
	}
}

func TestMakeMigrationsHelpDescribesForgetModel(t *testing.T) {
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	err := ExecuteRoot(context.Background(), NewRoot(stdout, stderr), []string{"db", "makemigrations", "--help"})
	if err != nil {
		t.Fatalf("gombit db makemigrations --help: %v", err)
	}
	out := stdout.String() + stderr.String()
	if !strings.Contains(out, "--forget-model") {
		t.Fatalf("help missing --forget-model:\n%s", out)
	}
	if !strings.Contains(out, "Merged with models already registered") {
		t.Fatalf("help missing note about the persisted model registry:\n%s", out)
	}
}
