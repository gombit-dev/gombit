//go:build !integration

package resourcegen

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestDecimalDefaultIsAString typechecks the generated form default. It lives
// outside the integration build so the Postgres and MySQL jobs, which run
// `go test -tags integration ./resourcegen` without installing adminui
// node_modules, do not execute it. The test job installs that typescript
// binary first. A missing binary fails with the exec error.
func TestDecimalDefaultIsAString(t *testing.T) {
	fields, err := parseFields([]string{"price:decimal:default=19.99"}, "person")
	if err != nil {
		t.Fatal(err)
	}
	name, err := parseResourceName("Person")
	if err != nil {
		t.Fatal(err)
	}
	form := renderFormTSX(newRenderContext("github.com/example/demo", name, fields, "/api/v1", "minimal", false, false))
	start := strings.Index(form, "type FormValues")
	end := strings.Index(form, "export function")
	if start < 0 || end < start {
		t.Fatalf("form missing FormValues:\n%s", form)
	}
	values := form[start:end]
	defStart := strings.Index(form, "defaultValues:")
	if defStart < 0 {
		t.Fatalf("form missing defaultValues:\n%s", form)
	}
	rel := form[defStart:]
	brace := strings.Index(rel, "}")
	if brace < 0 {
		t.Fatalf("form missing defaultValues close:\n%s", form)
	}
	obj := strings.TrimSpace(strings.TrimPrefix(rel[:brace+1], "defaultValues:"))
	snippet := values + "const check: FormValues = " + obj + ";\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "form.ts")
	if err := os.WriteFile(path, []byte(snippet), 0o600); err != nil {
		t.Fatal(err)
	}
	tsc := filepath.Join(resourcegenModuleRoot(t), "internal", "adminui", "node_modules", "typescript", "bin", "tsc")
	cmd := exec.Command(tsc, "--strict", "--noEmit", "--target", "ES2022", path) // #nosec G204 -- tsc is the repo's own typescript binary
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("decimal default does not typecheck as a string: %v\n%s\n--- snippet ---\n%s", err, out, snippet)
	}
}
