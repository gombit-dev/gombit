package contract

import "github.com/danielgtaylor/huma/v2"

const (
	// OpenAPIPath is the Huma OpenAPI prefix. Clients fetch /openapi.json.
	OpenAPIPath = "/openapi"
	// DocsPath is the FastAPI-style interactive docs UI.
	DocsPath = "/docs"
)

// HumaConfig returns the default Huma configuration for a Gombit app.
//
// Interactive docs are enabled at /docs (Swagger UI). The schema browser stays
// disabled. Pass docsEnabled=false to omit the docs route (production default).
func HumaConfig(title, version string) huma.Config {
	return HumaConfigFor(title, version, true)
}

// HumaConfigFor returns Huma configuration with docs explicitly enabled or not.
func HumaConfigFor(title, version string, docsEnabled bool) huma.Config {
	if title == "" {
		title = "Gombit"
	}
	if version == "" {
		version = "0.0.0"
	}
	config := huma.DefaultConfig(title, version)
	config.OpenAPIPath = OpenAPIPath
	config.SchemasPath = ""
	// Drop Huma's SchemaLinkTransformer. Its create hook injects a `$schema`
	// URL property into every response body (and into request input schemas,
	// so it leaks into the generated TS request types). The D10 envelope is
	// exactly {data, meta?} / {error}; a `$schema` key is off-contract. Clearing
	// CreateHooks is the supported way to opt out — it is the only default hook
	// and setting SchemasPath="" alone does not remove it. See #225.
	config.CreateHooks = nil
	// Encode before writing, so a response that cannot be encoded becomes a
	// D10 500 instead of a 200 with a plain-text body (#442). framework.New
	// replaces this with a format that also logs the failure.
	config.Formats = JSONFormats(nil)
	if docsEnabled {
		config.DocsPath = DocsPath
		config.DocsRenderer = huma.DocsRendererSwaggerUI
		return config
	}
	config.DocsPath = ""
	return config
}
