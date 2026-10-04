package contract

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync"

	"github.com/danielgtaylor/huma/v2"
)

// RequestIDHeader is the response header that carries the request ID; the
// framework's request-context middleware sets it on every response.
const RequestIDHeader = "X-Request-Id"

// encodeFailedMessage is the message of the 500 a response that cannot be
// encoded becomes.
const encodeFailedMessage = "The server could not encode the response."

// pooledEncoder is a buffer and the encoder bound to it, reused across
// responses so encoding first costs no more allocations than Huma's default
// (which builds an encoder per response).
type pooledEncoder struct {
	buf bytes.Buffer
	enc *json.Encoder
}

var encoderPool = sync.Pool{New: func() any {
	pe := &pooledEncoder{}
	pe.enc = json.NewEncoder(&pe.buf)
	pe.enc.SetEscapeHTML(false)
	return pe
}}

// maxPooledEncodeBuf bounds the buffer an encoder keeps between responses. A
// larger body still reuses the encoder but drops the buffer afterwards, so it
// costs one buffer allocation; bodies up to this size cost none.
const maxPooledEncodeBuf = 64 << 10

// JSONFormat is Huma's JSON format with one difference: the body is encoded in
// full before anything is written (issue #442).
//
// Huma sets the status and then marshals straight into the response. A value
// encoding/json cannot encode (a time outside years 0..9999, a float holding
// NaN or ±Inf) used to fail after the status was decided, so the client got
// HTTP 200, Content-Type application/json, and the plain-text body "error
// marshaling response", and every list page holding such a row did the same.
//
// Encoding first means a failure is known before any body byte is sent. When
// the writer proves it has not written yet, by reporting Written() == false
// (Gin's does: it holds the status until the first byte), the failure is
// answered with a 500 carrying the D10 internal envelope and the response's
// request ID, and report is called with that ID and the encoding error. Any
// other writer, including a net/http one that may already have sent Huma's
// status, gets the error back to Huma as before.
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
			pe.buf.Reset()
			defer func() {
				if pe.buf.Cap() > maxPooledEncodeBuf {
					// enc writes to &pe.buf, so replacing the value keeps it valid.
					pe.buf = bytes.Buffer{}
				}
				encoderPool.Put(pe)
			}()
			if err := pe.enc.Encode(v); err != nil {
				return answerEncodeFailure(w, err, report)
			}
			_, err := w.Write(pe.buf.Bytes())
			return err
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

// representationHeaders describe the body the handler meant to send. They are
// removed from the 500 that replaces it; Set-Cookie and the like are kept.
var representationHeaders = []string{"Content-Length", "Content-Disposition", "ETag", "Last-Modified", "Location"}

func answerEncodeFailure(w io.Writer, encodeErr error, report func(string, error)) error {
	rw, ok := w.(http.ResponseWriter)
	if !ok {
		return encodeErr
	}
	// Only a writer that proves it has sent nothing can still change the
	// status; a net/http writer may already have sent Huma's 200.
	written, ok := w.(interface{ Written() bool })
	if !ok || written.Written() {
		return encodeErr
	}
	requestID := rw.Header().Get(RequestIDHeader)
	report(requestID, encodeErr)
	body, err := json.Marshal(Internal(encodeFailedMessage).WithRequestID(requestID))
	if err != nil {
		return encodeErr
	}
	header := rw.Header()
	for _, name := range representationHeaders {
		header.Del(name)
	}
	header.Set("Content-Type", "application/json")
	header.Set("Cache-Control", "no-store")
	rw.WriteHeader(http.StatusInternalServerError)
	_, err = rw.Write(append(body, '\n'))
	return err
}
