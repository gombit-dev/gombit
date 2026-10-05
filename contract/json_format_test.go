package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

// Encoding first must not change what a good response looks like: the bytes
// are exactly Huma's default JSON format's, HTML left unescaped.
func TestJSONFormatWritesWhatHumasDefaultWrites(t *testing.T) {
	v := Data[map[string]any]{Data: map[string]any{"name": "<b>&co", "price": 1.5}}
	var want, got bytes.Buffer
	if err := huma.DefaultJSONFormat.Marshal(&want, v); err != nil {
		t.Fatal(err)
	}
	if err := JSONFormat(nil).Marshal(&got, v); err != nil {
		t.Fatal(err)
	}
	if got.String() != want.String() {
		t.Fatalf("JSONFormat wrote %q, Huma's default %q", got.String(), want.String())
	}
}

// A value encoding/json cannot encode becomes a D10 500 carrying the response's
// request ID, never a 200 with a plain-text body, and is reported (issue #442).
func TestJSONFormatAnswersAnUnencodableValueWithA500(t *testing.T) {
	for name, v := range map[string]any{
		"+Inf":       Data[float64]{Data: math.Inf(1)},
		"NaN":        Data[float64]{Data: math.NaN()},
		"year 10000": Data[time.Time]{Data: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)},
		"in a list":  Data[[]float64]{Data: []float64{1, math.Inf(-1)}},
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			rec.Header().Set(RequestIDHeader, "req-442")
			var reportedID string
			var reportedErr error
			err := JSONFormat(func(id string, err error) { reportedID, reportedErr = id, err }).Marshal(newPending(rec), v)
			if err != nil {
				t.Fatalf("Marshal() = %v, want the failure answered, not returned to Huma", err)
			}
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", rec.Code)
			}
			var body ErrorEnvelope
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not the D10 envelope: %v: %q", err, rec.Body.String())
			}
			if body.Body.Code != string(CategoryInternal) || body.Body.RequestID != "req-442" {
				t.Fatalf("envelope = %+v, want internal with the request ID", body.Body)
			}
			if strings.Contains(rec.Body.String(), "error marshaling response") {
				t.Fatalf("Huma's plain-text failure leaked into the body: %q", rec.Body.String())
			}
			if reportedID != "req-442" || reportedErr == nil {
				t.Fatalf("report(%q, %v), want the request ID and the encoding error", reportedID, reportedErr)
			}
		})
	}
}

// pending is a response writer that, like Gin's, holds the status until the
// first body byte: it proves it has sent nothing (Written) and reports the
// status it will send (Status).
type pending struct {
	*httptest.ResponseRecorder
	status int
}

func newPending(rec *httptest.ResponseRecorder) *pending {
	return &pending{ResponseRecorder: rec, status: http.StatusOK}
}

func (p *pending) Written() bool { return false }
func (p *pending) Status() int   { return p.status }
func (p *pending) WriteHeader(code int) {
	p.status = code
	p.ResponseRecorder.WriteHeader(code)
}

// holdsFirstStatus keeps the first status it is given, as a header-buffering
// wrapper over Gin's writer can: Huma's 200 sticks, so a 500 cannot be set.
type holdsFirstStatus struct {
	*httptest.ResponseRecorder
	status int
}

func (h *holdsFirstStatus) Written() bool { return false }
func (h *holdsFirstStatus) Status() int   { return h.status }
func (h *holdsFirstStatus) WriteHeader(code int) {
	if h.status == 0 {
		h.status = code
	}
}

type writtenWriter struct{ http.ResponseWriter }

func (writtenWriter) Written() bool { return true }
func (writtenWriter) Status() int   { return http.StatusOK }

// Where the status may already be on the wire, the error goes back to Huma as
// before, and nothing is written: a writer that is not an http.ResponseWriter,
// one that already wrote, and a net/http writer, which cannot say (Huma has
// called its WriteHeader(200) by now).
func TestJSONFormatReturnsTheErrorWhenItCannotAnswer(t *testing.T) {
	v := Data[float64]{Data: math.Inf(1)}
	var plain bytes.Buffer
	if err := JSONFormat(nil).Marshal(&plain, v); err == nil || plain.Len() != 0 {
		t.Fatalf("plain writer: err %v, wrote %q; want the error and nothing written", err, plain.String())
	}
	for name, rec := range map[string]*httptest.ResponseRecorder{"written": httptest.NewRecorder(), "net/http": httptest.NewRecorder()} {
		rec.WriteHeader(http.StatusOK)
		var w io.Writer = rec
		if name == "written" {
			w = writtenWriter{rec}
		}
		if err := JSONFormat(func(string, error) { t.Errorf("%s: reported a failure it did not answer", name) }).Marshal(w, v); err == nil || rec.Body.Len() != 0 || rec.Code != http.StatusOK {
			t.Fatalf("%s writer: err %v, status %d, wrote %q; want the error and nothing written", name, err, rec.Code, rec.Body.String())
		}
	}

	// A writer that says it has sent nothing but keeps Huma's 200 must not
	// get an error envelope under that 200.
	held := &holdsFirstStatus{ResponseRecorder: httptest.NewRecorder()}
	held.WriteHeader(http.StatusOK)
	if err := JSONFormat(func(string, error) { t.Error("reported a failure it did not answer") }).Marshal(held, v); err == nil || held.Body.Len() != 0 {
		t.Fatalf("status-holding writer: err %v, wrote %q; want the error and nothing written", err, held.Body.String())
	}
}

type failOnWrite struct{ t *testing.T }

func (f failOnWrite) Write(b []byte) (int, error) {
	f.t.Errorf("encoding/json wrote %d bytes of a value it could not encode", len(b))
	return len(b), nil
}

// The invariant the fix relies on: encoding/json encodes the whole value
// before its single Write, so a value it cannot encode writes nothing, however
// large and wherever the bad field sits.
func TestEncodingJSONWritesNothingOnFailure(t *testing.T) {
	big := make(map[string]float64, 20000)
	for i := range 20000 {
		big[fmt.Sprintf("k%05d", i)] = float64(i)
	}
	big["k99999"] = math.NaN()
	for name, v := range map[string]any{
		"+Inf":       math.Inf(1),
		"NaN":        math.NaN(),
		"year 10000": time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC),
		"last item":  []float64{1, 2, math.Inf(-1)},
		"1 MiB map":  big,
	} {
		if err := json.NewEncoder(failOnWrite{t}).Encode(v); err == nil {
			t.Errorf("%s: encoded", name)
		}
	}
}

// The 500 replaces the body the handler meant to send, so headers describing
// that body go, and it must not be cached; others, like Set-Cookie, stay.
func TestJSONFormatFailureDropsTheIntendedBodysHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	h := rec.Header()
	h.Set("ETag", `"v1"`)
	h.Set("Last-Modified", "Mon, 01 Jan 2024 00:00:00 GMT")
	h.Set("Location", "/api/v1/invoices/1")
	h.Set("Content-Length", "123")
	h.Set("Content-Encoding", "gzip")
	h.Set("Content-Language", "pt-BR")
	h.Set("Content-Disposition", "attachment")
	h.Set("Cache-Control", "max-age=600")
	h.Set("Set-Cookie", "refresh=abc")
	if err := JSONFormat(func(string, error) {}).Marshal(newPending(rec), Data[float64]{Data: math.NaN()}); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"ETag", "Last-Modified", "Location", "Content-Length", "Content-Encoding", "Content-Language", "Content-Disposition"} {
		if got := rec.Header().Get(gone); got != "" {
			t.Errorf("%s = %q survived onto the 500", gone, got)
		}
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := rec.Header().Get("Set-Cookie"); got != "refresh=abc" {
		t.Errorf("Set-Cookie = %q, want it kept", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

// Without a reporter the failure is still logged, through slog's default
// logger: it is never dropped silently.
func TestJSONFormatWithoutAReporterLogsThroughSlog(t *testing.T) {
	var logged bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	rec := httptest.NewRecorder()
	rec.Header().Set(RequestIDHeader, "req-slog")
	if err := JSONFormat(nil).Marshal(newPending(rec), Data[float64]{Data: math.Inf(1)}); err != nil {
		t.Fatal(err)
	}
	if out := logged.String(); !strings.Contains(out, "response could not be encoded") || !strings.Contains(out, "req-slog") {
		t.Fatalf("slog output = %q, want the failure with its request ID", out)
	}
}

// JSONFormats replaces only the JSON entries of huma.DefaultFormats: a format
// registered there (CBOR, by importing huma/v2/formats/cbor) is kept.
func TestJSONFormatsKeepsOtherRegisteredFormats(t *testing.T) {
	extra := huma.Format{Marshal: func(io.Writer, any) error { return nil }}
	huma.DefaultFormats["application/x-test"] = extra
	t.Cleanup(func() { delete(huma.DefaultFormats, "application/x-test") })
	formats := JSONFormats(nil)
	if _, ok := formats["application/x-test"]; !ok {
		t.Fatal("a format registered in huma.DefaultFormats was dropped")
	}
	if len(formats) != len(huma.DefaultFormats) {
		t.Fatalf("formats = %d entries, huma.DefaultFormats %d", len(formats), len(huma.DefaultFormats))
	}
}

// A Gombit JSON response costs no more allocations than Huma's default, at any
// body size and with sizes changing between responses (an ordinary 100-row
// list page is ~100 KiB): the encoder is pooled and keeps no buffer.
func TestJSONFormatAllocations(t *testing.T) {
	row := map[string]any{"id": 1, "name": strings.Repeat("x", 900), "price": 1.5}
	small := Data[map[string]any]{Data: row}
	mid := Data[[]map[string]any]{Data: slices.Repeat([]map[string]any{row}, 40)}
	large := Data[[]map[string]any]{Data: slices.Repeat([]map[string]any{row}, 200)}
	for name, bodies := range map[string][]any{
		"small":       {small},
		"large":       {large},
		"alternating": {mid, large, small, large},
	} {
		f := JSONFormat(nil)
		i := 0
		gombit := testing.AllocsPerRun(100, func() { _ = f.Marshal(io.Discard, bodies[i%len(bodies)]); i++ })
		i = 0
		def := testing.AllocsPerRun(100, func() { _ = huma.DefaultJSONFormat.Marshal(io.Discard, bodies[i%len(bodies)]); i++ })
		if gombit > def {
			t.Errorf("%s bodies: %.2f allocs, Huma's default %.2f", name, gombit, def)
		}
	}
}

// The pooled encoder is not poisoned by a failure: the next response encodes.
func TestJSONFormatRecoversAfterAFailure(t *testing.T) {
	f := JSONFormat(nil)
	for range 3 {
		_ = f.Marshal(httptest.NewRecorder(), Data[float64]{Data: math.NaN()})
		var out bytes.Buffer
		if err := f.Marshal(&out, Data[int]{Data: 7}); err != nil || out.String() != "{\"data\":7}\n" {
			t.Fatalf("after a failure: %q, %v", out.String(), err)
		}
	}
}
