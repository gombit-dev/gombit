package client

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRewriteSpecPathsForTypedClient(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		paths     []string
		wantPaths []string
		wantSame  bool
	}{
		{
			name:      "already typed prefix",
			paths:     []string{"/api/v1/widgets", "/api/v1/widgets/{id}"},
			wantPaths: []string{"/api/v1/widgets", "/api/v1/widgets/{id}"},
			wantSame:  true,
		},
		{
			name:      "custom prefix one resource",
			paths:     []string{"/svc/v2/widgets", "/svc/v2/widgets/{id}"},
			wantPaths: []string{"/api/v1/widgets", "/api/v1/widgets/{id}"},
		},
		{
			name:      "api v2 with auth and resource",
			paths:     []string{"/api/v2/auth/login", "/api/v2/me", "/api/v2/products"},
			wantPaths: []string{"/api/v1/auth/login", "/api/v1/me", "/api/v1/products"},
		},
		{
			name:      "single detail path",
			paths:     []string{"/svc/v2/widgets/{id}"},
			wantPaths: []string{"/api/v1/widgets/{id}"},
		},
		{
			name:     "empty paths object",
			paths:    nil,
			wantSame: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			spec := specWithPaths(t, tt.paths)
			// No prefix configured: the generator is left to infer it, which is
			// what these cases pin. Callers that know the application's
			// configuration pass it; see the configured-prefix tests.
			got, err := rewriteSpecPathsForTypedClient(spec, "")
			if err != nil {
				t.Fatalf("rewriteSpecPathsForTypedClient() error = %v", err)
			}
			if tt.wantSame && string(got) != string(spec) {
				t.Fatal("identity rewrite must return the original spec bytes")
			}
			gotKeys := pathKeys(t, got)
			if tt.wantPaths == nil {
				if len(gotKeys) != 0 {
					t.Fatalf("path keys = %v, want empty", gotKeys)
				}
				return
			}
			if !sameStringSet(gotKeys, tt.wantPaths) {
				t.Fatalf("path keys = %v, want %v", gotKeys, tt.wantPaths)
			}
			for _, key := range gotKeys {
				if strings.HasPrefix(key, "/svc/v2") {
					t.Fatalf("rewritten spec still has live prefix key %q", key)
				}
			}
		})
	}
}

// An app whose every route shares one first segment is the case the path
// heuristic cannot tell apart: /svc/auth/login and /svc/auth/me are the routes
// of an app mounted at /svc with an auth group just as much as of an app
// mounted at /svc/auth. The configuration says which, so the prefix it gives
// is what the keys are rewritten from (#506).
func TestRewriteSpecPathsForTypedClientUsesConfiguredPrefix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		live      string
		paths     []string
		wantPaths []string
	}{
		{
			name:      "prefix one segment",
			live:      "/svc",
			paths:     []string{"/svc/auth/login", "/svc/auth/refresh", "/svc/auth/me"},
			wantPaths: []string{"/api/v1/auth/login", "/api/v1/auth/refresh", "/api/v1/auth/me"},
		},
		{
			name:      "written without the leading slash",
			live:      "svc",
			paths:     []string{"/svc/auth/login"},
			wantPaths: []string{"/api/v1/auth/login"},
		},
		{
			name:      "written with the trailing slash",
			live:      "/svc/",
			paths:     []string{"/svc/auth/login"},
			wantPaths: []string{"/api/v1/auth/login"},
		},
		{
			name:      "mounted deeper than the heuristic would stop",
			live:      "/api/v2",
			paths:     []string{"/api/v2/auth/login", "/api/v2/auth/refresh"},
			wantPaths: []string{"/api/v1/auth/login", "/api/v1/auth/refresh"},
		},
		{
			name:      "routes outside the prefix keep their keys",
			live:      "/svc",
			paths:     []string{"/svc/widgets", "/healthz"},
			wantPaths: []string{"/api/v1/widgets", "/healthz"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := rewriteSpecPathsForTypedClient(specWithPaths(t, tt.paths), tt.live)
			if err != nil {
				t.Fatalf("rewriteSpecPathsForTypedClient(live=%q) error = %v", tt.live, err)
			}
			if keys := pathKeys(t, got); !sameStringSet(keys, tt.wantPaths) {
				t.Fatalf("path keys = %v, want %v", keys, tt.wantPaths)
			}
		})
	}
}

// A configured prefix the document does not use is two different applications
// in one command, and rewriting nothing would hand out a client whose keys
// match no route of either (#506).
func TestRewriteSpecPathsForTypedClientRejectsAPrefixWithNoPaths(t *testing.T) {
	t.Parallel()

	spec := specWithPaths(t, []string{"/svc/widgets", "/svc/widgets/{id}"})
	_, err := rewriteSpecPathsForTypedClient(spec, "/other")
	if err == nil {
		t.Fatal("rewriteSpecPathsForTypedClient(live=/other) error = nil, want the prefix reported")
	}
	for _, want := range []string{"/other", "matches no path"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to mention %q", err, want)
		}
	}
}

// A document that already uses the typed keys needs no rewriting whatever the
// caller is configured with, so a prefix that matches none of its paths is not
// an error there: the keys are the ones the generated client wants.
func TestRewriteSpecPathsForTypedClientKeepsATypedDocumentUntouched(t *testing.T) {
	t.Parallel()

	spec := specWithPaths(t, []string{"/api/v1/widgets"})
	for _, live := range []string{"", "/api/v1", "/svc"} {
		got, err := rewriteSpecPathsForTypedClient(spec, live)
		if err != nil {
			t.Fatalf("rewriteSpecPathsForTypedClient(live=%q) error = %v", live, err)
		}
		if string(got) != string(spec) {
			t.Fatalf("live=%q rewrote an already typed document: %s", live, got)
		}
	}
}

// The rewrite is a JSON round trip, so a number it does not touch is only
// safe if it survives as written: an int64 bound decodes through float64 to
// 9223372036854776000, changing the contract the generated types describe
// (#506). It only ever happens on the rewritten path, since the untouched
// document is returned as its own bytes.
func TestRewriteSpecPathsForTypedClientPreservesNumbersAsWritten(t *testing.T) {
	t.Parallel()

	spec := []byte(`{"openapi":"3.1.0","paths":{"/svc/auth/login":{}},"x":{"maximum":9223372036854775807,"minimum":-9223372036854775808,"ratio":1.0000000000000000001,"exp":1e3}}`)
	got, err := rewriteSpecPathsForTypedClient(spec, "/svc")
	if err != nil {
		t.Fatalf("rewriteSpecPathsForTypedClient() error = %v", err)
	}
	for _, want := range []string{
		`"maximum":9223372036854775807`,
		`"minimum":-9223372036854775808`,
		`"ratio":1.0000000000000000001`,
		`"exp":1e3`,
	} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("rewritten spec = %s, want %s kept as written", got, want)
		}
	}
}

func TestInferLiveAPIPrefix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		paths []string
		want  string
	}{
		{name: "typed", paths: []string{"/api/v1/widgets", "/api/v1/me"}, want: "/api/v1"},
		{name: "svc v2", paths: []string{"/svc/v2/widgets", "/svc/v2/widgets/{id}"}, want: "/svc/v2"},
		{name: "single detail", paths: []string{"/svc/v2/widgets/{id}"}, want: "/svc/v2"},
		{name: "auth only", paths: []string{"/api/v2/auth/login", "/api/v2/auth/refresh"}, want: "/api/v2/auth"},
		{name: "auth and resource", paths: []string{"/api/v2/auth/login", "/api/v2/me", "/api/v2/products"}, want: "/api/v2"},
		// Pinned as the guess it is: these paths are also those of an app
		// mounted at /svc with an auth group, and nothing in the document says
		// which. Only the configured prefix does (#506).
		{name: "one shared first segment", paths: []string{"/svc/auth/login", "/svc/auth/refresh", "/svc/auth/me"}, want: "/svc/auth"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := inferLiveAPIPrefix(tt.paths); got != tt.want {
				t.Fatalf("inferLiveAPIPrefix(%v) = %q, want %q", tt.paths, got, tt.want)
			}
		})
	}
}

func specWithPaths(t *testing.T, paths []string) []byte {
	t.Helper()
	doc := map[string]any{
		"openapi": "3.1.0",
		"info":    map[string]any{"title": "t", "version": "0"},
		"paths":   map[string]any{},
	}
	pathMap := doc["paths"].(map[string]any)
	for _, path := range paths {
		pathMap[path] = map[string]any{"get": map[string]any{"operationId": "op"}}
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

func pathKeys(t *testing.T, spec []byte) []string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(spec, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	raw, _ := doc["paths"].(map[string]any)
	keys := make([]string, 0, len(raw))
	for path := range raw {
		keys = append(keys, path)
	}
	return keys
}

func sameStringSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]int, len(want))
	for _, w := range want {
		seen[w]++
	}
	for _, g := range got {
		seen[g]--
		if seen[g] < 0 {
			return false
		}
	}
	return true
}
