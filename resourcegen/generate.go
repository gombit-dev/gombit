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

// Generate writes a feature-package resource into an existing Gombit app.
func Generate(ctx context.Context, opts Options) error {
	if ctx == nil {
		return errors.New("resourcegen: nil context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := opts.normalize(); err != nil {
		return err
	}
	if err := opts.validateAppLayout(); err != nil {
		return err
	}

	name, err := parseResourceName(opts.Name)
	if err != nil {
		return err
	}
	if err := checkHTTPPathConflict(opts.WorkDir, name); err != nil {
		return err
	}
	fields, err := parseFields(opts.Fields, name.Package)
	if err != nil {
		return err
	}
	module, err := readModulePath(opts.WorkDir)
	if err != nil {
		return err
	}
	ui := readUI(opts.WorkDir)
	// OpenAPI path keys stay /api/v1 (placeholder client / D8). The generated
	// SPA rewrites that prefix to the live GOMBIT_API_PREFIX at request time,
	// so make-resource pages must not bake gombit.yaml api_prefix.
	ctxData := newRenderContext(module, name, fields, defaultAPIPrefix, ui, opts.Service, opts.Repo)

	files, err := renderFeatureFiles(ctxData)
	if err != nil {
		return err
	}
	for i := range files {
		if strings.HasSuffix(files[i].relPath, ".go") {
			formatted, fmtErr := format.Source(files[i].content)
			if fmtErr != nil {
				return fmt.Errorf("resourcegen: format %s: %w", files[i].relPath, fmtErr)
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
		return fmt.Errorf("resourcegen: read %s: %w", serverMainRel, err)
	}
	// #nosec G304 -- application files under the user work dir
	platformSrc, err := os.ReadFile(platformPath)
	if err != nil {
		return fmt.Errorf("resourcegen: read %s: %w", platformDBRel, err)
	}
	newMain, err := AddImportAndRegister(mainSrc, ctxData.ImportPath, name.Package)
	if err != nil {
		return err
	}
	newPlatform, err := AddAutoMigrateModel(platformSrc, ctxData.ImportPath, name.Package, name.TypeName)
	if err != nil {
		return err
	}
	models, err := CollectAutoMigrateModels(newPlatform)
	if err != nil {
		return err
	}
	models = ensureModel(models, migrations.Model{ImportPath: ctxData.ImportPath, TypeName: name.TypeName})
	files = append(files,
		fileSpec{relPath: serverMainRel, content: newMain, owned: true},
		fileSpec{relPath: platformDBRel, content: newPlatform, owned: true},
	)

	sort.Slice(files, func(i, j int) bool {
		return files[i].relPath < files[j].relPath
	})

	planned, err := planWrites(opts, files)
	if err != nil {
		return err
	}

	for _, item := range planned {
		if _, err := fmt.Fprintf(opts.Stdout, "%s %s\n", item.action, item.display); err != nil {
			return err
		}
		if opts.DryRun || !item.write {
			continue
		}
		full := filepath.Join(opts.WorkDir, filepath.FromSlash(item.relPath))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			return fmt.Errorf("resourcegen: mkdir %s: %w", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, item.content, 0o644); err != nil { //nolint:gosec // generated source is a non-secret artifact
			return fmt.Errorf("resourcegen: write %s: %w", item.display, err)
		}
	}

	if opts.DryRun {
		return nil
	}
	if _, err := fmt.Fprintf(opts.Stdout, "GORM model is Atlas-loader ready: %s\n", ctxData.ModelSpec); err != nil {
		return err
	}
	return maybeMakeMigrations(ctx, opts, ctxData, models)
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
	if opts.skipAtlas {
		return printMakemigrationsHint(opts, spec, models, "skipped in tests")
	}
	atlasPath, lookErr := lookPath(opts.AtlasBin)
	if lookErr != nil || atlasPath == "" {
		return printMakemigrationsHint(opts, spec, models, "atlas not on PATH")
	}

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
