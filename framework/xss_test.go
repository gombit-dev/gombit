package framework

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gombit-dev/gombit/contract"
)

func TestStripHTML(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain", in: "hello", want: "hello"},
		{name: "script discarded", in: `<script>alert(1)</script>hi`, want: "hi"},
		{name: "bold stripped", in: `<b>hi</b>`, want: "hi"},
		{name: "img stripped", in: `x<img src=x onerror=alert(0)>y`, want: "xy"},
		{name: "unclosed script keeps trailing text", in: `<script src=x>alert(1)`, want: "alert(1)"},
		{name: "unclosed script keeps surrounding text", in: `hi<script>bye`, want: "hibye"},
		{name: "unclosed textarea after closed script", in: `<script>a</script>b<textarea>c`, want: "bc"},
		// The inner <textarea> finds its own matching close tag inside the
		// buffered raw text, so re-tokenizing discards its content exactly
		// like the top-level pass would — properly closed dangerous content
		// stays hidden even when it is nested inside a wrapper that never
		// closes at all.
		{name: "nested skip: inner closes, its content is still discarded", in: `<script><textarea>x</textarea>y`, want: "y"},
		{name: "nested skip, outer closes, all content discarded", in: `<script><textarea>x</textarea>y</script>z`, want: "z"},
		{name: "doubled unclosed skip of same tag", in: `<script><script>x`, want: "x"},
		{name: "stray end tag closes the real open, extra one is a no-op", in: `<textarea>a</textarea></textarea>b`, want: "b"},
		{name: "self-closing skip tag does not enter skip mode", in: `<script/>after`, want: "after"},
		{name: "empty closed skip element", in: `<script></script>`, want: ""},
		// script/style/textarea/noscript/iframe are HTML "raw text" elements:
		// golang.org/x/net/html scans them for a literal closing tag and, when
		// the wrapper never closes, hands back everything up to EOF as one
		// opaque, never-tokenized TextToken — so a payload smuggled inside an
		// unclosed wrapper must be stripped again on the way out, not trusted
		// as already-safe plain text. Here the inner <script> also finds its
		// own close tag, so (like the case above) its content is discarded —
		// no live tag AND no leftover script body reach the output.
		{name: "unclosed wrapper: inner script that does close is still fully discarded", in: `<textarea><script>alert(1)</script>`, want: ""},
		// Neither wrapper closes this time, so there is no "properly closed
		// dangerous content" to discard — the plain text at the very center
		// is legitimately safe and must survive, with no tag syntax at all
		// (not even a partial "<script>") leaking out around it.
		{name: "doubly unclosed wrapper still recovers the innermost plain text", in: `<textarea><script>alert(1)`, want: "alert(1)"},
		{name: "unclosed wrapper cannot smuggle a live img onerror tag", in: `<noscript>x<img src=x onerror=alert(1)>y`, want: "xy"},
		// A quoted attribute value may itself contain a literal ">". A regexp
		// strip that isn't quote-aware ends its match at that inner ">" and
		// leaks the rest of the tag (including the closing ">") as text — the
		// classic tag-stripper bypass. stripHTML must re-tokenize with the
		// real (quote-aware) HTML tokenizer, not a regexp, so the whole <img>
		// start tag is recognized and dropped instead of partially surviving.
		{name: "unclosed wrapper: quoted attribute containing > does not defeat the strip", in: `<noscript><img src=x onerror="if(1>0){alert(document.cookie)}">`, want: ""},
		// object/embed are ordinary elements, not HTML "raw text" elements
		// like script/style/textarea/noscript/iframe, so — unlike those —
		// they can genuinely nest within a single tokenizer pass. A single
		// shared skipDepth counter is not enough to get this right: the
		// inner </object> must discard its own "x" independently of whether
		// the outer <object> ever closes, so a nested-but-properly-closed
		// dangerous element cannot smuggle its content out just because an
		// ancestor happens to stay open.
		{name: "nested object, inner closes, outer closes: all discarded", in: `<object><object>x</object>y</object>z`, want: "z"},
		{name: "nested object, inner closes, outer never closes: inner content still discarded", in: `<object><object>x</object>y`, want: "y"},
		// Each unclosed raw-text wrapper forces one more re-tokenizing round
		// on a string that is only one tag shorter, so re-tokenizing without
		// a bound would be quadratic in a chain this deep. Past
		// maxUnclosedSkipRecursion (4), stripHTML falls back to discarding
		// the remainder rather than recursing further — verified fast in
		// isolation; asserted here only for correctness of the fallback.
		{name: "unclosed chain within the recursion budget still recovers text", in: `<script><script><script><script>x`, want: "x"},
		{name: "unclosed chain past the recursion budget falls back to discard", in: `<script><script><script><script><script><script>x`, want: ""},
		{name: "nested markup", in: `<div><b>a</b><i>b</i></div>`, want: "ab"},
		// Incomplete "<"+letter is not a tag (golang.org/x/net/html would
		// otherwise eat the rest of the string: "a<b" → "a").
		{name: "comparison a<b", in: "a<b", want: "a<b"},
		{name: "comparison a<b>c without real tag name", in: "a < b", want: "a < b"},
		{name: "less-than number", in: "score < 10", want: "score < 10"},
		{name: "greater-than only", in: "a>b", want: "a>b"},
		{name: "complete tag still stripped", in: "foo <bar> baz", want: "foo  baz"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := stripHTML(tt.in); got != tt.want {
				t.Fatalf("stripHTML(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSanitizeJSONValueNestedArray(t *testing.T) {
	t.Parallel()

	payload := map[string]any{
		"items": []any{
			map[string]any{"name": `<b>one</b>`},
			`<script>x</script>two`,
		},
	}
	sanitizeJSONValue(payload, "")

	items, ok := payload["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("items = %#v", payload["items"])
	}
	first, ok := items[0].(map[string]any)
	if !ok || first["name"] != "one" {
		t.Fatalf("items[0] = %#v, want name=one", items[0])
	}
	if items[1] != "two" {
		t.Fatalf("items[1] = %#v, want two", items[1])
	}
}

func TestSanitizeJSONBodyInvalidJSONPassthrough(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"comment":`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw))
	c.Request.Header.Set("Content-Type", "application/json")

	sanitizeJSONBody(c)

	got, err := io.ReadAll(c.Request.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("body = %q, want original invalid JSON %q", got, raw)
	}
}

func TestSanitizeJSONBodyEmptyBodyNoop(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader("   "))
	c.Request.Header.Set("Content-Type", "application/json")

	sanitizeJSONBody(c)

	got, err := io.ReadAll(c.Request.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(got) != "   " {
		t.Fatalf("body = %q, want unchanged whitespace", got)
	}
}

func TestSanitizeJSONBodyRejectsOversized(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := bytes.Repeat([]byte("x"), int(maxJSONBodyBytes)+1)
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	sanitizeJSONBody(c)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
	if !c.IsAborted() {
		t.Fatal("expected XSS sanitizer to abort an oversized JSON body")
	}
	assertXSSPayloadTooLarge(t, rec)
}

type infiniteJSONReader struct{}

func (infiniteJSONReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestSanitizeJSONBodyDoesNotBlockOnInfiniteBody(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", infiniteJSONReader{})
	c.Request.Header.Set("Content-Type", "application/json")

	done := make(chan struct{})
	go func() {
		sanitizeJSONBody(c)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sanitizeJSONBody blocked on unbounded ReadAll")
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
	if !c.IsAborted() {
		t.Fatal("expected XSS sanitizer to abort an unbounded JSON body")
	}
	assertXSSPayloadTooLarge(t, rec)
}

type errJSONReader struct{}

func (errJSONReader) Read([]byte) (int, error) {
	return 0, io.ErrUnexpectedEOF
}

func TestSanitizeJSONBodyReadErrorLeavesEmptyBody(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", errJSONReader{})
	c.Request.Header.Set("Content-Type", "application/json")

	sanitizeJSONBody(c)

	if c.IsAborted() {
		t.Fatal("read error should not abort; Huma/Gin emit D10 for the empty body")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want recorder default %d (no abort)", rec.Code, http.StatusOK)
	}
	got, err := io.ReadAll(c.Request.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("body = %q, want empty so handlers can reject it", got)
	}
}

func assertXSSPayloadTooLarge(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	var env contract.ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode D10 envelope: %v; body: %s", err, rec.Body.String())
	}
	if env.Body.Code != contract.CodePayloadTooLarge {
		t.Fatalf("error.code = %q, want %q; body: %s", env.Body.Code, contract.CodePayloadTooLarge, rec.Body.String())
	}
	if env.Body.Message == "" {
		t.Fatalf("error.message empty; body: %s", rec.Body.String())
	}
}

func TestSanitizeJSONBodyUpdatesContentLengthHeader(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"comment":"<b>hi</b>"}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("Content-Length", "999")

	sanitizeJSONBody(c)

	got, err := io.ReadAll(c.Request.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var payload map[string]string
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatalf("unmarshal sanitized body: %v (%s)", err, got)
	}
	if payload["comment"] != "hi" {
		t.Fatalf("comment = %q, want hi", payload["comment"])
	}
	if c.Request.ContentLength != int64(len(got)) {
		t.Fatalf("ContentLength = %d, want %d", c.Request.ContentLength, len(got))
	}
	if gotHeader := c.Request.Header.Get("Content-Length"); gotHeader != strconv.Itoa(len(got)) {
		t.Fatalf("Content-Length header = %q, want %d", gotHeader, len(got))
	}
}

func TestSanitizeJSONBodyPreservesComparisonText(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"name":"a<b","price":1}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw))
	c.Request.Header.Set("Content-Type", "application/json")

	sanitizeJSONBody(c)

	got, err := io.ReadAll(c.Request.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatalf("unmarshal sanitized body: %v (%s)", err, got)
	}
	if payload["name"] != "a<b" {
		t.Fatalf("name = %#v, want a<b (not truncated)", payload["name"])
	}
}

// TestSanitizeJSONBodyFastPathDoesNotBypassEscapedTags is the safety lock for
// the issue #241 fast path. A tag delivered through "\uXXXX" escapes contains a
// backslash in the raw bytes, so it must NOT take the no-'<'/'>'/'\\' fast path
// — it must be decoded and stripped like a literal tag. If the fast-path guard
// ever drops its backslash check, this test fails instead of silently opening
// an XSS bypass.
func TestSanitizeJSONBodyFastPathDoesNotBypassEscapedTags(t *testing.T) {
	t.Parallel()

	// No literal '<' or '>' anywhere; the tag is entirely \uXXXX-escaped.
	raw := []byte(`{"comment":"<script>alert(1)</script>"}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw))
	c.Request.Header.Set("Content-Type", "application/json")

	sanitizeJSONBody(c)

	got, err := io.ReadAll(c.Request.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var payload map[string]string
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatalf("unmarshal sanitized body: %v (%s)", err, got)
	}
	if strings.ContainsAny(payload["comment"], "<>") {
		t.Fatalf("comment = %q still contains tag markup — escaped-tag XSS bypass", payload["comment"])
	}
	// script element body is discarded by stripHTML (strict mode).
	if payload["comment"] != "" {
		t.Fatalf("comment = %q, want empty (script body discarded)", payload["comment"])
	}
}

// TestSanitizeJSONBodyCleanBodyPassesThroughVerbatim locks the fast path's
// other half: a body with no markup reaches the handler byte-for-byte, with no
// JSON re-encoding (issue #241). The Content-Length header the client sent is
// left untouched because the bytes are unchanged.
func TestSanitizeJSONBodyCleanBodyPassesThroughVerbatim(t *testing.T) {
	t.Parallel()

	// Deliberately non-normalized: spaces after colons, '&' in a value. If the
	// body were decoded and re-marshaled, these bytes would change.
	raw := []byte(`{"name": "Ada Lovelace", "note": "tea & cake"}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw))
	c.Request.Header.Set("Content-Type", "application/json")

	sanitizeJSONBody(c)

	got, err := io.ReadAll(c.Request.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("body = %q, want verbatim %q (clean body must not be re-encoded)", got, raw)
	}
}

func TestXSSMiddlewareRawBodyPathSkipsSanitization(t *testing.T) {
	// A body that json re-encoding would change (< / & escaped, <b> stripped).
	const raw = `{"tag":"v1","body":"<b>notes</b> a < b & c"}`

	newEngine := func() (*gin.Engine, *string) {
		var got string
		eng := gin.New()
		eng.Use(xssMiddleware("/api/v1/webhooks/github"))
		read := func(c *gin.Context) {
			b, _ := io.ReadAll(c.Request.Body)
			got = string(b)
			c.Status(http.StatusOK)
		}
		eng.POST("/api/v1/webhooks/github", read)
		eng.POST("/api/v1/other", read)
		return eng, &got
	}

	// Exempt raw-body path: the handler receives the original bytes verbatim.
	eng, got := newEngine()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", strings.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	eng.ServeHTTP(rec, req)
	if *got != raw {
		t.Fatalf("exempt-path body = %q, want unmodified %q", *got, raw)
	}

	// Non-exempt path: the body is sanitized/re-encoded (control).
	eng2, got2 := newEngine()
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/other", strings.NewReader(raw))
	req2.Header.Set("Content-Type", "application/json")
	eng2.ServeHTTP(rec2, req2)
	if *got2 == raw {
		t.Fatalf("non-exempt path body was not sanitized: %q", *got2)
	}
}
