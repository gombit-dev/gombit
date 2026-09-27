//go:build chaos

package chaos

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestReplayIsDeterministic: a scenario's randomness is a pure function of
// (seed, scenario, iteration): the same three give the same draws, and a
// different one gives different draws.
func TestReplayIsDeterministic(t *testing.T) {
	draw := func(seed int64, name string, it int) []uint64 {
		r := rngFor(seed, name, it)
		out := make([]uint64, 16)
		for i := range out {
			out[i] = r.Uint64()
		}
		return out
	}
	same := func(a, b []uint64) bool {
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}
	base := draw(928471923, "database/failure-boundary", 147)
	if !same(base, draw(928471923, "database/failure-boundary", 147)) {
		t.Fatal("the same seed, scenario, and iteration drew differently: replay is broken")
	}
	for name, other := range map[string][]uint64{
		"another seed":      draw(928471924, "database/failure-boundary", 147),
		"another scenario":  draw(928471923, "database/concurrent-writers", 147),
		"another iteration": draw(928471923, "database/failure-boundary", 148),
	} {
		if same(base, other) {
			t.Errorf("%s drew the same values: iterations are not independent", name)
		}
	}
}

// TestScenariosDrawOnlyFromEnvRand: a scenario that takes randomness from
// anywhere but env.Rand (math/rand's globals, crypto/rand, the clock as a
// seed) cannot be replayed from its seed, so no scenario file may import a
// random source.
func TestScenariosDrawOnlyFromEnvRand(t *testing.T) {
	files, err := filepath.Glob("scenario*_test.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no scenario files found (%v)", err)
	}
	for _, file := range files {
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if strings.HasSuffix(path, "/rand") || strings.HasPrefix(path, "math/rand") {
				t.Errorf("%s imports %s: scenarios must draw only from env.Rand, or a seed cannot replay them", file, path)
			}
		}
	}
}
