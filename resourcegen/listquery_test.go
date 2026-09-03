package resourcegen

import (
	"strings"
	"testing"
)

func TestParseListQueryModifiers(t *testing.T) {
	t.Parallel()
	fields, err := parseFields([]string{
		"title:string:required,searchable,sortable",
		"views:int:filterable,sortable",
		"published:bool:filterable",
	}, "post")
	if err != nil {
		t.Fatalf("parseFields() error = %v", err)
	}
	title, views, published := fields[0], fields[1], fields[2]
	if !title.Searchable || !title.Sortable || title.Filterable {
		t.Fatalf("title flags = %+v, want searchable+sortable only", title)
	}
	if !views.Filterable || !views.Sortable || views.Searchable {
		t.Fatalf("views flags = %+v, want filterable+sortable only", views)
	}
	if !published.Filterable || published.Sortable || published.Searchable {
		t.Fatalf("published flags = %+v, want filterable only", published)
	}
}

func TestListQueryModifierTypeErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		spec    string
		wantErr string
	}{
		{"searchable int", "views:int:searchable", "cannot be searchable"},
		{"filterable decimal", "price:decimal:filterable", "cannot be filterable"},
		{"filterable time", "starts_at:time:filterable", "cannot be filterable"},
		{"filterable text", "body:text:filterable", "cannot be filterable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseFields([]string{tt.spec}, "widget")
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("parseFields(%q) error = %v, want %q", tt.spec, err, tt.wantErr)
			}
		})
	}
}

// TestReservedQueryFieldNamesRejected guards the collision the generated list
// input would otherwise hit: a field named after a list-query param (page,
// per_page, search, ordering) becomes a duplicate struct field with a duplicate
// `query` tag — gofmt accepts it, go build does not. parseFields must refuse it
// upfront, with or without a modifier.
func TestReservedQueryFieldNamesRejected(t *testing.T) {
	t.Parallel()
	specs := []string{
		"page:int:filterable",
		"page:int",
		"per_page:int:filterable",
		"search:string:filterable",
		"ordering:string:filterable",
		"search:string",
		"ordering:string:sortable",
	}
	for _, spec := range specs {
		t.Run(spec, func(t *testing.T) {
			t.Parallel()
			if _, err := parseFields([]string{spec}, "post"); err == nil || !strings.Contains(err.Error(), "reserved for the list-query params") {
				t.Fatalf("parseFields(%q) error = %v, want reserved-name rejection", spec, err)
			}
		})
	}
}

// TestBelongsToFilterableByDefault documents that a belongs_to foreign key is a
// filter without any modifier — the has_many detail-list contract (#260).
func TestBelongsToFilterableByDefault(t *testing.T) {
	t.Parallel()
	fields, err := parseFields([]string{"author:belongs_to:Author"}, "post")
	if err != nil {
		t.Fatalf("parseFields() error = %v", err)
	}
	if !fields[0].isFilterable() {
		t.Fatal("belongs_to field should be filterable by default")
	}
	if fields[0].filterColumn() != "author_id" {
		t.Fatalf("belongs_to filter column = %q, want author_id", fields[0].filterColumn())
	}
}

func TestRenderHandlerListQuery(t *testing.T) {
	t.Parallel()
	fields, err := parseFields([]string{
		"title:string:required,searchable,sortable",
		"body:text:searchable",
		"views:int:filterable,sortable",
		"published:bool:filterable",
		"author:belongs_to:Author",
	}, "post")
	if err != nil {
		t.Fatalf("parseFields() error = %v", err)
	}
	name, err := parseResourceName("Post")
	if err != nil {
		t.Fatalf("parseResourceName() error = %v", err)
	}
	src := string(mustFormatGo(renderHandler(newRenderContext("example.com/demo", name, fields, "/api/v1", "minimal", false, false))))
	if strings.Contains(src, "format error") {
		t.Fatalf("generated handler does not format:\n%s", src)
	}
	wantContains := []string{
		`query:"search" doc:"Search term`,
		`query:"ordering" doc:"Field to order by; prefix with - for DESC (allowed: title, views)"`,
		`query:"views" doc:"Filter by Views (exact match)"`,
		`query:"published" enum:"true,false" doc:"Filter by Published (exact match)"`,
		`query:"author_id" doc:"Filter by AuthorID (exact match)"`,
		`database.FilterEq(ctx, q, "views", database.FilterInt, input.Views)`,
		`database.FilterEq(ctx, q, "published", database.FilterBool, input.Published)`,
		`database.FilterEq(ctx, q, "author_id", database.FilterUint, input.AuthorID)`,
		`database.Search(q, []string{"title", "body"}, input.Search)`,
		`database.Ordering(ctx, q, input.Ordering, []string{"title", "views"}, "id")`,
	}
	for _, want := range wantContains {
		if !strings.Contains(src, want) {
			t.Fatalf("generated handler missing %q\n%s", want, src)
		}
	}
	// Body is searchable but not filterable/sortable: no filter param, no sort entry.
	if strings.Contains(src, `query:"body"`) {
		t.Fatalf("body must not be a filter param:\n%s", src)
	}
}

// TestRenderHandlerNoListQuery is the regression guard that a resource declaring
// no list-query modifiers keeps the original fixed Order("id") page and adds no
// filter/search/sort query params.
func TestRenderHandlerNoListQuery(t *testing.T) {
	t.Parallel()
	fields, err := parseFields([]string{"title:string:required"}, "post")
	if err != nil {
		t.Fatalf("parseFields() error = %v", err)
	}
	name, err := parseResourceName("Post")
	if err != nil {
		t.Fatalf("parseResourceName() error = %v", err)
	}
	src := string(mustFormatGo(renderHandler(newRenderContext("example.com/demo", name, fields, "/api/v1", "minimal", false, false))))
	if !strings.Contains(src, `q.Order("id").Offset(`) {
		t.Fatalf("handler without sort must keep fixed Order(\"id\"):\n%s", src)
	}
	for _, unwanted := range []string{`query:"search"`, `query:"ordering"`, `database.Ordering`, `database.Search`, `database.FilterEq`} {
		if strings.Contains(src, unwanted) {
			t.Fatalf("handler without modifiers must not contain %q:\n%s", unwanted, src)
		}
	}
}
