package contract_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/contract"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humagin"
	"github.com/gin-gonic/gin"
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

// A missing required property of a nested object is reported by Huma against
// the object that lacks it, so the D10 key must name the property: the
// scaffold's applyContractErrors feeds keys straight into react-hook-form's
// setError, whose field paths are `address.city` and `lines[1].sku`.
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

	cases := []struct {
		name string
		body string
		want map[string][]string
	}{
		{
			name: "present but empty nested object",
			body: `{"title":"t","address":{"city":"","zip":"z"},"lines":[{"sku":"s"}]}`,
			want: map[string][]string{"address.city": {"expected length >= 1"}},
		},
		{
			name: "nested object without its properties",
			body: `{"title":"t","address":{},"lines":[{"sku":"s"}]}`,
			want: map[string][]string{
				"address.city": {"expected required property city to be present"},
				"address.zip":  {"expected required property zip to be present"},
			},
		},
		{
			name: "array item without its property",
			body: `{"title":"t","address":{"city":"c","zip":"z"},"lines":[{"sku":"s"},{}]}`,
			want: map[string][]string{
				"lines[1].sku": {"expected required property sku to be present"},
			},
		},
		{
			name: "top-level property stays keyed on its own name",
			body: `{"address":{"city":"c","zip":"z"},"lines":[{"sku":"s"}]}`,
			want: map[string][]string{
				"title": {"expected required property title to be present"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/orders", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusUnprocessableEntity, rec.Body.String())
			}
			var envelope contract.ErrorEnvelope
			if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("decode error envelope: %v; body: %s", err, rec.Body.String())
			}
			for key, want := range tc.want {
				if got := envelope.Body.Fields[key]; !reflect.DeepEqual(got, want) {
					t.Fatalf("fields[%q] = %#v, want %#v; fields=%#v", key, got, want, envelope.Body.Fields)
				}
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
