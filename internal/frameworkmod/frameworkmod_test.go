package frameworkmod

import (
	"errors"
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
