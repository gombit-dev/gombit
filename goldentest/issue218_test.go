package goldentest

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/generate"
	"github.com/gombit-dev/gombit/resourcegen"
)

// TestIssue218ModelFirstResolvesDTODrift is the end-to-end regression for #218:
// the generated handler DTO used to be frozen at generation time and hand-owned,
// so evolving the model rejected new fields on input, dropped them from responses,
// and silently zero-filled NOT NULL columns — with nothing to detect the
// divergence. The model-first redesign (RESGEN-1) makes the model the source of
// truth and derives the *.gen.go from it, and `gombit generate --check` detects
// staleness. This test reproduces the exact #218 scenario — scaffold a resource,
// then hand-add NOT NULL FK + time columns to the model — and asserts every
// failure mode is now resolved.
func TestIssue218ModelFirstResolvesDTODrift(t *testing.T) {
	appDir := scaffoldDemo(t)

	// Model-first scaffold of a resource with a single scalar (as in the #218 repro,
	// `make resource Ownership note:string`). resourcegen.Generate writes the phase-1
	// scaffold (model + marker + wiring); the generator-owned *.gen.go come from
	// gombit generate below.
	stdout := new(bytes.Buffer)
	if err := resourcegen.Generate(context.Background(), resourcegen.Options{
		WorkDir:  appDir,
		Name:     "Ownership",
		Fields:   []string{"note:string"},
		AtlasBin: missingAtlas,
		Stdout:   stdout,
		Stderr:   io.Discard,
	}); err != nil {
		t.Fatalf("make resource Ownership: %v\n%s", err, stdout.String())
	}

	// Work in a temp copy carrying the local framework replace so generate's
	// Program-Mode loader (`go run`) resolves this working tree.
	copyDir := filepath.Join(t.TempDir(), "issue218")
	copyTree(t, appDir, copyDir)
	appendLocalReplace(t, copyDir)
	runGo(t, copyDir, "mod", "tidy")

	// Generate the initial (note-only) generator-owned files, then confirm they are
	// fresh — the baseline the drift check guards.
	mustGenerate(t, copyDir, false)
	if err := generateCheck(t, copyDir); err != nil {
		t.Fatalf("freshly generated resource must pass --check, got: %v", err)
	}

	// The #218 hand-edit: evolve the model with a NOT NULL foreign key and a NOT
	// NULL timestamp the original generated DTO never learned about.
	modelPath := filepath.Join(copyDir, "internal", "ownership", "ownership.go")
	writeFile(t, modelPath, `package ownership

import (
	"time"

	"gorm.io/gorm"
)

type Ownership struct {
	gorm.Model
	Note     string    `+"`gorm:\"size:255\"`"+`
	EngineID uint      `+"`gorm:\"not null\"`"+`
	StartsAt time.Time `+"`gorm:\"not null\"`"+`
}
`)

	// #218's core ask: "model↔handler divergence should be detectable." It now is —
	// the committed *.gen.go are stale against the evolved model, so --check fails.
	if err := generateCheck(t, copyDir); err == nil {
		t.Fatal("gombit generate --check must fail on a model that evolved without regenerating (#218 detectability)")
	} else if !strings.Contains(err.Error(), "stale") {
		t.Fatalf("--check error should report stale files, got: %v", err)
	}

	// Regenerate: the DTOs are re-derived from the current model.
	mustGenerate(t, copyDir, false)

	dto := readFileString218(t, filepath.Join(copyDir, "internal", "ownership", "dto.gen.go"))
	// Collapse gofmt's column alignment so field checks are whitespace-insensitive.
	createBody := collapseWS(sliceBetween(dto, "ownershipCreateBody struct", "}"))
	// Failure mode 1 (new fields rejected on input) is fixed: the evolved fields are
	// now in the create body, so a client can supply them.
	if !strings.Contains(createBody, "EngineID uint") {
		t.Fatalf("create body must accept the new NOT NULL FK; got:\n%s", createBody)
	}
	// Failure mode 3 (silent NOT NULL zero-fill) is fixed: the NOT NULL columns are
	// non-pointer fields, which Huma treats as required — omitting them is a 422, not
	// a silently zero-filled row. (An optional value type would render as a pointer.)
	if !strings.Contains(createBody, "StartsAt time.Time") || strings.Contains(createBody, "*time.Time") {
		t.Fatalf("NOT NULL time column must be a required (non-pointer) create field; got:\n%s", createBody)
	}
	// Failure mode 2 (new fields never returned) is fixed: they are in the response.
	data := collapseWS(sliceBetween(dto, "ownershipData struct", "}"))
	if !strings.Contains(data, "EngineID uint") || !strings.Contains(data, "StartsAt time.Time") {
		t.Fatalf("response DTO must expose the evolved fields; got:\n%s", data)
	}

	// And the regenerated app still compiles.
	runGo(t, copyDir, "build", "./...")
}

func mustGenerate(t *testing.T, dir string, check bool) {
	t.Helper()
	if err := generate.Generate(context.Background(), generate.Options{
		WorkDir: dir,
		Check:   check,
		Stdout:  io.Discard,
		Stderr:  io.Discard,
	}); err != nil {
		t.Fatalf("gombit generate (check=%v): %v", check, err)
	}
}

func generateCheck(t *testing.T, dir string) error {
	t.Helper()
	return generate.Generate(context.Background(), generate.Options{
		WorkDir: dir,
		Check:   true,
		Stdout:  io.Discard,
		Stderr:  io.Discard,
	})
}

func runGo(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("go", args...) // #nosec G204 -- test helper; go subcommand args are fixed literals from callers
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func readFileString218(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) // #nosec G304 -- test reads a generated file under its own temp dir
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// sliceBetween returns the substring of s from the first occurrence of start
// through the next occurrence of end (inclusive of neither delimiter's role beyond
// bounding), for coarse structural assertions on generated source.
func sliceBetween(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	rest := s[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		return rest
	}
	return rest[:j]
}

// collapseWS replaces every run of whitespace with a single space, so assertions
// on generated source do not depend on gofmt's column alignment.
func collapseWS(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
