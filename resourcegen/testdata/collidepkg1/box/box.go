// Package box is a fixture for the resourcegen modelgen import tests
// (issue #352 review, finding 4): a real, importable package whose basename
// ("box") collides with resourcegen/testdata/collidepkg2/box at a different
// import path. typeRenderer.aliasFor must uniquify the two rather than let the
// second field silently reuse the first import's alias.
package box

// Box is a defined string type: named, so it is rendered by identity and
// requires an import, not decomposed into "string".
type Box string
