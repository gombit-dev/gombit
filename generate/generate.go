// Package generate implements `gombit generate`: it regenerates a gombit
// application's model-first resource files (the generator-owned *.gen.go DTOs and
// handler) from the app's real compiled GORM models, and, with Check, verifies
// they are up to date without mutating anything.
//
// Model-first generation must read the ACTUAL current model — the one the
// developer may have edited — so it cannot work off the make-resource CLI grammar
// or off source it just wrote. Like migrations (ADR-012), it uses Program Mode: a
// throwaway `main` is written into the app module and `go run`, importing the
// app's model packages and calling resourcegen.RenderResource on each. That
// loader is a pure function — it emits the artifacts (path, bytes, ownership) as
// JSON and has no write access to the app tree. This package (the parent) owns
// all filesystem policy: which files are overwritten, which are seeded once, and
// what --check compares.
package generate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gombit-dev/gombit/migrations"
	"github.com/gombit-dev/gombit/resourcegen"
)

// Options configures a generate run.
type Options struct {
	WorkDir string
	// Check renders in temp space and compares against the committed
	// generator-owned files instead of writing; it fails on any difference and
	// never mutates the tree (a CI drift gate). Seed-once files are ignored.
	Check bool
	// DryRun prints what a write run would do without writing.
	DryRun bool
	Stdout io.Writer
	Stderr io.Writer

	runner commandRunner
}

// commandRunner runs an external command; execRunner is the default, and tests
// inject a fake to avoid a real `go run`. Mirrors the migrations package.
type commandRunner interface {
	Run(ctx context.Context, dir, name string, args []string, stdout, stderr io.Writer) error
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, dir, name string, args []string, stdout, stderr io.Writer) error {
	// #nosec G204 -- generate intentionally runs `go run` on a loader it wrote.
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

func (opts *Options) withDefaults() {
	if opts.WorkDir == "" {
		opts.WorkDir = "."
	}
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}
	if opts.runner == nil {
		opts.runner = execRunner{}
	}
}

// Generate regenerates (or, with Check, verifies) the model-first resource files.
func Generate(ctx context.Context, opts Options) error {
	opts.withDefaults()

	absWorkDir, err := filepath.Abs(opts.WorkDir)
	if err != nil {
		return fmt.Errorf("generate: resolve work dir: %w", err)
	}
	if err := resourcegen.ValidateAppLayout(absWorkDir); err != nil {
		return err
	}
	module, err := resourcegen.ReadModulePath(absWorkDir)
	if err != nil {
		return err
	}

	models, err := discoverResources(absWorkDir, module)
	if err != nil {
		return err
	}
	if len(models) == 0 {
		_, _ = fmt.Fprintln(opts.Stdout, "generate: no model-first resources found; nothing to do")
		return nil
	}
	// Fail closed on the legacy layout BEFORE running the loader or writing
	// anything: a package that still has the human-owned handler.go would get a
	// second, generated Handler/Register and stop compiling. It must be migrated
	// first, not half-converted.
	if err := ensureNoLegacyLayout(absWorkDir, models); err != nil {
		return err
	}
	// Also fail closed if a resource directory's files declare a package name
	// different from the directory basename we render the *.gen.go under: writing
	// them would leave two package clauses in one directory (a build break) while
	// reporting success. reflect gives the import path, not the declared name, so
	// the basename is only a heuristic — verify it against what is on disk.
	if err := ensurePackageMatchesDir(absWorkDir, models); err != nil {
		return err
	}

	artifacts, err := loadArtifacts(ctx, opts, absWorkDir, models, nil)
	if err != nil {
		return err
	}

	if opts.Check {
		return checkArtifacts(absWorkDir, artifacts)
	}
	return applyArtifacts(opts, absWorkDir, artifacts)
}

// PlanResource runs the Program-Mode phase-2 render for a single pending
// (not-yet-written) resource and returns its generator-owned + seed artifacts
// WITHOUT writing anything. The pending model is compiled through a Go build
// overlay (pend.Overlay), so the loader sees the resource as make resource will
// commit it while the real app tree stays untouched and local replace directives
// keep their meaning — go run executes from the real module root. It is make
// resource's phase-2 preflight: a model that would not compile fails here, before
// any file is written.
func PlanResource(ctx context.Context, opts Options, pend resourcegen.Pending) ([]resourcegen.GeneratedArtifact, error) {
	opts.withDefaults()
	absWorkDir, err := filepath.Abs(opts.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("generate: resolve work dir: %w", err)
	}
	if err := resourcegen.ValidateAppLayout(absWorkDir); err != nil {
		return nil, err
	}
	model := migrations.Model{ImportPath: pend.ImportPath, TypeName: pend.TypeName}
	return loadArtifacts(ctx, opts, absWorkDir, []migrations.Model{model}, pend.Overlay)
}

// discoverResources returns the app's model-first resources: the feature
// packages under internal/ that carry the resource marker (resourcegen.
// ResourceMarkerFile), each paired with its persisted model. Discovery is by the
// marker, NOT by AutoMigrate membership — AutoMigrate means "persisted", which a
// join table or audit-log model also is; only a marked package is a generated
// CRUD resource. make resource (slice 5b) writes the marker at bootstrap; here we
// only read it. The result is sorted by import path for deterministic loader
// output.
//
// The marker identifies the package; the model type comes from the AutoMigrate
// call. Because generated files are package-level (internal/<pkg>/dto.gen.go),
// a marked package must resolve to EXACTLY ONE AutoMigrate model — zero (the
// resource has no persisted model registered) or several (ambiguous target) both
// fail closed with a diagnostic rather than guess.
func discoverResources(absWorkDir, module string) ([]migrations.Model, error) {
	marked, err := markedResourcePackages(absWorkDir)
	if err != nil {
		return nil, err
	}
	if len(marked) == 0 {
		return nil, nil
	}

	dbPath := filepath.Join(absWorkDir, "internal", "platform", "database.go")
	// #nosec G304 -- database.go inside the validated application work dir
	src, err := os.ReadFile(dbPath)
	if err != nil {
		return nil, fmt.Errorf("generate: read %s: %w", filepath.ToSlash(filepath.Join("internal", "platform", "database.go")), err)
	}
	all, err := resourcegen.CollectAutoMigrateModels(src)
	if err != nil {
		return nil, fmt.Errorf("generate: collect models: %w", err)
	}

	out := make([]migrations.Model, 0, len(marked))
	for _, pkg := range marked {
		importPath := module + "/internal/" + pkg
		var matches []migrations.Model
		for _, m := range all {
			if m.ImportPath == importPath {
				matches = append(matches, m)
			}
		}
		switch len(matches) {
		case 1:
			out = append(out, matches[0])
		case 0:
			return nil, fmt.Errorf("generate: resource package internal/%s is marked (%s) but no model in it is registered with AutoMigrate; add its model to internal/platform/database.go", pkg, resourcegen.ResourceMarkerFile)
		default:
			names := make([]string, len(matches))
			for i, m := range matches {
				names[i] = m.TypeName
			}
			sort.Strings(names)
			return nil, fmt.Errorf("generate: resource package internal/%s registers %d models with AutoMigrate (%s), but a resource package must have exactly one; split the extra models into their own packages", pkg, len(matches), strings.Join(names, ", "))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ImportPath != out[j].ImportPath {
			return out[i].ImportPath < out[j].ImportPath
		}
		return out[i].TypeName < out[j].TypeName
	})
	return out, nil
}

// markedResourcePackages returns the names of feature packages under internal/
// that carry the resource marker, sorted.
func markedResourcePackages(absWorkDir string) ([]string, error) {
	internalDir := filepath.Join(absWorkDir, "internal")
	entries, err := os.ReadDir(internalDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("generate: read internal/: %w", err)
	}
	var pkgs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		marker := filepath.Join(internalDir, e.Name(), resourcegen.ResourceMarkerFile)
		if _, err := os.Stat(marker); err == nil {
			pkgs = append(pkgs, e.Name())
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("generate: stat %s: %w", filepath.Join("internal", e.Name(), resourcegen.ResourceMarkerFile), err)
		}
	}
	sort.Strings(pkgs)
	return pkgs, nil
}

// ensureNoLegacyLayout refuses to run against a resource that still has the
// legacy human-owned handler.go (see package doc).
func ensureNoLegacyLayout(absWorkDir string, models []migrations.Model) error {
	for _, m := range models {
		pkg := path.Base(m.ImportPath)
		legacy := filepath.Join(absWorkDir, "internal", pkg, "handler.go")
		if _, err := os.Stat(legacy); err == nil {
			return fmt.Errorf("generate: resource %q uses the legacy handler layout (internal/%s/handler.go); migrate it to the model-first layout before regenerating", pkg, pkg)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("generate: stat internal/%s/handler.go: %w", pkg, err)
		}
	}
	return nil
}

// ensurePackageMatchesDir verifies that every existing Go file in each resource
// directory declares the package name the generator will write its files under —
// path.Base(importPath), the directory basename. RenderResource emits the
// *.gen.go and hooks.go as `package <basename>`, so if a file already there (a
// hand-renamed model, a copy into a differently named directory) declares a
// different package, the written files would not compile alongside it. reflect
// exposes only the import path, never the declared package name, so this on-disk
// cross-check is the only way to catch the mismatch — and it must happen before
// any write, so generate never leaves an app in a non-building state and reports
// success. Test files are skipped (a foo_test / foo package split is legal).
func ensurePackageMatchesDir(absWorkDir string, models []migrations.Model) error {
	for _, m := range models {
		pkg := path.Base(m.ImportPath)
		dir := filepath.Join(absWorkDir, "internal", pkg)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // no directory yet; the loader will surface a missing package
			}
			return fmt.Errorf("generate: read internal/%s: %w", pkg, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			clause, err := packageClauseOf(filepath.Join(dir, name))
			if err != nil {
				return err
			}
			if clause != pkg {
				return fmt.Errorf("generate: internal/%s/%s declares package %q, but model-first generation writes files as package %q (the directory name); rename the package to match the directory before regenerating", pkg, name, clause, pkg)
			}
		}
	}
	return nil
}

// packageClauseOf returns the declared package name of a Go file, parsing only
// the package clause (cheap; no function bodies).
func packageClauseOf(file string) (string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.PackageClauseOnly)
	if err != nil {
		return "", fmt.Errorf("generate: parse package clause of %s: %w", file, err)
	}
	return f.Name.Name, nil
}

// loadArtifacts writes the Program-Mode loader into a temp dir under the app,
// `go run`s it, and decodes the artifacts it emits. The loader runs with the app
// as its working directory so `go run` resolves the app module and its internal
// packages.
func loadArtifacts(ctx context.Context, opts Options, absWorkDir string, models []migrations.Model, overlay map[string][]byte) ([]resourcegen.GeneratedArtifact, error) {
	tmpRoot := filepath.Join(absWorkDir, ".gombit")
	_, statErr := os.Stat(tmpRoot)
	tmpRootExisted := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, fmt.Errorf("generate: inspect temp root: %w", statErr)
	}
	if err := os.MkdirAll(tmpRoot, 0o750); err != nil {
		return nil, fmt.Errorf("generate: create temp root: %w", err)
	}
	tmpDir, err := os.MkdirTemp(tmpRoot, "generate-*")
	if err != nil {
		return nil, fmt.Errorf("generate: create temp dir: %w", err)
	}
	defer func() {
		_ = os.RemoveAll(tmpDir)
		if !tmpRootExisted {
			_ = os.Remove(tmpRoot)
		}
	}()

	loaderDir := filepath.Join(tmpDir, "loader")
	if err := os.MkdirAll(loaderDir, 0o750); err != nil {
		return nil, fmt.Errorf("generate: create loader dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(loaderDir, "main.go"), []byte(loaderSource(models)), 0o600); err != nil {
		return nil, fmt.Errorf("generate: write loader: %w", err)
	}

	loaderRel, err := filepath.Rel(absWorkDir, loaderDir)
	if err != nil {
		return nil, fmt.Errorf("generate: resolve loader path: %w", err)
	}
	// Stage any pending (not-yet-written) sources as a Go build overlay so the
	// loader compiles the resource as it will be committed, without touching the
	// real tree. Backing files live under the same temp dir and vanish with it.
	overlayArg, err := writeOverlay(tmpDir, overlay)
	if err != nil {
		return nil, err
	}
	var stdout bytes.Buffer
	goArgs := []string{"run", "-mod=mod"}
	goArgs = append(goArgs, overlayArg...)
	goArgs = append(goArgs, "./"+filepath.ToSlash(loaderRel))
	if err := opts.runner.Run(ctx, absWorkDir, "go", goArgs, &stdout, opts.Stderr); err != nil {
		return nil, fmt.Errorf("generate: run resource loader: %w", err)
	}

	var artifacts []resourcegen.GeneratedArtifact
	if err := json.Unmarshal(stdout.Bytes(), &artifacts); err != nil {
		return nil, fmt.Errorf("generate: decode loader output: %w", err)
	}
	return artifacts, nil
}

// writeOverlay materializes an overlay (real absolute path -> staged source) as
// backing files plus the JSON manifest `go run -overlay` reads, and returns the
// `-overlay=<path>` argument (empty when there is nothing to stage). Backing files
// live under tmpDir, which the caller removes after the run.
func writeOverlay(tmpDir string, overlay map[string][]byte) ([]string, error) {
	if len(overlay) == 0 {
		return nil, nil
	}
	ovDir := filepath.Join(tmpDir, "overlay")
	if err := os.MkdirAll(ovDir, 0o750); err != nil {
		return nil, fmt.Errorf("generate: create overlay dir: %w", err)
	}
	replace := make(map[string]string, len(overlay))
	i := 0
	// Sort the real paths so backing filenames are assigned deterministically.
	reals := make([]string, 0, len(overlay))
	for real := range overlay {
		reals = append(reals, real)
	}
	sort.Strings(reals)
	for _, real := range reals {
		backing := filepath.Join(ovDir, fmt.Sprintf("stage%d.go", i))
		i++
		if err := os.WriteFile(backing, overlay[real], 0o600); err != nil {
			return nil, fmt.Errorf("generate: write overlay backing file: %w", err)
		}
		replace[real] = backing
	}
	manifest, err := json.Marshal(struct{ Replace map[string]string }{Replace: replace})
	if err != nil {
		return nil, fmt.Errorf("generate: encode overlay manifest: %w", err)
	}
	ovPath := filepath.Join(tmpDir, "overlay.json")
	if err := os.WriteFile(ovPath, manifest, 0o600); err != nil {
		return nil, fmt.Errorf("generate: write overlay manifest: %w", err)
	}
	return []string{"-overlay=" + ovPath}, nil
}

// loaderSource builds the Program-Mode main: it imports resourcegen and each
// model's package, renders every resource, and writes the flattened artifacts to
// stdout as JSON. Kept minimal on purpose — all filesystem policy lives in the
// parent.
func loaderSource(models []migrations.Model) string {
	var b strings.Builder
	b.WriteString("package main\n\n")
	b.WriteString("import (\n")
	b.WriteString("\t\"encoding/json\"\n")
	b.WriteString("\t\"fmt\"\n")
	b.WriteString("\t\"os\"\n\n")
	b.WriteString("\t\"github.com/gombit-dev/gombit/resourcegen\"\n")
	for i, m := range models {
		fmt.Fprintf(&b, "\tmodel%d %q\n", i, m.ImportPath)
	}
	b.WriteString(")\n\n")
	b.WriteString("func main() {\n")
	b.WriteString("\tvar all []resourcegen.GeneratedArtifact\n")
	for i, m := range models {
		fmt.Fprintf(&b, "\t{\n")
		fmt.Fprintf(&b, "\t\tarts, err := resourcegen.RenderResource(&model%d.%s{}, %q)\n", i, m.TypeName, path.Base(m.ImportPath))
		fmt.Fprintf(&b, "\t\tif err != nil {\n")
		fmt.Fprintf(&b, "\t\t\tfmt.Fprintf(os.Stderr, \"render %s: %%v\\n\", err)\n", m.TypeName)
		fmt.Fprintf(&b, "\t\t\tos.Exit(1)\n")
		fmt.Fprintf(&b, "\t\t}\n")
		fmt.Fprintf(&b, "\t\tall = append(all, arts...)\n")
		fmt.Fprintf(&b, "\t}\n")
	}
	b.WriteString("\tif err := json.NewEncoder(os.Stdout).Encode(all); err != nil {\n")
	b.WriteString("\t\tfmt.Fprintf(os.Stderr, \"encode artifacts: %v\\n\", err)\n")
	b.WriteString("\t\tos.Exit(1)\n")
	b.WriteString("\t}\n")
	b.WriteString("}\n")
	return b.String()
}

// checkArtifacts compares generator-owned artifacts against the committed files
// and returns a non-nil error listing every stale/missing one. Seed-once files
// are human-owned and never compared. It never writes.
func checkArtifacts(absWorkDir string, artifacts []resourcegen.GeneratedArtifact) error {
	var stale []string
	for _, a := range artifacts {
		if a.Ownership != resourcegen.GeneratorOwned {
			continue
		}
		abs := filepath.Join(absWorkDir, filepath.FromSlash(a.Path))
		// #nosec G304 -- path is a generator-owned file under the app work dir
		existing, err := os.ReadFile(abs)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				stale = append(stale, a.Path+" (missing)")
				continue
			}
			return fmt.Errorf("generate: read %s: %w", a.Path, err)
		}
		if !bytes.Equal(existing, a.Content) {
			stale = append(stale, a.Path)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		return fmt.Errorf("generate: generated files are stale; run `gombit generate`:\n\t%s", strings.Join(stale, "\n\t"))
	}
	return nil
}

// applyArtifacts writes generator-owned files (always) and seeds seed-once files
// (only when absent). DryRun reports the plan without writing.
func applyArtifacts(opts Options, absWorkDir string, artifacts []resourcegen.GeneratedArtifact) error {
	for _, a := range artifacts {
		abs := filepath.Join(absWorkDir, filepath.FromSlash(a.Path))

		if a.Ownership == resourcegen.SeedOnce {
			if _, err := os.Stat(abs); err == nil {
				_, _ = fmt.Fprintf(opts.Stdout, "keep   %s (seed-once, already present)\n", a.Path)
				continue
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("generate: stat %s: %w", a.Path, err)
			}
		}

		verb := "write"
		if a.Ownership == resourcegen.SeedOnce {
			verb = "seed "
		}
		if opts.DryRun {
			_, _ = fmt.Fprintf(opts.Stdout, "would %s %s\n", strings.TrimSpace(verb), a.Path)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
			return fmt.Errorf("generate: create dir for %s: %w", a.Path, err)
		}
		if err := os.WriteFile(abs, a.Content, 0o600); err != nil {
			return fmt.Errorf("generate: write %s: %w", a.Path, err)
		}
		_, _ = fmt.Fprintf(opts.Stdout, "%s %s\n", verb, a.Path)
	}
	return nil
}
