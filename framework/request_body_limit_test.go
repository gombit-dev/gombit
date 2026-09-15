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

// TestRequestBodyLimitAppliesToRawBodyPaths locks the #271 review decision: the
// size gate is NOT bypassed for WithRawBodyPaths. Bounding the read does not
// alter accepted bytes, so a webhook still gets its exact body for signature
// verification — but it no longer gets an unlimited body. An oversized webhook
// body is rejected with a 413 before the handler runs; a within-cap one reaches
// the handler byte-for-byte.
func TestRequestBodyLimitAppliesToRawBodyPaths(t *testing.T) {
	cfg := config.Default()
	cfg.Environment = config.EnvironmentTest
	cfg.HTTP.Addr = "127.0.0.1:0"
	app := newTestApp(t, WithConfig(cfg), WithRawBodyPaths("/webhooks/github"))

	handlerRan := false
	var gotBody string
	app.Router().POST("/webhooks/github", func(c *gin.Context) {
		handlerRan = true
		b, _ := io.ReadAll(c.Request.Body)
		gotBody = string(b)
		c.Status(http.StatusOK)
	})

	// Oversized: rejected before the handler runs, even on a raw-body path.
	oversized := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader(oversizedJSONBody()))
	req.Header.Set("Content-Type", "application/json")
	app.Router().ServeHTTP(oversized, req)
	if oversized.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized raw-body status = %d, want 413 (raw paths are no longer exempt); body: %s", oversized.Code, oversized.Body.String())
	}
	if handlerRan {
		t.Fatal("webhook handler ran on an oversized body — the size gate must reject before dispatch")
	}

	// Within cap: reaches the handler byte-for-byte, so a signature still verifies.
	const signed = `{"event":"push","ref":"refs/heads/main"}`
	ok := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader(signed))
	req2.Header.Set("Content-Type", "application/json")
	app.Router().ServeHTTP(ok, req2)
	if ok.Code != http.StatusOK {
		t.Fatalf("within-cap raw-body status = %d, want 200; body: %s", ok.Code, ok.Body.String())
	}
	if gotBody != signed {
		t.Fatalf("handler read %q, want the body byte-for-byte %q", gotBody, signed)
	}
}

// TestRequestBodyLimitRejectsOversizedChunkedJSON is the #271-review regression
// guard for chunked transfer (ContentLength == -1): with no declared length the
// gate must still reject an oversized body with a D10 413 before the handler
// runs, not merely bound the read and let the handler ignore a read error.
func TestRequestBodyLimitRejectsOversizedChunkedJSON(t *testing.T) {
	app := newTestApp(t)

	handlerRan := false
	app.Router().POST("/echo", func(c *gin.Context) {
		handlerRan = true
		b, _ := io.ReadAll(c.Request.Body)
		c.JSON(http.StatusOK, gin.H{"len": len(b)})
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/echo", strings.NewReader(oversizedJSONBody()))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = -1 // simulate chunked transfer: length unknown up front
	app.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked oversized status = %d, want 413; body: %s", rec.Code, rec.Body.String())
	}
	if handlerRan {
		t.Fatal("handler ran on an oversized chunked body — the gate must reject before dispatch")
	}
	var env contract.ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v; body: %s", err, rec.Body.String())
	}
	if env.Body.Code != contract.CodePayloadTooLarge {
		t.Fatalf("code = %q, want %q (D10 413)", env.Body.Code, contract.CodePayloadTooLarge)
	}
}

// TestRequestBodyLimitRestoresChunkedBodyUnderCap confirms the unknown-length
// path restores a within-cap body byte-for-byte for the handler after the gate
// buffered it to check the size.
func TestRequestBodyLimitRestoresChunkedBodyUnderCap(t *testing.T) {
	app := newTestApp(t)

	var gotBody string
	app.Router().POST("/echo", func(c *gin.Context) {
		b, _ := io.ReadAll(c.Request.Body)
		gotBody = string(b)
		c.Status(http.StatusOK)
	})

	const payload = `{"comment":"<b>hi</b> a < b"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/echo", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = -1
	app.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("chunked under-cap status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if gotBody != payload {
		t.Fatalf("handler read %q, want the body restored byte-for-byte %q", gotBody, payload)
	}
}

// TestRequestBodyLimitBoundsLyingContentLength locks the known-length hardening:
// even when a client declares a small Content-Length but streams a body larger
// than the cap, the MaxBytesReader wrap this layer installs stops the handler
// from reading past the cap — the bound is enforced by the layer, not trusted
// from the declared length.
func TestRequestBodyLimitBoundsLyingContentLength(t *testing.T) {
	app := newTestApp(t)

	var readLen int
	var readErr error
	app.Router().POST("/echo", func(c *gin.Context) {
		b, err := io.ReadAll(c.Request.Body)
		readLen = len(b)
		readErr = err
		c.Status(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/echo", strings.NewReader(oversizedJSONBody()))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = 16 // lie: declare a tiny length while delivering an oversized body
	app.Router().ServeHTTP(rec, req)

	if readErr == nil {
		t.Fatal("handler read a body with a lying small Content-Length without hitting the size bound")
	}
	if int64(readLen) > maxRequestBodyBytes {
		t.Fatalf("handler read %d bytes, past the %d cap, despite the size limit", readLen, maxRequestBodyBytes)
	}
}

// errAfterReader yields data, then fails with err — a truncated/broken request
// stream (e.g. a connection reset mid-body), which surfaces as a non-EOF read
// error rather than a clean end.
type errAfterReader struct {
	data []byte
	err  error
	pos  int
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if r.pos < len(r.data) {
		n := copy(p, r.data[r.pos:])
		r.pos += n
		return n, nil
	}
	return 0, r.err
}

// TestRequestBodyLimitAbortsOnStreamReadFailure is the #358-review guard: once
// the middleware takes the read on the unknown-length path, it owns the failure.
// A stream that fails mid-body must abort with a stable D10 client error before
// dispatch, not fabricate an empty body and let a non-validating handler answer
// 2xx.
func TestRequestBodyLimitAbortsOnStreamReadFailure(t *testing.T) {
	app := newTestApp(t)

	handlerRan := false
	app.Router().POST("/echo", func(c *gin.Context) {
		handlerRan = true
		_, _ = io.ReadAll(c.Request.Body)
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodPost, "/echo", &errAfterReader{data: []byte(`{"partial":`), err: io.ErrUnexpectedEOF})
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = -1 // unknown length → the buffering path owns the read
	rec := httptest.NewRecorder()
	app.Router().ServeHTTP(rec, req)

	if handlerRan {
		t.Fatal("handler ran on a broken request stream; the middleware must own the read failure and abort")
	}
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (D10 client error on a broken stream); body: %s", rec.Code, rec.Body.String())
	}
	var env contract.ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v; body: %s", err, rec.Body.String())
	}
	if env.Body.Code != contract.CodeValidationError {
		t.Fatalf("code = %q, want %q", env.Body.Code, contract.CodeValidationError)
	}
}

// TestRequestBodyLimitNormalizesChunkedFraming is the #358-review guard against
// impossible framing: after the middleware buffers a within-cap chunked body it
// must present a coherent fixed-length request — a non-negative ContentLength
// and no leftover chunked Transfer-Encoding. Models a real chunked request by
// setting both fields, not just ContentLength == -1.
func TestRequestBodyLimitNormalizesChunkedFraming(t *testing.T) {
	app := newTestApp(t)

	var gotCL int64
	var gotTE []string
	var gotBody string
	app.Router().POST("/echo", func(c *gin.Context) {
		gotCL = c.Request.ContentLength
		gotTE = c.Request.TransferEncoding
		b, _ := io.ReadAll(c.Request.Body)
		gotBody = string(b)
		c.Status(http.StatusOK)
	})

	const payload = `{"ok":true}`
	req := httptest.NewRequest(http.MethodPost, "/echo", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = -1
	req.TransferEncoding = []string{"chunked"}
	rec := httptest.NewRecorder()
	app.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if gotCL < 0 {
		t.Fatalf("handler saw ContentLength=%d, want a non-negative fixed length after buffering", gotCL)
	}
	if len(gotTE) != 0 {
		t.Fatalf("handler saw TransferEncoding=%v, want it cleared (no ContentLength>=0 + chunked contradiction)", gotTE)
	}
	if gotBody != payload {
		t.Fatalf("handler read %q, want the body restored byte-for-byte %q", gotBody, payload)
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
