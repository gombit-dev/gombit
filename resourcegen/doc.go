// Package resourcegen implements `gombit make resource`.
//
// It scaffolds a model-first feature-package (ADR-016): the human-owned GORM
// model and the .gombit-resource marker (optional service.go / repo.go), React
// list/form pages that import generated OpenAPI client types, and the
// registration in cmd/server/main.go and internal/platform/database.go's
// AutoMigrate call via go/ast. RenderResource derives the generator-owned
// dto.gen.go / handler.gen.go (thin Huma handler) and the seed-once hooks.go
// from the compiled model; `gombit generate` drives it. Generators are
// idempotent and additive; Go source is never patched with regular
// expressions.
package resourcegen
