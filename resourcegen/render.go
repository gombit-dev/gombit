package resourcegen

import "fmt"

// This file exposes the model-first generation primitive that both gombit
// generate (regenerate / --check) and, later, make resource (bootstrap) call, so
// the two never derive a resource's files two different ways — the single
// authoritative derivation ADR-016 requires (issue #352). It is the only exported
// entry point into the slice 3/4 emitters: a Program-Mode loader (which runs
// inside the application module, importing the app's real compiled models) calls
// RenderResource on each model and emits the resulting artifacts to the parent.

// ResourceMarkerFile marks a feature package as a model-first CRUD resource.
// make resource writes it at bootstrap (slice 5b); gombit generate discovers
// resources by its presence, never by treating every persisted AutoMigrate model
// as a resource — "AutoMigrate" means "this model is persisted", not "expose a
// generated API for it" (a join table, an audit-log or token model is persisted
// but not a resource). It is a durable sentinel, deliberately NOT a generated
// *.gen.go: a *.gen.go going missing must read as drift, not make the resource
// itself vanish from discovery.
const ResourceMarkerFile = ".gombit-resource"

// Ownership classifies who owns a generated file, which decides how gombit
// generate treats it.
type Ownership int

const (
	// GeneratorOwned files are the derived *.gen.go (DTOs, mappers, handler): the
	// generator rewrites them every run and gombit generate --check compares them.
	// They carry the DO-NOT-EDIT banner.
	GeneratorOwned Ownership = iota
	// SeedOnce files are human-owned (the hooks file): the generator writes one
	// only when it is absent, and never overwrites or drift-checks it afterward —
	// it is the customization surface, so editing it must be safe.
	SeedOnce
)

// GeneratedArtifact is one file the model-first generator produces for a
// resource: a path relative to the application root, its rendered bytes, and its
// ownership class. It is the unit the Program-Mode loader emits (as JSON) and the
// generate command plans its writes / drift check over. The loader has no write
// access to the app tree; producing artifacts is all it does.
type GeneratedArtifact struct {
	Path      string    `json:"path"`
	Content   []byte    `json:"content"`
	Ownership Ownership `json:"ownership"`
}

// RenderResource renders every model-first file for one resource from its parsed
// model and declared policy: the DTOs + mappers and the CRUD handler (both
// generator-owned .gen.go), and the default hooks file (seed-once, human-owned).
// pkg is the feature package the files live in — the generated file paths are
// internal/<pkg>/…, the gombit feature-package layout. The output is
// deterministic (slices 3/4), so a regenerate-and-compare drift check is sound.
func RenderResource(model any, pkg string) ([]GeneratedArtifact, error) {
	res, err := buildModelResource(model, pkg)
	if err != nil {
		return nil, err
	}
	handler, err := renderModelHandler(res)
	if err != nil {
		return nil, err
	}
	dir := "internal/" + pkg
	return []GeneratedArtifact{
		{Path: dir + "/dto.gen.go", Content: mustFormatGo(renderModelDTOs(res)), Ownership: GeneratorOwned},
		{Path: dir + "/handler.gen.go", Content: mustFormatGo(handler), Ownership: GeneratorOwned},
		{Path: dir + "/hooks.go", Content: mustFormatGo(renderModelHooks(res)), Ownership: SeedOnce},
	}, nil
}

// String names the ownership class for diagnostics.
func (o Ownership) String() string {
	switch o {
	case GeneratorOwned:
		return "generator-owned"
	case SeedOnce:
		return "seed-once"
	default:
		return fmt.Sprintf("Ownership(%d)", int(o))
	}
}

// ReadModulePath returns the module path from the go.mod in workDir. It is the
// exported form of the reader make resource already uses, so gombit generate
// resolves the app's module the same way.
func ReadModulePath(workDir string) (string, error) {
	return readModulePath(workDir)
}

// ValidateAppLayout reports whether workDir looks like a gombit application
// (go.mod + cmd/server/main.go + internal/platform/database.go), the same check
// make resource applies before touching an app.
func ValidateAppLayout(workDir string) error {
	return Options{WorkDir: workDir}.validateAppLayout()
}
