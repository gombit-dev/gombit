package framework

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gin-gonic/gin"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/contract"
)

// oversizedJSONBody returns a syntactically valid JSON object whose encoded
// length exceeds maxRequestBodyBytes.
func oversizedJSONBody() string {
	return `{"x":"` + strings.Repeat("a", int(maxRequestBodyBytes)+64) + `"}`
}

// TestRequestBodyLimitRejectsOversizedJSONOnRawGin is the regression guard for
// the #271 review: making the sanitizer opt-in must not remove the request-size
// bound from raw app.Router() JSON routes. An oversized JSON body is rejected
// with a D10 413 before the handler runs, on a default app (sanitization off).
func TestRequestBodyLimitRejectsOversizedJSONOnRawGin(t *testing.T) {
	app := newTestApp(t) // Security.SanitizeInput is false by default (issue #271)

	handlerRan := false
	app.Router().POST("/echo", func(c *gin.Context) {
		handlerRan = true
		var body map[string]any
		_ = c.ShouldBindJSON(&body)
		c.Status(http.StatusNoContent)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/echo", strings.NewReader(oversizedJSONBody()))
	req.Header.Set("Content-Type", "application/json")
	app.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body: %s", rec.Code, rec.Body.String())
	}
	if handlerRan {
		t.Fatal("handler ran on an oversized body — the size gate must reject before dispatch")
	}
	var env contract.ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v; body: %s", err, rec.Body.String())
	}
	if env.Body.Code != contract.CodePayloadTooLarge {
		t.Fatalf("code = %q, want %q (D10 413)", env.Body.Code, contract.CodePayloadTooLarge)
	}
}

// TestRequestBodyLimitRejectsOversizedJSONOnHumaRoute covers the other dispatch
// path: the same size gate protects Huma-typed operations, and aborts before
// the Huma handler executes.
func TestRequestBodyLimitRejectsOversizedJSONOnHumaRoute(t *testing.T) {
	app := newTestApp(t)

	type createBody struct {
		X string `json:"x"`
	}
	type createInput struct {
		Body createBody
	}
	type createOutput struct {
		Body contract.Data[createBody]
	}
	handlerRan := false
	prefix := app.Config().API.Prefix
	huma.Register(app.API(), huma.Operation{
		OperationID: "create-thing",
		Method:      http.MethodPost,
		Path:        prefix + "/things",
		Summary:     "Create a thing",
	}, func(_ context.Context, input *createInput) (*createOutput, error) {
		handlerRan = true
		return &createOutput{Body: contract.Data[createBody]{Data: input.Body}}, nil
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, prefix+"/things", strings.NewReader(oversizedJSONBody()))
	req.Header.Set("Content-Type", "application/json")
	app.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body: %s", rec.Code, rec.Body.String())
	}
	if handlerRan {
		t.Fatal("Huma handler ran on an oversized body — the size gate must reject before dispatch")
	}
}

// TestRequestBodyLimitAllowsBodyUnderCap confirms an ordinary small JSON body
// passes through untouched (the gate only rejects oversized bodies).
func TestRequestBodyLimitAllowsBodyUnderCap(t *testing.T) {
	app := newTestApp(t)
	app.Router().POST("/echo", func(c *gin.Context) {
		var body map[string]string
		if err := c.ShouldBindJSON(&body); err != nil {
			c.Status(http.StatusBadRequest)
			return
		}
		c.JSON(http.StatusOK, body)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/echo", strings.NewReader(`{"comment":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	app.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body: %s", err, rec.Body.String())
	}
	if body["comment"] != "hello" {
		t.Fatalf("comment = %q, want %q", body["comment"], "hello")
	}
}

// TestRequestBodyLimitExemptsRawBodyPaths locks the parity decision: a
// WithRawBodyPaths path keeps its exact bytes and is not subject to the size
// gate (as it was before #271 — the sanitizer, and thus its cap, skipped these
// paths entirely so a webhook signature verifies over the original body).
func TestRequestBodyLimitExemptsRawBodyPaths(t *testing.T) {
	cfg := config.Default()
	cfg.Environment = config.EnvironmentTest
	cfg.HTTP.Addr = "127.0.0.1:0"
	app := newTestApp(t, WithConfig(cfg), WithRawBodyPaths("/webhooks/github"))

	var gotLen int
	app.Router().POST("/webhooks/github", func(c *gin.Context) {
		b, _ := io.ReadAll(c.Request.Body)
		gotLen = len(b)
		c.Status(http.StatusOK)
	})

	body := oversizedJSONBody()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	app.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (raw-body path is exempt from the size gate); body: %s", rec.Code, rec.Body.String())
	}
	if gotLen != len(body) {
		t.Fatalf("handler read %d bytes, want the full %d — a raw-body path must reach the handler unmodified", gotLen, len(body))
	}
}

// TestRequestBodyLimitIgnoresNonJSON documents the JSON-only scope (parity with
// the sanitizer's old cap, which only bounded JSON bodies): a large non-JSON
// body is not gated here.
func TestRequestBodyLimitIgnoresNonJSON(t *testing.T) {
	app := newTestApp(t)
	app.Router().POST("/upload", func(c *gin.Context) {
		b, _ := io.ReadAll(c.Request.Body)
		c.JSON(http.StatusOK, gin.H{"len": len(b)})
	})

	body := strings.Repeat("a", int(maxRequestBodyBytes)+64)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader(body))
	req.Header.Set("Content-Type", "text/plain")
	app.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (non-JSON body is not size-gated); body: %s", rec.Code, rec.Body.String())
	}
}
