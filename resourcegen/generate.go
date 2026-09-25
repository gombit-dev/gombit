package resourcegen

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/migrations"
)

// Generate scaffolds a feature-package resource (phase 1 only): the human-owned
// model, the .gombit-resource marker, the AST-wired registration/AutoMigrate, and
// the frontend pages. It does NOT produce the generator-owned *.gen.go — gombit
// generate does, from the committed model. It is the primitive the golden tests
// and app fixtures use; the user-facing `gombit make resource` (cli/make.go)
// composes this phase with the Program-Mode phase 2 atomically, via Plan.
func Generate(ctx context.Context, opts Options) error {
	plan, err := Plan(ctx, opts)
	if err != nil {
		return err
	}
	if plan.opts.DryRun {
		// Phase-1-only preview: append the files gombit generate would produce, from
		// the generator's own path contract (not a hand-maintained duplicate).
		return plan.PrintPlan(nil)
	}
	if err := plan.Apply(nil); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(plan.opts.Stdout, "GORM model is Atlas-loader ready: %s\n", plan.spec.ModelSpec); err != nil {
		return err
	}
	return plan.Migrate(ctx)
}

// Pending describes the resource a make resource run is about to scaffold, so the
// Program-Mode phase-2 preflight can compile the model through a Go build overlay
// without the model — or its .gombit-resource marker — existing on disk yet.
type Pending struct {
	ImportPath string // module/internal/<pkg>
	TypeName   string // Book
	Pkg        string // book
	// Overlay maps real absolute paths to the source Program Mode compiles instead
	// of what is on disk, so the phase-2 preflight sees the resource exactly as this
	// run will commit it while the real tree stays untouched (nil when the committed
	// package already matches disk). It stages the model when make resource will
	// (re)write it, and — for a --force re-scaffold that changes the model — stubs
	// the package's other .go files so the changed model compiles without the stale
	// generated files this run is about to replace.
	Overlay map[string][]byte
}

// ResourcePlan is a fully computed, not-yet-applied resource scaffold: the phase-1
// files, the Pending model the phase-2 overlay preflight compiles, and the
// AutoMigrate model set the post-commit migration needs. Planning writes nothing; a
// caller applies the plan (optionally merged with the phase-2 artifacts) as one
// transaction only after the whole operation has validated.
type ResourcePlan struct {
	Pending Pending
	Models  []migrations.Model

	files []fileSpec
	spec  renderContext
	opts  Options
}

// ModelSpec is the Atlas loader spec (import path + type) for the scaffolded model.
func (p *ResourcePlan) ModelSpec() string { return p.spec.ModelSpec }

// Plan computes a resource scaffold without writing anything. It runs every static
// check make resource can before touching the tree — app layout, name, HTTP path
// collision, and the legacy-layout guard — so a caller can preflight
// the whole operation (including the Program-Mode phase 2) and apply the result
// atomically.
func Plan(ctx context.Context, opts Options) (*ResourcePlan, error) {
	if ctx == nil {
		return nil, errors.New("resourcegen: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := opts.normalize(); err != nil {
		return nil, err
	}
	if err := opts.validateAppLayout(); err != nil {
		return nil, err
	}

	name, err := parseResourceName(opts.Name)
	if err != nil {
		return nil, err
	}
	// Refuse to scaffold over a resource still on the legacy human-owned handler
	// layout: re-scaffolding would overwrite the human-edited model and drop a
	// resource marker beside a handler.go that gombit generate then refuses to
	// regenerate — a half-migration. Migrate by hand (see the migration guide), so
	// this fires even with --force.
	if err := ensureNotLegacyResource(opts.WorkDir, name.Package); err != nil {
		return nil, err
	}
	if err := checkHTTPPathConflict(opts.WorkDir, name); err != nil {
		return nil, err
	}
	fields, err := parseFields(opts.Fields, name.Package)
	if err != nil {
		return nil, err
	}
	// Fail closed on a missing Atlas here, in the library, not in a caller: whether
	// Atlas happens to be on PATH must never change what make resource commits
	// (#300). It runs after the input checks above, so a bad name or field is
	// reported as such rather than masked by an environment error, and before
	// anything is written — Plan writes nothing — so the failure stays atomic.
	if err := opts.ensureAtlas(); err != nil {
		return nil, err
	}
	module, err := readModulePath(opts.WorkDir)
	if err != nil {
		return nil, err
	}
	ui := readUI(opts.WorkDir)
	// OpenAPI path keys stay /api/v1 (placeholder client / D8). The generated
	// SPA rewrites that prefix to the live GOMBIT_API_PREFIX at request time,
	// so make-resource pages must not bake gombit.yaml api_prefix.
	ctxData := newRenderContext(module, name, fields, defaultAPIPrefix, ui, opts.Service, opts.Repo)

	files, err := renderFeatureFiles(ctxData)
	if err != nil {
		return nil, err
	}
	for i := range files {
		if strings.HasSuffix(files[i].relPath, ".go") {
			formatted, fmtErr := format.Source(files[i].content)
			if fmtErr != nil {
				return nil, fmt.Errorf("resourcegen: format %s: %w", files[i].relPath, fmtErr)
			}
			files[i].content = formatted
		}
	}

	resources := collectFrontendResources(opts.WorkDir, name)
	files = append(files, fileSpec{
		relPath: resourcesTSRel,
		content: renderResourcesTS(resources),
	})

	mainPath := filepath.Join(opts.WorkDir, filepath.FromSlash(serverMainRel))
	platformPath := filepath.Join(opts.WorkDir, filepath.FromSlash(platformDBRel))
	// #nosec G304 -- application files under the user work dir
	mainSrc, err := os.ReadFile(mainPath)
	if err != nil {
		return nil, fmt.Errorf("resourcegen: read %s: %w", serverMainRel, err)
	}
	// #nosec G304 -- application files under the user work dir
	platformSrc, err := os.ReadFile(platformPath)
	if err != nil {
		return nil, fmt.Errorf("resourcegen: read %s: %w", platformDBRel, err)
	}
	newMain, err := AddImportAndRegister(mainSrc, ctxData.ImportPath, name.Package)
	if err != nil {
		return nil, err
	}
	newPlatform, err := AddAutoMigrateModel(platformSrc, ctxData.ImportPath, name.Package, name.TypeName)
	if err != nil {
		return nil, err
	}
	models, err := CollectAutoMigrateModels(newPlatform)
	if err != nil {
		return nil, err
	}
	models = ensureModel(models, migrations.Model{ImportPath: ctxData.ImportPath, TypeName: name.TypeName})
	files = append(files,
		fileSpec{relPath: serverMainRel, content: newMain, owned: true},
		fileSpec{relPath: platformDBRel, content: newPlatform, owned: true},
	)

	sort.Slice(files, func(i, j int) bool {
		return files[i].relPath < files[j].relPath
	})

	modelRel := fmt.Sprintf("internal/%s/%s.go", name.Package, name.FileBase)
	modelAbsPath := filepath.Join(opts.WorkDir, filepath.FromSlash(modelRel))
	var modelContent []byte
	for _, f := range files {
		if f.relPath == modelRel {
			modelContent = f.content
			break
		}
	}
	overlay, err := stageOverlay(opts, name.Package, modelAbsPath, modelContent)
	if err != nil {
		return nil, err
	}

	return &ResourcePlan{
		Pending: Pending{
			ImportPath: ctxData.ImportPath,
			TypeName:   name.TypeName,
			Pkg:        name.Package,
			Overlay:    overlay,
		},
		Models: models,
		files:  files,
		spec:   ctxData,
		opts:   opts,
	}, nil
}

// stageOverlay decides how the phase-2 preflight should see the resource package,
// as a Go build overlay (real absolute path -> staged source). It matches what this
// run will actually commit so generation renders from the right model:
//
//   - model absent (a fresh resource): stage the new model; nothing else exists.
//   - model present and unchanged: no overlay — compile the package as it is on
//     disk (already consistent).
//   - model present, differs, no --force: no overlay — the human-owned model is
//     kept (seed-once), so the on-disk model is the source of truth to render from.
//   - model present, differs, --force: stage the new model and stub the package's
//     other .go files, so the changed model compiles without the stale generated
//     files this run is about to replace.
func stageOverlay(opts Options, pkg, modelAbsPath string, modelContent []byte) (map[string][]byte, error) {
	onDisk, err := os.ReadFile(modelAbsPath) // #nosec G304 -- model path under the app work dir
	if errors.Is(err, os.ErrNotExist) {
		return map[string][]byte{modelAbsPath: modelContent}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("resourcegen: read %s: %w", filepath.Base(modelAbsPath), err)
	}
	if bytes.Equal(onDisk, modelContent) || !opts.Force {
		return nil, nil
	}
	overlay := map[string][]byte{modelAbsPath: modelContent}
	dir := filepath.Dir(modelAbsPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("resourcegen: read internal/%s: %w", pkg, err)
	}
	stub := []byte("package " + pkg + "\n")
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if p == modelAbsPath {
			continue
		}
		overlay[p] = stub
	}
	return overlay, nil
}

// PrintPlan writes the human-readable plan to stdout, changing nothing. With
// artifacts nil (the phase-1-only Generate path) it appends the files gombit
// generate would produce, from the generator's own path contract; with the real
// phase-2 artifacts (the composed make resource path) it prints the exact merged
// plan the command would apply.
func (p *ResourcePlan) PrintPlan(artifacts []GeneratedArtifact) error {
	planned, err := p.resolve(artifacts)
	if err != nil {
		return err
	}
	for _, item := range planned {
		if _, err := fmt.Fprintf(p.opts.Stdout, "%s %s\n", item.action, item.display); err != nil {
			return err
		}
	}
	if artifacts == nil {
		for _, a := range GeneratedArtifactPlan(p.Pending.Pkg) {
			verb := "would write"
			if a.Ownership == SeedOnce {
				verb = "would seed"
			}
			if _, err := fmt.Fprintf(p.opts.Stdout, "%s %s (gombit generate)\n", verb, a.Path); err != nil {
				return err
			}
		}
	}
	return nil
}

// Apply writes the plan (phase-1 files plus the phase-2 artifacts) to the tree as a
// single transaction: on any filesystem write error it rolls back every file it
// already wrote, so a failed apply leaves the tree as it found it. It prints each
// action as it goes.
func (p *ResourcePlan) Apply(artifacts []GeneratedArtifact) error {
	planned, err := p.resolve(artifacts)
	if err != nil {
		return err
	}
	return applyWrites(p.opts, planned)
}

// Migrate generates the Atlas migration for the scaffolded model. It is a
// post-commit step, deliberately not part of the atomic apply: a migration is a
// separate artifact, and the make resource transaction is about the source tree.
func (p *ResourcePlan) Migrate(ctx context.Context) error {
	return maybeMakeMigrations(ctx, p.opts, p.spec, p.Models)
}

// FinalOverlay returns a Go build overlay describing the resource package exactly
// as this run will commit it: every .go file in internal/<pkg> the plan actually
// writes (the model when re-scaffolded, the freshly rendered *.gen.go, a seeded
// hooks.go), staged at its real path. Files the plan keeps (a human-owned hooks.go)
// and any hand-written files not in the plan are intentionally absent, so a
// compile over this overlay reads them from disk — the true committed package.
// generate.ValidateResource compiles that package as the phase-2 preflight's second
// step, catching preserved customization that no longer matches the regenerated
// model.
func (p *ResourcePlan) FinalOverlay(artifacts []GeneratedArtifact) (map[string][]byte, error) {
	planned, err := p.resolve(artifacts)
	if err != nil {
		return nil, err
	}
	prefix := "internal/" + p.Pending.Pkg + "/"
	overlay := map[string][]byte{}
	for _, item := range planned {
		if !item.write || !strings.HasPrefix(item.relPath, prefix) || !strings.HasSuffix(item.relPath, ".go") {
			continue
		}
		overlay[filepath.Join(p.opts.WorkDir, filepath.FromSlash(item.relPath))] = item.content
	}
	return overlay, nil
}

// resolve turns the phase-1 file specs (and any phase-2 artifacts) into concrete
// write actions against the current tree, applying the seed-once and
// banner/overwrite rules. It reads the filesystem but never mutates it.
func (p *ResourcePlan) resolve(artifacts []GeneratedArtifact) ([]plannedFile, error) {
	planned, err := planWrites(p.opts, p.files)
	if err != nil {
		return nil, err
	}
	for _, art := range artifacts {
		item, err := resolveArtifact(p.opts, art)
		if err != nil {
			return nil, err
		}
		planned = append(planned, item)
	}
	sort.Slice(planned, func(i, j int) bool { return planned[i].relPath < planned[j].relPath })
	return planned, nil
}

// resolveArtifact resolves one phase-2 artifact into a write action: a
// generator-owned file is (re)written unless byte-identical; a seed-once file is
// written only when absent.
func resolveArtifact(opts Options, art GeneratedArtifact) (plannedFile, error) {
	full := filepath.Join(opts.WorkDir, filepath.FromSlash(art.Path))
	existing, err := os.ReadFile(full) // #nosec G304 -- generator output path
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return plannedFile{}, fmt.Errorf("resourcegen: read %s: %w", art.Path, err)
	}
	exists := err == nil
	if art.Ownership == SeedOnce {
		if exists {
			return plannedFile{relPath: art.Path, display: art.Path, action: "keep", write: false}, nil
		}
		return plannedFile{relPath: art.Path, display: art.Path, content: art.Content, action: "seed", write: true}, nil
	}
	if exists && bytes.Equal(existing, art.Content) {
		return plannedFile{relPath: art.Path, display: art.Path, action: "keep", write: false}, nil
	}
	action := "create"
	if exists {
		action = "modify"
	}
	return plannedFile{relPath: art.Path, display: art.Path, content: art.Content, action: action, write: true}, nil
}

type plannedFile struct {
	relPath string
	display string
	content []byte
	action  string
	write   bool
}

func planWrites(opts Options, files []fileSpec) ([]plannedFile, error) {
	var planned []plannedFile
	for _, file := range files {
		full := filepath.Join(opts.WorkDir, filepath.FromSlash(file.relPath))
		display, err := displayPath(opts.WorkDir, full)
		if err != nil {
			return nil, err
		}
		existing, err := os.ReadFile(full) // #nosec G304 -- generator output path
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("resourcegen: read %s: %w", display, err)
		}
		exists := err == nil
		if exists && bytes.Equal(existing, file.content) {
			continue
		}
		// A seed-once file is the developer's once scaffolded: never overwrite it
		// on a re-run (that would clobber their edits to the authoritative model),
		// unless --force explicitly re-scaffolds from the CLI spec. Report it as
		// kept so the run is transparent about preserving human-owned files.
		if exists && file.seedOnce && !opts.Force {
			planned = append(planned, plannedFile{
				relPath: file.relPath,
				display: display,
				action:  "keep",
				write:   false,
			})
			continue
		}
		if exists {
			if err := checkOverwrite(display, existing, file, opts.Force); err != nil {
				return nil, err
			}
		}
		action := "create"
		if exists {
			action = "modify"
		}
		planned = append(planned, plannedFile{
			relPath: file.relPath,
			display: display,
			content: file.content,
			action:  action,
			write:   true,
		})
	}
	return planned, nil
}

// ensureNotLegacyResource refuses to scaffold over a resource still on the legacy
// human-owned handler layout (internal/<pkg>/handler.go or routes.go). See Plan.
func ensureNotLegacyResource(workDir, pkg string) error {
	for _, base := range []string{"handler.go", "routes.go"} {
		p := filepath.Join(workDir, "internal", pkg, base)
		if _, err := os.Stat(p); err == nil {
			return fmt.Errorf("resourcegen: internal/%s is a legacy (handler-owned) resource (%s present); migrate it to the model-first layout before running make resource — see docs/migration-model-first-resources.md — rather than re-scaffolding over it", pkg, base)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("resourcegen: stat internal/%s/%s: %w", pkg, base, err)
		}
	}
	return nil
}

// applyWrites writes a resolved plan as one transaction: it prints each action, and
// on the first filesystem write failure rolls back every file it already wrote so
// the tree is left as it was found.
func applyWrites(opts Options, planned []plannedFile) error {
	var tx fsTx
	for _, item := range planned {
		if _, err := fmt.Fprintf(opts.Stdout, "%s %s\n", item.action, item.display); err != nil {
			_ = tx.rollback()
			return err
		}
		if !item.write {
			continue
		}
		full := filepath.Join(opts.WorkDir, filepath.FromSlash(item.relPath))
		if err := tx.write(full, item.content); err != nil {
			if rbErr := tx.rollback(); rbErr != nil {
				return fmt.Errorf("resourcegen: write %s: %w (rollback failed: %v)", item.display, err, rbErr)
			}
			return fmt.Errorf("resourcegen: write %s: %w", item.display, err)
		}
	}
	return nil
}

func checkOverwrite(display string, existing []byte, file fileSpec, force bool) error {
	if force {
		return nil
	}
	if file.owned {
		// Additive AST edits of known registration points.
		return nil
	}
	if file.relPath == resourcesTSRel && bytes.Contains(existing, []byte(GeneratedBanner)) {
		return nil
	}
	if bytes.Contains(existing, []byte(GeneratedBanner)) {
		return fmt.Errorf("resourcegen: refuse to overwrite %s without --force (generated file differs from this run)", display)
	}
	return fmt.Errorf("resourcegen: refuse to overwrite %s without --force (not generated by gombit)", display)
}

func readModulePath(workDir string) (string, error) {
	path := filepath.Join(workDir, "go.mod")
	// #nosec G304 -- go.mod inside the application work dir
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("resourcegen: read go.mod: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			module := modulePathFromGoModLine(line)
			if module == "" {
				break
			}
			return module, nil
		}
	}
	return "", errors.New("resourcegen: go.mod is missing a module path")
}

// modulePathFromGoModLine returns the module path from a `module …` line,
// stripping a trailing // comment and optional quotes the way go list -m does.
func modulePathFromGoModLine(line string) string {
	rest := strings.TrimSpace(strings.TrimPrefix(line, "module "))
	inQuote := false
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case '"':
			inQuote = !inQuote
		case '/':
			if !inQuote && i+1 < len(rest) && rest[i+1] == '/' {
				return strings.Trim(strings.TrimSpace(rest[:i]), `"`)
			}
		}
	}
	return strings.Trim(strings.TrimSpace(rest), `"`)
}

func readUI(workDir string) string {
	path := filepath.Join(workDir, "gombit.yaml")
	// #nosec G304 -- optional project file
	data, err := os.ReadFile(path)
	if err != nil {
		return defaultUI
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ui:") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(line, "ui:"))
		value = strings.Trim(value, `"'`)
		if value == "mui" {
			return "mui"
		}
		return defaultUI
	}
	return defaultUI
}

func checkHTTPPathConflict(workDir string, name ResourceName) error {
	dirs := []struct {
		root   string
		accept func(root, pkg string) bool
	}{
		{
			root: filepath.Join(workDir, "internal"),
			accept: func(root, pkg string) bool {
				_, err := os.Stat(filepath.Join(root, pkg, "routes.go"))
				return err == nil
			},
		},
		{
			root:   filepath.Join(workDir, "frontend", "src"),
			accept: hasGeneratedListPage,
		},
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir.root)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("resourcegen: read %s: %w", dir.root, err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			if !dir.accept(dir.root, entry.Name()) {
				continue
			}
			other, err := parseResourceName(entry.Name())
			if err != nil {
				continue
			}
			if other.Package == name.Package {
				continue
			}
			if other.HTTPPath == name.HTTPPath {
				return fmt.Errorf("resourcegen: resource name %q maps to HTTP path %q already used by %q", name.Input, name.HTTPPath, other.Package)
			}
		}
	}
	return nil
}

func collectFrontendResources(workDir string, current ResourceName) []ResourceName {
	result := []ResourceName{current}
	seen := map[string]struct{}{current.Package: {}}
	dir := filepath.Join(workDir, "frontend", "src")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return result
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, ok := seen[entry.Name()]; ok {
			continue
		}
		if !hasGeneratedListPage(dir, entry.Name()) {
			continue
		}
		parsed, err := parseResourceName(entry.Name())
		if err != nil {
			continue
		}
		result = append(result, parsed)
		seen[entry.Name()] = struct{}{}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Package < result[j].Package
	})
	return result
}

func hasGeneratedListPage(srcDir, pkg string) bool {
	for _, name := range []string{"list.tsx", "list.ts"} {
		if _, err := os.Stat(filepath.Join(srcDir, pkg, name)); err == nil {
			return true
		}
	}
	return false
}

func renderResourcesTS(resources []ResourceName) []byte {
	var b strings.Builder
	b.WriteString(tsBanner())
	b.WriteString("\n")
	b.WriteString("// React list + create-form pages. Types come from ./api/generated\n")
	b.WriteString("// (gombit client generate / gombit dev). OpenAPI path keys use /api/v1;\n")
	b.WriteString("// createAppClient rewrites them to the live GOMBIT_API_PREFIX.\n")
	b.WriteString("// Access tokens stay in memory; this file does not use web storage.\n\n")
	b.WriteString("import type { RouteObject } from \"react-router\";\n\n")
	for _, res := range resources {
		b.WriteString("import { ")
		b.WriteString(res.TypeName)
		b.WriteString("ListPage } from \"./")
		b.WriteString(res.Package)
		b.WriteString("/list\";\n")
		b.WriteString("import { ")
		b.WriteString(res.TypeName)
		b.WriteString("FormPage } from \"./")
		b.WriteString(res.Package)
		b.WriteString("/form\";\n")
	}
	b.WriteString("\nexport type GeneratedResource = {\n")
	b.WriteString("  slug: string;\n  title: string;\n")
	b.WriteString("  listPath: string;\n  createPath: string;\n")
	b.WriteString("};\n\n")
	b.WriteString("export const generatedResources: GeneratedResource[] = [\n")
	for _, res := range resources {
		b.WriteString("  {\n")
		b.WriteString("    slug: \"" + res.Package + "\",\n")
		b.WriteString("    title: \"" + res.TypeName + "\",\n")
		b.WriteString("    listPath: \"/" + res.Kebab + "\",\n")
		b.WriteString("    createPath: \"/" + res.Kebab + "/new\",\n")
		b.WriteString("  },\n")
	}
	b.WriteString("];\n\n")
	b.WriteString("export const generatedResourceRoutes: RouteObject[] = [\n")
	for _, res := range resources {
		b.WriteString("  { path: \"" + res.Kebab + "\", element: <" + res.TypeName + "ListPage /> },\n")
		b.WriteString("  { path: \"" + res.Kebab + "/new\", element: <" + res.TypeName + "FormPage /> },\n")
	}
	b.WriteString("];\n")
	return []byte(b.String())
}

func maybeMakeMigrations(ctx context.Context, opts Options, spec renderContext, models []migrations.Model) error {
	if opts.SkipMigrations {
		return skipMigrations(opts, spec, models, "--skip-migrations set")
	}
	if opts.skipAtlas {
		return printMakemigrationsHint(opts, spec, models, "skipped in tests")
	}
	// No "Atlas not on PATH" fallback: Plan already failed closed if Atlas was
	// missing, and a silent skip here is what made the committed tree depend on
	// PATH (#300). If Atlas vanished since Plan, makeMigrations errors loudly.
	driver := readDatabaseDriver(opts.WorkDir)
	err := makeMigrations(ctx, migrations.Options{
		WorkDir:      opts.WorkDir,
		Name:         "create_" + spec.Resource.PluralSnake,
		Driver:       driver,
		MigrationDir: "database/migrations",
		AtlasBinary:  opts.AtlasBin,
		Models:       models,
		Stdout:       opts.Stdout,
		Stderr:       opts.Stderr,
	})
	if err != nil {
		return fmt.Errorf("resourcegen: makemigrations: %w", err)
	}
	return nil
}

// skipMigrations persists the loader/registry state (models.json) without running
// the Atlas SQL diff, so the tree make resource commits is identical whether or
// not Atlas is installed — the deterministic half of #300. It is reached only via
// the explicit SkipMigrations opt-in; Plan otherwise fails closed when Atlas is
// absent.
func skipMigrations(opts Options, spec renderContext, models []migrations.Model, reason string) error {
	migrationDir := filepath.Join(opts.WorkDir, "database", "migrations")
	if err := os.MkdirAll(migrationDir, 0o750); err != nil {
		return fmt.Errorf("resourcegen: create migration dir: %w", err)
	}
	registered, err := migrations.LoadRegistry(migrationDir)
	if err != nil {
		return err
	}
	if err := migrations.SaveRegistry(migrationDir, migrations.MergeModels(registered, models)); err != nil {
		return err
	}
	return printMakemigrationsHint(opts, spec, models, reason)
}

func printMakemigrationsHint(opts Options, spec renderContext, models []migrations.Model, reason string) error {
	_, err := fmt.Fprintf(
		opts.Stdout,
		"note: %s; run: gombit db makemigrations create_%s%s\n",
		reason,
		spec.Resource.PluralSnake,
		modelFlagArgs(models),
	)
	return err
}

func modelFlagArgs(models []migrations.Model) string {
	var b strings.Builder
	for _, model := range models {
		b.WriteString(" --model ")
		b.WriteString(model.ImportPath)
		b.WriteString(".")
		b.WriteString(model.TypeName)
	}
	return b.String()
}

func ensureModel(models []migrations.Model, extra migrations.Model) []migrations.Model {
	for _, model := range models {
		if model.ImportPath == extra.ImportPath && model.TypeName == extra.TypeName {
			return models
		}
	}
	return append(models, extra)
}

func readDatabaseDriver(workDir string) config.DatabaseDriver {
	path := filepath.Join(workDir, "gombit.yaml")
	// #nosec G304 -- optional project file
	data, err := os.ReadFile(path)
	if err != nil {
		return config.DatabaseDriverSQLite
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "database:") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(line, "database:"))
		value = strings.Trim(value, `"'`)
		switch value {
		case "postgres":
			return config.DatabaseDriverPostgres
		case "mysql":
			return config.DatabaseDriverMySQL
		default:
			return config.DatabaseDriverSQLite
		}
	}
	return config.DatabaseDriverSQLite
}

func displayPath(workDir, full string) (string, error) {
	rel, err := filepath.Rel(workDir, full)
	if err != nil {
		return "", fmt.Errorf("resourcegen: relative path: %w", err)
	}
	return filepath.ToSlash(rel), nil
}

var lookPath = func(name string) (string, error) {
	return execLookPath(name)
}

var makeMigrations = migrations.MakeMigrations
