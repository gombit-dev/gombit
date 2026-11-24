//go:build !integration

package resourcegen

import (
	"regexp"
	"strings"
	"testing"
)

// TestGeneratedPagesStayTyped checks that the generated form and list pages do
// not erase the client types. A Record<string, unknown> body or a row cast to
// an all-optional literal type accepts any field name, so renaming a model
// field would pass tsc and fail only at runtime.
func TestGeneratedPagesStayTyped(t *testing.T) {
	fields, err := parseFields([]string{"title:string:required", "meta:json"}, "task")
	if err != nil {
		t.Fatal(err)
	}
	name, err := parseResourceName("Task")
	if err != nil {
		t.Fatal(err)
	}
	for _, ui := range []string{"minimal", "mui"} {
		ctx := newRenderContext("github.com/example/demo", name, fields, "/api/v1", ui, false, false)
		form := renderFormTSX(ctx)
		if strings.Contains(form, "Record<string, unknown>") {
			t.Errorf("%s form body is cast to Record<string, unknown>; a renamed field is not a compile error:\n%s", ui, form)
		}
		if !strings.Contains(form, "as Record<keyof CreateBody, unknown>") {
			t.Errorf("%s form body is not keyed by CreateBody:\n%s", ui, form)
		}
		for _, pat := range erasingCasts {
			if pat.MatchString(form) {
				t.Errorf("%s form erases the client types (%s); a renamed field is not a compile error:\n%s", ui, pat, form)
			}
		}
		list := renderListTSX(ctx)
		if strings.Contains(list, "row as {") {
			t.Errorf("%s list row is cast to an optional literal type; a renamed field is not a compile error:\n%s", ui, list)
		}
		for _, pat := range erasingCasts {
			if pat.MatchString(list) {
				t.Errorf("%s list erases the client types (%s); a renamed field is not a compile error:\n%s", ui, pat, list)
			}
		}
		if !strings.Contains(list, "const record: ListRow = row;") {
			t.Errorf("%s list page does not type its row as ListRow:\n%s", ui, list)
		}
	}
}

// erasingCasts are the spellings that make tsc accept any field name. A
// generated page must not contain them.
var erasingCasts = []*regexp.Regexp{
	regexp.MustCompile(`\bas any\b`),
	regexp.MustCompile(`:\s*any\b`),
	regexp.MustCompile(`as unknown as`),
	regexp.MustCompile(`Record<string,\s*any>`),
	regexp.MustCompile(`Partial<CreateBody>`),
	regexp.MustCompile(`as Partial<`),
}
