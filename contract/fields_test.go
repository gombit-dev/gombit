package contract

import (
	"reflect"
	"testing"

	"github.com/danielgtaylor/huma/v2"
)

func TestFieldsFromErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		errs []error
		want map[string][]string
	}{
		{
			name: "nil and empty",
			errs: nil,
			want: nil,
		},
		{
			name: "body field strips prefix",
			errs: []error{
				&huma.ErrorDetail{Location: "body.name", Message: "expected length >= 1"},
			},
			want: map[string][]string{"name": {"expected length >= 1"}},
		},
		{
			name: "nested body path",
			errs: []error{
				&huma.ErrorDetail{Location: "body.items[0].tags", Message: "expected array"},
			},
			want: map[string][]string{"items[0].tags": {"expected array"}},
		},
		{
			name: "query path header keep prefix",
			errs: []error{
				&huma.ErrorDetail{Location: "query.limit", Message: "expected number"},
				&huma.ErrorDetail{Location: "path.widget-id", Message: "expected uuid"},
				&huma.ErrorDetail{Location: "header.x-token", Message: "required"},
			},
			want: map[string][]string{
				"query.limit":    {"expected number"},
				"path.widget-id": {"expected uuid"},
				"header.x-token": {"required"},
			},
		},
		{
			name: "multiple messages same field",
			errs: []error{
				&huma.ErrorDetail{Location: "body.email", Message: "required"},
				&huma.ErrorDetail{Location: "body.email", Message: "invalid format"},
			},
			want: map[string][]string{"email": {"required", "invalid format"}},
		},
		{
			name: "plain error is not a field detail",
			errs: []error{errString("boom")},
			want: nil,
		},
		{
			name: "required property inferred from message",
			errs: []error{
				&huma.ErrorDetail{Message: "expected required property name to be present"},
			},
			want: map[string][]string{"name": {"expected required property name to be present"}},
		},
		{
			name: "required property extends a nested body path",
			errs: []error{
				&huma.ErrorDetail{Location: "body.address", Message: "expected required property city to be present"},
				&huma.ErrorDetail{Location: "body.address", Message: "expected required property zip to be present"},
			},
			want: map[string][]string{
				"address.city": {"expected required property city to be present"},
				"address.zip":  {"expected required property zip to be present"},
			},
		},
		{
			name: "required property in an array item",
			errs: []error{
				&huma.ErrorDetail{Location: "body.items[1]", Message: "expected required property name to be present"},
			},
			want: map[string][]string{
				"items[1].name": {"expected required property name to be present"},
			},
		},
		{
			name: "required property extends a top-level array body item",
			errs: []error{
				&huma.ErrorDetail{Location: "body[1]", Message: "expected required property sku to be present"},
			},
			want: map[string][]string{
				"body[1].sku": {"expected required property sku to be present"},
			},
		},
		{
			name: "required property extends a deepObject query path",
			errs: []error{
				&huma.ErrorDetail{Location: "query.filter", Message: "expected required property kind to be present"},
			},
			want: map[string][]string{
				"query.filter.kind": {"expected required property kind to be present"},
			},
		},
		{
			name: "required property extends a form object path",
			errs: []error{
				&huma.ErrorDetail{Location: "form.data", Message: "expected required property title to be present"},
			},
			want: map[string][]string{
				"form.data.title": {"expected required property title to be present"},
			},
		},
		{
			name: "dependent required property extends an object path",
			errs: []error{
				&huma.ErrorDetail{Location: "body.address", Message: "expected property zip to be present when city is present"},
			},
			want: map[string][]string{
				"address.zip": {"expected property zip to be present when city is present"},
			},
		},
		{
			name: "dependent required property at the body root",
			errs: []error{
				&huma.ErrorDetail{Location: "body", Message: "expected property zip to be present when city is present"},
			},
			want: map[string][]string{
				"zip": {"expected property zip to be present when city is present"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := FieldsFromErrors(tt.errs...)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("FieldsFromErrors() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

type errString string

func (e errString) Error() string { return string(e) }
