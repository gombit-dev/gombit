package framework

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gombit-dev/gombit/contract"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A handler whose response cannot be encoded (a stored time outside years
// 0..9999, a float holding ±Inf) is answered with a D10 500 carrying the
// request ID and logged through the app's logger, not with HTTP 200 and the
// plain-text body "error marshaling response" (issue #442). The single row and
// any list holding it fail the same way.
func TestUnencodableResponseIsAD10InternalError(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	app := newTestApp(t, WithLogger(zap.New(core)))
	prefix := app.Config().API.Prefix

	type invoice struct {
		Due time.Time `json:"due"`
	}
	type oneOutput struct {
		Body contract.Data[invoice]
	}
	type listOutput struct {
		Body contract.DataMeta[[]invoice, contract.PageMeta]
	}
	type rateOutput struct {
		Body contract.Data[float64]
	}
	bad := invoice{Due: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}
	huma.Register(app.API(), huma.Operation{OperationID: "get-invoice", Method: http.MethodGet, Path: prefix + "/invoices/1"},
		func(context.Context, *struct{}) (*oneOutput, error) {
			return &oneOutput{Body: contract.Data[invoice]{Data: bad}}, nil
		})
	huma.Register(app.API(), huma.Operation{OperationID: "list-invoices", Method: http.MethodGet, Path: prefix + "/invoices"},
		func(context.Context, *struct{}) (*listOutput, error) {
			return &listOutput{Body: contract.DataMeta[[]invoice, contract.PageMeta]{Data: []invoice{{}, bad}}}, nil
		})
	huma.Register(app.API(), huma.Operation{OperationID: "get-rate", Method: http.MethodGet, Path: prefix + "/rate"},
		func(context.Context, *struct{}) (*rateOutput, error) {
			return &rateOutput{Body: contract.Data[float64]{Data: math.Inf(1)}}, nil
		})

	for _, path := range []string{"/invoices/1", "/invoices", "/rate"} {
		rec := httptest.NewRecorder()
		app.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, prefix+path, nil))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("GET %s status = %d, want 500; body %q", path, rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Fatalf("GET %s Content-Type = %q, want application/json", path, ct)
		}
		var body contract.ErrorEnvelope
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("GET %s body is not the D10 envelope: %v: %q", path, err, rec.Body.String())
		}
		id := rec.Header().Get(RequestIDHeader)
		if body.Body.Code != string(contract.CategoryInternal) || id == "" || body.Body.RequestID != id {
			t.Fatalf("GET %s envelope = %+v, request ID header %q", path, body.Body, id)
		}
		entries := logs.FilterField(zap.String("request_id", id)).All()
		if len(entries) != 1 || entries[0].Level != zapcore.ErrorLevel {
			t.Fatalf("GET %s: want one error log for request %s, got %v", path, id, logs.All())
		}
	}
}
