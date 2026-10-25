package contract_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humagin"
	"github.com/gin-gonic/gin"
	"github.com/gombit-dev/gombit/contract"
)

func TestValidationReturnsD10FieldErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	api := humagin.New(router, contract.HumaConfig("contract-test", "0.0.0"))

	type createBody struct {
		Name string `json:"name" minLength:"1" maxLength:"80"`
	}
	type createInput struct {
		Body createBody
	}
	type createOutput struct {
		Body contract.Data[createBody]
	}

	huma.Register(api, huma.Operation{
		OperationID: "create-widget",
		Method:      http.MethodPost,
		Path:        "/widgets",
		Summary:     "Create a widget",
	}, func(ctx context.Context, input *createInput) (*createOutput, error) {
		return &createOutput{Body: contract.Data[createBody]{Data: input.Body}}, nil
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/widgets", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusUnprocessableEntity, rec.Body.String())
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	if strings.Contains(ct, "problem+json") {
		t.Fatalf("Content-Type = %q, must not be problem+json", ct)
	}

	var body contract.ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error envelope: %v; body: %s", err, rec.Body.String())
	}
	if body.Body.Code != contract.CodeValidationError {
		t.Fatalf("code = %q, want %q", body.Body.Code, contract.CodeValidationError)
	}
	if body.Body.RequestID != "req-test-1" {
		t.Fatalf("request_id = %q, want req-test-1", body.Body.RequestID)
	}
	if len(body.Body.Fields["name"]) == 0 {
		t.Fatalf("fields.name missing; fields=%#v body=%s", body.Body.Fields, rec.Body.String())
	}
}

// A missing property is reported by Huma against the object that lacks it,
// whatever kind of location that object sits at, so the D10 key must name the
// property: the scaffold's applyContractErrors feeds keys straight into
// react-hook-form's setError, whose field paths are `address.city`,
// `lines[1].sku`, `body[1].sku` and `query.filter.kind`.
func TestValidationKeysNestedRequiredProperties(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	api := humagin.New(router, contract.HumaConfig("contract-test", "0.0.0"))

	type address struct {
		City string `json:"city" minLength:"1"`
		Zip  string `json:"zip"`
	}
	type line struct {
		SKU string `json:"sku" minLength:"1"`
	}
	type createOrderInput struct {
		Body struct {
			Title   string  `json:"title"`
			Address address `json:"address"`
			Lines   []line  `json:"lines"`
		}
	}

	huma.Register(api, huma.Operation{
		OperationID: "create-order",
		Method:      http.MethodPost,
		Path:        "/orders",
	}, func(ctx context.Context, input *createOrderInput) (*struct{}, error) {
		return nil, nil
	})

	// The body itself is an array: Huma keys item errors at `body[i]` with no
	// dot after `body` (PushIndex appends `[i]`), so a missing property must
	// still grow that key into the property's own path.
	type createLineItemsInput struct {
		Body []line
	}
	huma.Register(api, huma.Operation{
		OperationID: "create-line-items",
		Method:      http.MethodPost,
		Path:        "/line-items",
	}, func(ctx context.Context, input *createLineItemsInput) (*struct{}, error) {
		return nil, nil
	})

	// A deepObject query param is an object too: Huma validates it against its
	// schema at `query.filter`, so a missing required property arrives there.
	// The schema property name comes from the `json` tag (a `query` tag on an
	// inner field does not name it), so the tag carries the lowercase name.
	type filterSearchInput struct {
		Filter struct {
			Name string `json:"name" minLength:"2"`
			Kind string `json:"kind"`
		} `query:"filter,deepObject"`
	}
	huma.Register(api, huma.Operation{
		OperationID: "filter-search",
		Method:      http.MethodGet,
		Path:        "/filter-search",
	}, func(ctx context.Context, input *filterSearchInput) (*struct{}, error) {
		return nil, nil
	})

	// dependentRequired pulls a property in without it being required: Huma
	// reports the missing dependent at the object's path as well.
	type createShipmentInput struct {
		Body struct {
			Address struct {
				City string `json:"city,omitempty" dependentRequired:"zip"`
				Zip  string `json:"zip,omitempty"`
			} `json:"address"`
		}
	}
	huma.Register(api, huma.Operation{
		OperationID: "create-shipment",
		Method:      http.MethodPost,
		Path:        "/shipments",
	}, func(ctx context.Context, input *createShipmentInput) (*struct{}, error) {
		return nil, nil
	})

	cases := []struct {
		name   string
		method string
		path   string
		body   string
		want   map[string][]string
	}{
		{
			name:   "present but invalid nested value",
			method: http.MethodPost,
			path:   "/orders",
			body:   `{"title":"t","address":{"city":"","zip":"z"},"lines":[{"sku":"s"}]}`,
			want:   map[string][]string{"address.city": {"expected length >= 1"}},
		},
		{
			name:   "nested object without its properties",
			method: http.MethodPost,
			path:   "/orders",
			body:   `{"title":"t","address":{},"lines":[{"sku":"s"}]}`,
			want: map[string][]string{
				"address.city": {"expected required property city to be present"},
				"address.zip":  {"expected required property zip to be present"},
			},
		},
		{
			name:   "array item without its property",
			method: http.MethodPost,
			path:   "/orders",
			body:   `{"title":"t","address":{"city":"c","zip":"z"},"lines":[{"sku":"s"},{}]}`,
			want: map[string][]string{
				"lines[1].sku": {"expected required property sku to be present"},
			},
		},
		{
			name:   "top-level property stays keyed on its own name",
			method: http.MethodPost,
			path:   "/orders",
			body:   `{"address":{"city":"c","zip":"z"},"lines":[{"sku":"s"}]}`,
			want: map[string][]string{
				"title": {"expected required property title to be present"},
			},
		},
		{
			name:   "top-level array body item without its property",
			method: http.MethodPost,
			path:   "/line-items",
			body:   `[{"sku":"s"},{}]`,
			want: map[string][]string{
				"body[1].sku": {"expected required property sku to be present"},
			},
		},
		{
			name:   "deepObject query object without its property",
			method: http.MethodGet,
			path:   "/filter-search?filter[name]=x",
			want: map[string][]string{
				"query.filter.kind": {"expected required property kind to be present"},
				"query.filter.name": {"expected length >= 2"},
			},
		},
		{
			name:   "dependent required property names the missing dependent",
			method: http.MethodPost,
			path:   "/shipments",
			body:   `{"address":{"city":"c"}}`,
			want: map[string][]string{
				"address.zip": {"expected property zip to be present when city is present"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			var req *http.Request
			if tc.body == "" {
				req = httptest.NewRequest(tc.method, tc.path, nil)
			} else {
				req = httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
				req.Header.Set("Content-Type", "application/json")
			}
			router.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusUnprocessableEntity, rec.Body.String())
			}
			var envelope contract.ErrorEnvelope
			if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("decode error envelope: %v; body: %s", err, rec.Body.String())
			}
			if !reflect.DeepEqual(envelope.Body.Fields, tc.want) {
				t.Fatalf("fields = %#v, want %#v; body: %s", envelope.Body.Fields, tc.want, rec.Body.String())
			}
		})
	}
}

func TestHumaConfigEnablesDocsAndDisablesSchemas(t *testing.T) {
	cfg := contract.HumaConfig("Demo", "1.2.3")
	if cfg.DocsPath != contract.DocsPath {
		t.Fatalf("DocsPath = %q, want %q", cfg.DocsPath, contract.DocsPath)
	}
	if cfg.DocsRenderer != huma.DocsRendererSwaggerUI {
		t.Fatalf("DocsRenderer = %q, want %q", cfg.DocsRenderer, huma.DocsRendererSwaggerUI)
	}
	if cfg.SchemasPath != "" {
		t.Fatalf("SchemasPath = %q, want empty", cfg.SchemasPath)
	}
	if cfg.OpenAPIPath != contract.OpenAPIPath {
		t.Fatalf("OpenAPIPath = %q, want %q", cfg.OpenAPIPath, contract.OpenAPIPath)
	}
	if cfg.Info == nil || cfg.Info.Title != "Demo" || cfg.Info.Version != "1.2.3" {
		t.Fatalf("Info = %#v, want Demo/1.2.3", cfg.Info)
	}

	disabled := contract.HumaConfigFor("Demo", "1.2.3", false)
	if disabled.DocsPath != "" {
		t.Fatalf("HumaConfigFor(docs=false) DocsPath = %q, want empty", disabled.DocsPath)
	}
}

func TestOpenAPIUsesD10ErrorSchema(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	api := humagin.New(router, contract.HumaConfig("contract-test", "0.0.0"))

	huma.Register(api, huma.Operation{
		OperationID: "ping",
		Method:      http.MethodGet,
		Path:        "/ping",
	}, func(ctx context.Context, input *struct{}) (*struct{}, error) {
		return nil, nil
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json status = %d; body: %s", rec.Code, rec.Body.String())
	}

	spec := rec.Body.String()
	if !strings.Contains(spec, `"ErrorEnvelope"`) && !strings.Contains(spec, `"error"`) {
		t.Fatalf("OpenAPI missing D10 error shape markers; body: %s", spec)
	}
	if strings.Contains(spec, `"ErrorModel"`) {
		t.Fatalf("OpenAPI still advertises ErrorModel Problem Details schema")
	}
	if strings.Contains(spec, "application/problem+json") {
		t.Fatalf("OpenAPI still advertises application/problem+json")
	}
}
