package framework

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gin-gonic/gin"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/contract"
)

func TestAPIValidationReturnsD10FieldErrors(t *testing.T) {
	app := newTestApp(t)

	type createBody struct {
		Name string `json:"name" minLength:"1" maxLength:"80"`
	}
	type createInput struct {
		Body createBody
	}
	type createOutput struct {
		Body contract.Data[createBody]
	}

	prefix := app.Config().API.Prefix
	huma.Register(app.API(), huma.Operation{
		OperationID: "create-widget",
		Method:      http.MethodPost,
		Path:        prefix + "/widgets",
		Summary:     "Create a widget",
	}, func(ctx context.Context, input *createInput) (*createOutput, error) {
		return &createOutput{Body: contract.Data[createBody]{Data: input.Body}}, nil
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, prefix+"/widgets", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(RequestIDHeader, "req-framework-1")
	app.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusUnprocessableEntity, rec.Body.String())
	}
	if got := rec.Header().Get(RequestIDHeader); got != "req-framework-1" {
		t.Fatalf("%s = %q, want req-framework-1", RequestIDHeader, got)
	}

	var body contract.ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error envelope: %v; body: %s", err, rec.Body.String())
	}
	if body.Body.Code != contract.CodeValidationError {
		t.Fatalf("code = %q, want %q", body.Body.Code, contract.CodeValidationError)
	}
	if body.Body.RequestID != "req-framework-1" {
		t.Fatalf("request_id = %q, want req-framework-1", body.Body.RequestID)
	}
	if len(body.Body.Fields["name"]) == 0 {
		t.Fatalf("fields.name missing; fields=%#v body=%s", body.Body.Fields, rec.Body.String())
	}
}

func TestAPIOpenAPIUsesD10ErrorSchema(t *testing.T) {
	app := newTestApp(t)

	huma.Register(app.API(), huma.Operation{
		OperationID: "ping",
		Method:      http.MethodGet,
		Path:        "/ping",
	}, func(ctx context.Context, input *struct{}) (*struct{}, error) {
		return nil, nil
	})

	rec := httptest.NewRecorder()
	app.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json status = %d; body: %s", rec.Code, rec.Body.String())
	}

	spec := rec.Body.String()
	if strings.Contains(spec, `"ErrorModel"`) {
		t.Fatalf("OpenAPI still advertises ErrorModel Problem Details schema")
	}
	if strings.Contains(spec, "application/problem+json") {
		t.Fatalf("OpenAPI still advertises application/problem+json")
	}
	if !strings.Contains(spec, `"ErrorEnvelope"`) {
		t.Fatalf("OpenAPI missing ErrorEnvelope schema; body: %s", spec)
	}
}

// TestAPIResponseOmitsSchemaKey verifies #225 item 2: Huma's SchemaLinkTransformer
// is disabled, so neither response bodies nor the OpenAPI request/response
// schemas carry an off-contract "$schema" key. The D10 envelope is exactly
// {data, meta?} / {error}.
func TestAPIResponseOmitsSchemaKey(t *testing.T) {
	app := newTestApp(t)

	prefix := app.Config().API.Prefix
	huma.Register(app.API(), huma.Operation{
		OperationID: "get-thing",
		Method:      http.MethodGet,
		Path:        prefix + "/things",
	}, func(ctx context.Context, input *struct{}) (*struct {
		Body contract.Data[[]string]
	}, error) {
		return &struct {
			Body contract.Data[[]string]
		}{Body: contract.Data[[]string]{Data: []string{"a"}}}, nil
	})

	rec := httptest.NewRecorder()
	app.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, prefix+"/things", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode body: %v; body: %s", err, rec.Body.String())
	}
	if _, ok := raw["$schema"]; ok {
		t.Fatalf("response body carries off-contract $schema key: %s", rec.Body.String())
	}

	oapi := httptest.NewRecorder()
	app.Router().ServeHTTP(oapi, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	if strings.Contains(oapi.Body.String(), `"$schema"`) {
		t.Fatalf("OpenAPI schemas still advertise $schema; body: %s", oapi.Body.String())
	}
}

func TestAPIDocsServesSwaggerUIAndOmitsRawRoutes(t *testing.T) {
	app := newTestApp(t)

	prefix := app.Config().API.Prefix
	huma.Register(app.API(), huma.Operation{
		OperationID: "list-widgets",
		Method:      http.MethodGet,
		Path:        prefix + "/widgets",
		Summary:     "List widgets",
	}, func(ctx context.Context, input *struct{}) (*struct {
		Body contract.Data[[]string]
	}, error) {
		return &struct {
			Body contract.Data[[]string]
		}{Body: contract.Data[[]string]{Data: []string{"widget-1"}}}, nil
	})
	app.Router().GET("/raw/ping", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"status": "ok"}})
	})

	docs := httptest.NewRecorder()
	app.Router().ServeHTTP(docs, httptest.NewRequest(http.MethodGet, "/docs", nil))
	if docs.Code != http.StatusOK {
		t.Fatalf("GET /docs status = %d; body: %s", docs.Code, docs.Body.String())
	}
	if !strings.Contains(docs.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("GET /docs Content-Type = %q, want text/html", docs.Header().Get("Content-Type"))
	}
	if !strings.Contains(docs.Body.String(), "swagger-ui") {
		t.Fatalf("GET /docs missing swagger-ui; body: %s", docs.Body.String())
	}
	if !strings.Contains(docs.Body.String(), "/openapi") {
		t.Fatalf("GET /docs missing OpenAPI URL for try-it-out; body: %s", docs.Body.String())
	}
	csp := docs.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "unpkg.com/swagger-ui-dist") || !strings.Contains(csp, "connect-src") {
		t.Fatalf("GET /docs CSP = %q, want Swagger UI + connect-src so try-it-out works", csp)
	}

	specRec := httptest.NewRecorder()
	app.Router().ServeHTTP(specRec, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	if specRec.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json status = %d; body: %s", specRec.Code, specRec.Body.String())
	}
	generated, err := contract.OpenAPIJSON(app.API())
	if err != nil {
		t.Fatalf("OpenAPIJSON() error = %v", err)
	}
	if !jsonBodiesEqual(generated, specRec.Body.Bytes()) {
		t.Fatal("generated OpenAPI JSON differs from served /openapi.json")
	}
	spec := specRec.Body.String()
	if !strings.Contains(spec, prefix+"/widgets") {
		t.Fatalf("OpenAPI missing Huma route %s/widgets; body: %s", prefix, spec)
	}
	if strings.Contains(spec, "/raw/ping") {
		t.Fatal("raw Gin route /raw/ping unexpectedly appears in OpenAPI")
	}
	if strings.Contains(spec, "/livez") || strings.Contains(spec, "/readyz") || strings.Contains(spec, "/metrics") {
		t.Fatal("framework probe/metrics routes unexpectedly appear in OpenAPI")
	}
}

func TestAPIDocsOffForDefaultForProduction(t *testing.T) {
	previousMode := gin.Mode()
	t.Cleanup(func() {
		gin.SetMode(previousMode)
	})

	cfg := config.DefaultFor(config.EnvironmentProduction)
	cfg.HTTP.Addr = "127.0.0.1:0"
	app := newTestApp(t, WithConfig(cfg))

	docs := httptest.NewRecorder()
	app.Router().ServeHTTP(docs, httptest.NewRequest(http.MethodGet, "/docs", nil))
	if docs.Code != http.StatusNotFound {
		t.Fatalf("GET /docs status = %d, want %d for DefaultFor(production)", docs.Code, http.StatusNotFound)
	}
}

func TestAPIDocsCanBeDisabled(t *testing.T) {
	cfg := config.Default()
	cfg.Environment = config.EnvironmentTest
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.API.DocsEnabled = false
	app := newTestApp(t, WithConfig(cfg))

	docs := httptest.NewRecorder()
	app.Router().ServeHTTP(docs, httptest.NewRequest(http.MethodGet, "/docs", nil))
	if docs.Code != http.StatusNotFound {
		t.Fatalf("GET /docs status = %d, want %d when docs disabled", docs.Code, http.StatusNotFound)
	}

	spec := httptest.NewRecorder()
	app.Router().ServeHTTP(spec, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	if spec.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json status = %d, want %d when docs disabled", spec.Code, http.StatusOK)
	}
}

func TestAppExposesAPI(t *testing.T) {
	app := newTestApp(t)
	if app.API() == nil {
		t.Fatal("API() = nil, want huma.API")
	}
}

func TestAPICategoryErrorReturnsD10Envelope(t *testing.T) {
	app := newTestApp(t)

	type getInput struct {
		ID string `path:"id"`
	}
	type getOutput struct {
		Body contract.Data[map[string]string]
	}

	prefix := app.Config().API.Prefix
	huma.Register(app.API(), huma.Operation{
		OperationID: "get-widget",
		Method:      http.MethodGet,
		Path:        prefix + "/widgets/{id}",
		Summary:     "Get a widget",
	}, func(ctx context.Context, input *getInput) (*getOutput, error) {
		return nil, contract.WithContext(ctx, contract.NotFound("widget not found"))
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, prefix+"/widgets/missing", nil)
	req.Header.Set(RequestIDHeader, "req-framework-2")
	app.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}

	var body contract.ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error envelope: %v; body: %s", err, rec.Body.String())
	}
	if body.Body.Code != "not_found" {
		t.Fatalf("code = %q, want not_found", body.Body.Code)
	}
	if body.Body.RequestID != "req-framework-2" {
		t.Fatalf("request_id = %q, want req-framework-2", body.Body.RequestID)
	}
}

func jsonBodiesEqual(left, right []byte) bool {
	var leftValue any
	var rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		return false
	}
	leftBytes, err := json.Marshal(leftValue)
	if err != nil {
		return false
	}
	rightBytes, err := json.Marshal(rightValue)
	if err != nil {
		return false
	}
	return string(leftBytes) == string(rightBytes)
}
