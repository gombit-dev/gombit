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
// A belongs_to FK is filterable by default: the generated model tags its uint
// foreign key gombit:"read,write,filterable", so gombit generate produces the
// ?<parent>_id= filter that the has_many detail-list case relies on. (The
// association object carries no policy — it is not a column.)
func TestBelongsToFilterableByDefault(t *testing.T) {
	t.Parallel()
	fields, err := parseFields([]string{"author:belongs_to:Author"}, "post")
	if err != nil {
		t.Fatalf("parseFields() error = %v", err)
	}
	lines := modelFieldLines(fields[0], "post")
	if !strings.Contains(lines, "AuthorID uint `gorm:\"index\" gombit:\"read,write,filterable\"`") {
		t.Fatalf("belongs_to FK should be filterable by default:\n%s", lines)
	}
	if strings.Contains(lines, "Author author.Author `gombit:") {
		t.Fatalf("belongs_to association must carry no gombit policy:\n%s", lines)
	}
}
