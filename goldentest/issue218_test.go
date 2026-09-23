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

// TestIssue218ModelFirstResolvesDTODrift is the regression for #218: the generated
// handler DTO used to be frozen at generation time and hand-owned, so evolving the
// model rejected new fields on input, dropped them from responses, and silently
// zero-filled NOT NULL columns, with nothing to detect the divergence. The
// model-first redesign (RESGEN-1) makes the model the source of truth, derives the
// *.gen.go from it, and gates staleness with `gombit generate --check`.
//
// This reproduces the exact #218 scenario (scaffold a resource, then hand-add a
// NOT NULL FK + a NOT NULL timestamp to the model) and asserts:
//
//   - detectability: `gombit generate --check` (the function the CLI wraps) FAILS
//     with "stale" on the evolved-but-not-regenerated tree, and PASSES after
//     regeneration;
//   - the source-level contract regeneration establishes: the evolved fields land
//     in the request and response DTOs with their wire names AND are copied by the
//     generated mappers (row from body on create, data from row on read) — what
//     makes them accepted, persisted, and returned rather than present-but-discarded
//     (ADR-016's fourth #218 mode); the NOT NULL value type is a required
//     (non-pointer) field;
//   - the regenerated app compiles.
//
// This package does not boot HTTP, so it does not observe the 422 an omitted
// required field produces (that is Huma's behavior for a non-pointer field); it
// locks the derivation — the model's fields flowing through the DTOs and mappers.
func TestIssue218ModelFirstResolvesDTODrift(t *testing.T) {
	appDir := scaffoldDemo(t)

	// Model-first scaffold of a resource with a single scalar (as in the #218 repro,
	// `make resource Ownership note:string`). resourcegen.Generate writes the phase-1
	// scaffold (model + marker + wiring); the generator-owned *.gen.go come from
	// gombit generate below.
	stdout := new(bytes.Buffer)
	if err := resourcegen.Generate(context.Background(), resourcegen.Options{
		WorkDir:        appDir,
		Name:           "Ownership",
		Fields:         []string{"note:string"},
		SkipMigrations: true,
		Stdout:         stdout,
		Stderr:         io.Discard,
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

	// Regenerate: the DTOs, mappers, and handler are re-derived from the current model.
	mustGenerate(t, copyDir, false)

	// The regenerated files are now fresh — the drift gate round-trips.
	if err := generateCheck(t, copyDir); err != nil {
		t.Fatalf("gombit generate --check must pass after regeneration, got: %v", err)
	}

	dto := readFileString218(t, filepath.Join(copyDir, "internal", "ownership", "dto.gen.go"))
	flat := collapseWS(dto) // collapse gofmt column alignment for substring checks
	// Scope wire-name checks to the struct they belong to — a json tag in the
	// response DTO must not satisfy a create-body claim (both DTOs carry the field).
	createBody := collapseWS(sliceBetween(dto, "ownershipCreateBody struct", "}"))
	responseData := collapseWS(sliceBetween(dto, "ownershipData struct", "}"))

	// Mode 1 (new fields rejected on input): the evolved FK is in the CREATE BODY
	// with its wire name, AND the create mapper copies it into the row — accepted and
	// persisted, not present-but-discarded (ADR-016's fourth mode). The mapper string
	// stays file-global; it occurs only in ownershipFromCreateBody.
	if !strings.Contains(createBody, "EngineID uint") || !strings.Contains(createBody, `json:"engine_id"`) {
		t.Fatalf("create body must accept the NOT NULL FK as engine_id; body:\n%s", createBody)
	}
	if !strings.Contains(flat, "row.EngineID = body.EngineID") {
		t.Fatalf("create mapper must copy the FK into the row; dto:\n%s", dto)
	}
	// Mode 3 (silent NOT NULL zero-fill): the NOT NULL value type is a required
	// (non-pointer) create field with its wire name — an optional column would render
	// as *time.Time — and is likewise copied by the create mapper, so it cannot be
	// dropped from the request struct and then zero-filled.
	if !strings.Contains(createBody, "StartsAt time.Time") || strings.Contains(createBody, "*time.Time") || !strings.Contains(createBody, `json:"starts_at"`) {
		t.Fatalf("NOT NULL time must be a required (non-pointer) starts_at create field; body:\n%s", createBody)
	}
	if !strings.Contains(flat, "row.StartsAt = body.StartsAt") {
		t.Fatalf("create mapper must copy the NOT NULL timestamp; dto:\n%s", dto)
	}
	// Mode 2 (new fields never returned): they are in the RESPONSE DTO with their
	// wire names, and the response mapper copies them out of the row.
	if !strings.Contains(responseData, `json:"engine_id"`) || !strings.Contains(responseData, `json:"starts_at"`) {
		t.Fatalf("response DTO must expose engine_id + starts_at; body:\n%s", responseData)
	}
	for _, want := range []string{"EngineID: row.EngineID", "StartsAt: row.StartsAt"} {
		if !strings.Contains(flat, want) {
			t.Fatalf("response mapper must copy the evolved fields (missing %q); dto:\n%s", want, dto)
		}
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
