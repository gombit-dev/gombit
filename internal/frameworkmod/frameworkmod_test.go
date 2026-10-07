package frameworkmod

import (
	"errors"
	"strings"
	"testing"
)

const require = "module x\n\ngo 1.26\n\nrequire github.com/gombit-dev/gombit v0.8.2\n"

func TestParse(t *testing.T) {
	for name, tc := range map[string]struct {
		gomod string
		want  Declaration
	}{
		"require only":  {require, Declaration{Required: "v0.8.2"}},
		"local replace": {require + "replace github.com/gombit-dev/gombit => ../gombit\n", Declaration{"v0.8.2", &Replacement{Path: "../gombit"}}},
		"fork replace":  {require + "replace github.com/gombit-dev/gombit => github.com/me/gombit v0.8.3-fork\n", Declaration{"v0.8.2", &Replacement{"github.com/me/gombit", "v0.8.3-fork"}}},
		// The go command applies a replace of the required version only, and
		// prefers it to a replace of every version, in either order.
		"replace of another version": {require + "replace github.com/gombit-dev/gombit v0.7.0 => github.com/fork/gombit v1.2.3\n", Declaration{Required: "v0.8.2"}},
		"exact over wildcard, wildcard first": {
			require + "replace github.com/gombit-dev/gombit => ../wildcard\nreplace github.com/gombit-dev/gombit v0.8.2 => github.com/fork/gombit v1.2.3\n",
			Declaration{"v0.8.2", &Replacement{"github.com/fork/gombit", "v1.2.3"}},
		},
		"exact over wildcard, exact first": {
			require + "replace (\n\tgithub.com/gombit-dev/gombit v0.8.2 => ../exact\n\tgithub.com/gombit-dev/gombit => ../wildcard\n)\n",
			Declaration{"v0.8.2", &Replacement{Path: "../exact"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Parse("go.mod", []byte(tc.gomod))
			if err != nil {
				t.Fatal(err)
			}
			if got.Required != tc.want.Required || (got.Replace == nil) != (tc.want.Replace == nil) || (got.Replace != nil && *got.Replace != *tc.want.Replace) {
				t.Fatalf("Parse = %+v (replace %+v), want %+v (replace %+v)", got, got.Replace, tc.want, tc.want.Replace)
			}
		})
	}
	if _, err := Parse("go.mod", []byte("module x\n\ngo 1.26\n")); !errors.Is(err, ErrNotRequired) {
		t.Fatalf("Parse without the framework = %v, want ErrNotRequired", err)
	}
	if _, err := Parse("go.mod", []byte("module\n")); err == nil {
		t.Fatal("Parse of a malformed go.mod succeeded")
	}
}

// TestParseRefusesWhatGoRefuses: a go.mod the go command rejects is not
// given a confident answer.
func TestParseRefusesWhatGoRefuses(t *testing.T) {
	for name, tc := range map[string]struct{ gomod, msg string }{
		"required twice": {
			"module x\n\ngo 1.26\n\nrequire github.com/gombit-dev/gombit v0.9.0\nrequire github.com/gombit-dev/gombit v0.8.2\n",
			"required twice (v0.9.0 and v0.8.2)",
		},
		"exact replaces conflict": {
			require + "replace github.com/gombit-dev/gombit v0.8.2 => github.com/gombit-dev/gombit v0.9.0\nreplace github.com/gombit-dev/gombit v0.8.2 => ../x\n",
			"conflicting replacements for github.com/gombit-dev/gombit@v0.8.2",
		},
		"wildcard replaces conflict": {
			require + "replace github.com/gombit-dev/gombit => github.com/gombit-dev/gombit v0.9.0\nreplace github.com/gombit-dev/gombit => ../x\n",
			"conflicting replacements for github.com/gombit-dev/gombit (",
		},
		// Go checks every replace, not just the one that applies.
		"conflict on another version": {
			require + "replace github.com/gombit-dev/gombit v0.7.0 => ../a\nreplace github.com/gombit-dev/gombit v0.7.0 => ../b\n",
			"conflicting replacements for github.com/gombit-dev/gombit@v0.7.0",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse("go.mod", []byte(tc.gomod)); err == nil || !strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("Parse = %v; want an error mentioning %q", err, tc.msg)
			}
		})
	}
	// The same replace twice is no conflict.
	same := require + "replace github.com/gombit-dev/gombit => ../x\nreplace github.com/gombit-dev/gombit => ../x\n"
	if d, err := Parse("go.mod", []byte(same)); err != nil || d.Replace == nil || d.Replace.Path != "../x" {
		t.Fatalf("Parse with a repeated identical replace = %+v, %v", d, err)
	}
}

func TestRelease(t *testing.T) {
	for name, tc := range map[string]struct {
		d    Declaration
		want string
	}{
		"required":          {Declaration{Required: "v0.8.2"}, "v0.8.2"},
		"framework replace": {Declaration{"v0.8.2", &Replacement{ModulePath, "v0.9.0"}}, "v0.9.0"},
		"fork":              {Declaration{"v0.8.2", &Replacement{"github.com/fork/gombit", "v1.2.3"}}, ""},
		"local":             {Declaration{"v0.8.2", &Replacement{Path: "../gombit"}}, ""},
	} {
		if got := tc.d.Release(); got != tc.want {
			t.Errorf("%s: Release = %q, want %q", name, got, tc.want)
		}
	}
}
