package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// TypedAPIPrefix is D8 / DEFAULT_API_PREFIX. Scaffolded pages and the
// placeholder OpenAPI client use these path keys. gombit client generate
// rewrites live Huma paths to this prefix before openapi-typescript so
// `client.GET("/api/v1/...")` keeps typechecking after GOMBIT_API_PREFIX
// changes. rewriteAPIRequest maps them back to the live prefix on the wire.
const TypedAPIPrefix = "/api/v1"

// rewriteSpecPathsForTypedClient copies spec and rewrites OpenAPI path keys
// from the live API prefix to TypedAPIPrefix. live is the prefix the document
// was served under, which is configuration (GOMBIT_API_PREFIX): an empty live
// means the caller does not know it, so the prefix is inferred from the paths
// and can only be a guess (see inferLiveAPIPrefix).
//
// The caller's document on disk is not modified (examples/client contract-drift
// still compares live Huma paths). A spec that already uses /api/v1 is returned
// unchanged, whatever the caller is configured with: its keys are the ones the
// typed client wants. A configured prefix that matches no path in a spec that
// does need rewriting is an error, because the alternative is a generated
// client whose keys belong to no route of this app (#506).
func rewriteSpecPathsForTypedClient(spec []byte, live string) ([]byte, error) {
	var doc map[string]any
	if err := decodeSpecDocument(spec, &doc); err != nil {
		return nil, fmt.Errorf("client: parse spec for path rewrite: %w", err)
	}
	rawPaths, ok := doc["paths"].(map[string]any)
	if !ok || len(rawPaths) == 0 {
		return spec, nil
	}
	keys := make([]string, 0, len(rawPaths))
	for path := range rawPaths {
		keys = append(keys, path)
	}
	if allTypedAPIPrefix(keys) {
		return spec, nil
	}
	live = normalizeLivePrefix(live)
	if live == "" {
		guessed := inferLiveAPIPrefix(keys)
		if guessed == TypedAPIPrefix || !anyPathUnderPrefix(guessed, keys) {
			return spec, nil
		}
		live = guessed
	} else if !anyPathUnderPrefix(live, keys) {
		return nil, fmt.Errorf(
			"client: live API prefix %q matches no path of the OpenAPI document: generate against the "+
				"document of the app this prefix is set for, or pass the document's own prefix with --api-prefix",
			live)
	}
	if live == TypedAPIPrefix {
		return spec, nil
	}

	rewritten := make(map[string]any, len(rawPaths))
	for path, value := range rawPaths {
		newPath := path
		if path == live || strings.HasPrefix(path, live+"/") {
			newPath = TypedAPIPrefix + path[len(live):]
		}
		rewritten[newPath] = value
	}
	doc["paths"] = rewritten
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("client: marshal rewritten spec: %w", err)
	}
	return out, nil
}

// decodeSpecDocument decodes a document into a generic map keeping every
// number as the text it was written with. Re-encoding a json.Number is
// byte-for-byte the input; a float64 is not, and an OpenAPI integer bound
// above 2^53 (an int64 `maximum`, say) comes back altered:
// 9223372036854775807 becomes 9223372036854776000 (#506).
func decodeSpecDocument(spec []byte, doc *map[string]any) error {
	decoder := json.NewDecoder(bytes.NewReader(spec))
	decoder.UseNumber()
	return decoder.Decode(doc)
}

// normalizeLivePrefix turns a configured prefix into the path form the spec's
// keys are written in: `/svc/v2/`, `/svc/v2` and `svc/v2` all mean `/svc/v2`.
// An empty result means "not configured" or "mounted at the root", and neither
// is a prefix any key can start with.
func normalizeLivePrefix(prefix string) string {
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if prefix == "" {
		return ""
	}
	return "/" + prefix
}

func anyPathUnderPrefix(prefix string, paths []string) bool {
	if prefix == "" {
		return false
	}
	for _, path := range paths {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

// inferLiveAPIPrefix returns the Huma mount prefix shared by every path.
// Longest common string prefix is walked back to a slash boundary until no
// operation path equals the candidate and every remainder starts with a
// non-parameter segment (so /svc/v2/widgets + /svc/v2/widgets/{id} yields
// /svc/v2, not /svc/v2/widgets).
//
// It is a guess, and no rule can make it more: the paths of an app mounted at
// /svc whose first resource group is auth (/svc/auth/login, /svc/auth/me) are
// the paths of an app mounted at /svc/auth with a login resource. Only the
// application's own configuration tells the two apart, which is why callers
// pass a prefix (see rewriteSpecPathsForTypedClient) and this is the fallback.
func inferLiveAPIPrefix(paths []string) string {
	if len(paths) == 0 {
		return TypedAPIPrefix
	}
	if allTypedAPIPrefix(paths) {
		return TypedAPIPrefix
	}
	candidate := paths[0]
	for _, path := range paths[1:] {
		candidate = commonStringPrefix(candidate, path)
	}
	for {
		candidate = strings.TrimSuffix(candidate, "/")
		if candidate == "" || candidate == "/" {
			return TypedAPIPrefix
		}
		if isValidAPIPrefix(candidate, paths) {
			return candidate
		}
		slash := strings.LastIndex(candidate, "/")
		if slash <= 0 {
			return TypedAPIPrefix
		}
		candidate = candidate[:slash]
	}
}

func allTypedAPIPrefix(paths []string) bool {
	for _, path := range paths {
		if path != TypedAPIPrefix && !strings.HasPrefix(path, TypedAPIPrefix+"/") {
			return false
		}
	}
	return true
}

func isValidAPIPrefix(prefix string, paths []string) bool {
	if prefix == "" || strings.Contains(prefix, "{") {
		return false
	}
	for _, path := range paths {
		if path == prefix {
			return false
		}
		if !strings.HasPrefix(path, prefix+"/") {
			return false
		}
		rest := strings.TrimPrefix(path, prefix+"/")
		seg, _, _ := strings.Cut(rest, "/")
		if seg == "" || strings.HasPrefix(seg, "{") {
			return false
		}
	}
	return true
}

func commonStringPrefix(a, b string) string {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return a[:i]
}
