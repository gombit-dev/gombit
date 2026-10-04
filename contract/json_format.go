package contract

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/danielgtaylor/huma/v2"
)

// RequestIDHeader is the response header that carries the request ID; the
// framework's request-context middleware sets it on every response.
const RequestIDHeader = "X-Request-Id"

// encodeFailedMessage is the message of the 500 a response that cannot be
// encoded becomes.
const encodeFailedMessage = "The server could not encode the response."

// pooledEncoder is a JSON encoder bound to a writer it can swap per response,
// so a Gombit response costs no more allocations than Huma's default (which
// builds an encoder per response) and needs no buffer of its own.
type pooledEncoder struct {
	w   io.Writer
	enc *json.Encoder
}

func (p *pooledEncoder) Write(b []byte) (int, error) { return p.w.Write(b) }

var encoderPool = sync.Pool{New: func() any {
	pe := &pooledEncoder{}
	pe.enc = json.NewEncoder(pe)
	pe.enc.SetEscapeHTML(false)
	return pe
}}

// JSONFormat is Huma's JSON format, except that a body that cannot be encoded
// is answered as an error instead of being handed back to Huma (issue #442).
//
// Huma sets the status and then marshals into the response; when marshalling
// fails it writes the plain-text "error marshaling response" under that status
// and panics. A value encoding/json cannot encode (a time outside years
// 0..9999, a float holding NaN or ±Inf) therefore reached the client as HTTP
// 200, Content-Type application/json, with that plain-text body, on every
// endpoint and list page holding it.
//
// encoding/json encodes the whole value before its single Write, so on such a
// failure nothing has been written. When the writer can still change its
// status, which Gin's does until the first byte, the failure is answered with
// a 500 carrying the D10 internal envelope and the response's request ID, and
// report is called with that ID and the encoding error. The writer must prove
// it: it has to report Written() == false, and Status() == 500 once the 500 is
// set. Any other writer, such as a net/http one that may already have sent
// Huma's status, gets the error back to Huma as before.
//
// report may be nil, in which case the failure is logged with slog's default
// logger: it is never dropped silently.
func JSONFormat(report func(requestID string, err error)) huma.Format {
	if report == nil {
		report = func(requestID string, err error) {
			slog.Error("http: response could not be encoded", "request_id", requestID, "error", err)
		}
	}
	return huma.Format{
		Marshal: func(w io.Writer, v any) error {
			pe := encoderPool.Get().(*pooledEncoder)
			pe.w = w
			err := pe.enc.Encode(v)
			pe.w = nil
			if err != nil {
				// Not pooled again: an Encoder whose Write failed keeps
				// returning that error.
				return answerEncodeFailure(w, err, report)
			}
			encoderPool.Put(pe)
			return nil
		},
		Unmarshal: json.Unmarshal,
	}
}

// JSONFormats returns huma.DefaultFormats with its JSON entries replaced by
// JSONFormat. Any other format registered there (such as CBOR, by importing
// huma/v2/formats/cbor) is kept.
func JSONFormats(report func(requestID string, err error)) map[string]huma.Format {
	formats := make(map[string]huma.Format, len(huma.DefaultFormats))
	for name, format := range huma.DefaultFormats {
		formats[name] = format
	}
	format := JSONFormat(report)
	formats["application/json"] = format
	formats["json"] = format
	return formats
}

// validatorHeaders describe the body the handler meant to send without being
// Content-* headers; they go with the Content-* headers (bar Content-Type)
// from the 500 that replaces it. Set-Cookie and the like are kept.
var validatorHeaders = []string{"ETag", "Last-Modified", "Location"}

func answerEncodeFailure(w io.Writer, encodeErr error, report func(string, error)) error {
	rw, ok := w.(http.ResponseWriter)
	if !ok {
		return encodeErr
	}
	// Only a writer that proves it has sent nothing, and then proves the 500
	// took, can answer: a net/http writer may already have sent Huma's 200,
	// and a wrapper that holds the first WriteHeader keeps that 200.
	sw, ok := w.(interface {
		Written() bool
		Status() int
	})
	if !ok || sw.Written() {
		return encodeErr
	}
	rw.WriteHeader(http.StatusInternalServerError)
	if sw.Status() != http.StatusInternalServerError {
		return encodeErr
	}
	requestID := rw.Header().Get(RequestIDHeader)
	report(requestID, encodeErr)
	body, err := json.Marshal(Internal(encodeFailedMessage).WithRequestID(requestID))
	if err != nil {
		return encodeErr
	}
	header := rw.Header()
	for name := range header {
		if strings.HasPrefix(name, "Content-") && name != "Content-Type" {
			header.Del(name)
		}
	}
	for _, name := range validatorHeaders {
		header.Del(name)
	}
	header.Set("Content-Type", "application/json")
	header.Set("Cache-Control", "no-store")
	_, err = rw.Write(append(body, '\n'))
	return err
}
