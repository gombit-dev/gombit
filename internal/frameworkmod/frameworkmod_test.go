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
		"local replace": {require + "replace github.com/gombit-dev/gombit => ../gombit\n", Declaration{Required: "v0.8.2", Replace: &Replacement{Path: "../gombit"}}},
		"fork replace":  {require + "replace github.com/gombit-dev/gombit => github.com/me/gombit v0.8.3-fork\n", Declaration{Required: "v0.8.2", Replace: &Replacement{Path: "github.com/me/gombit", Version: "v0.8.3-fork"}}},
		// The go command applies a replace of the required version only, and
		// prefers it to a replace of every version, in either order.
		"replace of another version": {require + "replace github.com/gombit-dev/gombit v0.7.0 => github.com/fork/gombit v1.2.3\n", Declaration{Required: "v0.8.2"}},
		"exact over wildcard, wildcard first": {
			require + "replace github.com/gombit-dev/gombit => ../wildcard\nreplace github.com/gombit-dev/gombit v0.8.2 => github.com/fork/gombit v1.2.3\n",
			Declaration{Required: "v0.8.2", Replace: &Replacement{Path: "github.com/fork/gombit", Version: "v1.2.3", ForVersion: "v0.8.2"}},
		},
		"exact over wildcard, exact first": {
			require + "replace (\n\tgithub.com/gombit-dev/gombit v0.8.2 => ../exact\n\tgithub.com/gombit-dev/gombit => ../wildcard\n)\n",
			Declaration{Required: "v0.8.2", Replace: &Replacement{Path: "../exact", ForVersion: "v0.8.2"}},
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
		"framework replace": {Declaration{Required: "v0.8.2", Replace: &Replacement{Path: ModulePath, Version: "v0.9.0"}}, "v0.9.0"},
		"fork":              {Declaration{Required: "v0.8.2", Replace: &Replacement{Path: "github.com/fork/gombit", Version: "v1.2.3"}}, ""},
		"local":             {Declaration{Required: "v0.8.2", Replace: &Replacement{Path: "../gombit"}}, ""},
	} {
		if got := tc.d.Release(); got != tc.want {
			t.Errorf("%s: Release = %q, want %q", name, got, tc.want)
		}
	}
}

// TestParseKeepsEveryReplace: Replaces holds every framework replace,
// applying or not (an exact repeat once), and Resolve picks the one that
// applies to a version as the go command does.
func TestParseKeepsEveryReplace(t *testing.T) {
	d, err := Parse("go.mod", []byte(require+
		"replace github.com/gombit-dev/gombit v0.9.0 => ../next\n"+
		"replace github.com/gombit-dev/gombit => ../every\n"+
		"replace github.com/gombit-dev/gombit v0.8.2 => github.com/gombit-dev/gombit v0.8.3\n"+
		"replace github.com/gombit-dev/gombit => ../every\n"+
		"replace github.com/other/mod => ../other\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []Replacement{
		{Path: "../next", ForVersion: "v0.9.0"},
		{Path: "../every"},
		{Path: ModulePath, Version: "v0.8.3", ForVersion: "v0.8.2"},
	}
	if len(d.Replaces) != len(want) {
		t.Fatalf("Replaces = %+v; want %+v", d.Replaces, want)
	}
	for i := range want {
		if d.Replaces[i] != want[i] {
			t.Fatalf("Replaces[%d] = %+v; want %+v", i, d.Replaces[i], want[i])
		}
	}
	if d.Replace == nil || *d.Replace != want[2] {
		t.Fatalf("Replace = %+v; want the exact v0.8.2 one", d.Replace)
	}
	for version, wantPath := range map[string]string{"v0.9.0": "../next", "v0.8.2": ModulePath, "v0.7.0": "../every"} {
		if r := Resolve(d.Replaces, version); r == nil || r.Path != wantPath {
			t.Errorf("Resolve(%s) = %+v; want %s", version, r, wantPath)
		}
	}
	if r := Resolve(want[:1], "v0.7.0"); r != nil {
		t.Errorf("Resolve with only an exact replace of another version = %+v; want none", r)
	}
}
