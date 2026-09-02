package framework

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/gombit-dev/gombit/contract"
	"golang.org/x/net/html"
)

// xssPasswordField is exempt from sanitization (exact JSON/query key match,
// case-sensitive). Keys like "Password" or nested paths still get stripped.
const xssPasswordField = "password"

// maxJSONBodyBytes caps XSS JSON body buffering (issue #137). A first-class
// body-size middleware is still deferred (docs/router.md); this keeps
// sanitizeJSONBody from io.ReadAll-ing an attacker-controlled stream.
const maxJSONBodyBytes int64 = 8 << 20

// Elements whose text content must not reach handlers (matched to HTML
// sanitizer "strict" expectations: tags stripped, dangerous element bodies
// discarded).
var xssSkipElementContent = map[string]struct{}{
	"script":   {},
	"style":    {},
	"noscript": {},
	"iframe":   {},
	"object":   {},
	"embed":    {},
	"textarea": {},
}

// completeHTMLTag matches a real tag with a closing ">". The HTML tokenizer
// treats "<" + letter as a start tag even without ">", which silently
// truncates comparison text like "a<b". Incomplete angle brackets are left
// unchanged; complete tags still go through stripHTML.
var completeHTMLTag = regexp.MustCompile(`(?i)<\s*/?[a-z][a-z0-9:-]*(?:\s[^>]*)?\s*/?>`)

// xssMiddleware sanitizes HTML tags from request input before handlers run.
// JSON string values (POST/PUT/PATCH) and GET query values are stripped to
// plain text via golang.org/x/net/html. The exact key "password" is left
// unchanged. Non-JSON bodies (form/multipart) pass through unchanged.
//
// Re-encoding JSON may change key order and whitespace; callers that hash
// the raw body must hash the sanitized bytes handlers actually receive — which
// a webhook can't, since it signs the original bytes. exemptPaths (exact request
// paths, from framework.WithRawBodyPaths) therefore skip sanitization entirely,
// so their body reaches the handler unmodified for signature verification.
func xssMiddleware(exemptPaths ...string) gin.HandlerFunc {
	exempt := make(map[string]struct{}, len(exemptPaths))
	for _, path := range exemptPaths {
		if path = strings.TrimSpace(path); path != "" {
			exempt[path] = struct{}{}
		}
	}
	return func(c *gin.Context) {
		if _, ok := exempt[c.Request.URL.Path]; ok {
			c.Next()
			return
		}
		switch c.Request.Method {
		case http.MethodGet:
			sanitizeQuery(c)
		case http.MethodPost, http.MethodPut, http.MethodPatch:
			sanitizeJSONBody(c)
		}
		if c.IsAborted() {
			return
		}
		c.Next()
	}
}

func writeXSSError(c *gin.Context, env *contract.ErrorEnvelope) {
	env = contract.WithContext(c.Request.Context(), env)
	if env == nil {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	c.Abort()
	c.Header("Content-Type", "application/json")
	c.Status(env.GetStatus())
	_ = json.NewEncoder(c.Writer).Encode(env)
}

func sanitizeQuery(c *gin.Context) {
	// No query string: skip the url.ParseQuery allocation entirely. The common
	// case for API/admin traffic and every bodyless GET without parameters.
	if c.Request.URL.RawQuery == "" {
		return
	}
	query := c.Request.URL.Query()
	changed := false
	for key, values := range query {
		if key == xssPasswordField {
			continue
		}
		for i, value := range values {
			cleaned := stripHTML(value)
			if cleaned != value {
				values[i] = cleaned
				changed = true
			}
		}
		query[key] = values
	}
	if changed {
		c.Request.URL.RawQuery = query.Encode()
	}
}

func sanitizeJSONBody(c *gin.Context) {
	if c.Request.Body == nil || c.Request.Body == http.NoBody {
		return
	}
	if !isJSONContentType(c.Request.Header.Get("Content-Type")) {
		return
	}

	limited := io.LimitReader(c.Request.Body, maxJSONBodyBytes+1)
	raw, err := io.ReadAll(limited)
	_ = c.Request.Body.Close()
	if err != nil {
		// Leave an empty body so Gin/Huma can emit a normal D10 validation
		// error. Aborting with a bare 400 would skip the envelope (D10).
		c.Request.Body = io.NopCloser(bytes.NewReader(nil))
		return
	}
	if int64(len(raw)) > maxJSONBodyBytes {
		c.Request.Body = io.NopCloser(bytes.NewReader(nil))
		writeXSSError(c, contract.PayloadTooLarge("JSON body exceeds the 8MiB sanitizer buffer"))
		return
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		c.Request.Body = io.NopCloser(bytes.NewReader(raw))
		return
	}

	// Fast path (issue #241): an HTML tag can only reach a decoded JSON string
	// as a literal '<'/'>' or as a "\uXXXX" escape, and every JSON escape needs
	// a backslash. A body containing none of '<', '>', or '\' therefore cannot
	// carry a tag in any string value or key, so sanitization is a guaranteed
	// no-op. Skip the decode + re-encode round trip entirely and hand the
	// original bytes to the handler unchanged. This is the common case for API
	// traffic. ContainsAny scans the (up to 8 MiB) body once; the escaped-bypass
	// reasoning — why '\' must be in the set — is locked by
	// TestSanitizeJSONBodyFastPathDoesNotBypassEscapedTags.
	if !bytes.ContainsAny(raw, "<>\\") {
		c.Request.Body = io.NopCloser(bytes.NewReader(raw))
		return
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var payload any
	if err := decoder.Decode(&payload); err != nil {
		// Leave the original body for Gin/Huma validation errors.
		c.Request.Body = io.NopCloser(bytes.NewReader(raw))
		return
	}

	// Only re-encode when a value actually changed. When sanitization stripped
	// nothing (the body reached here only because of a backslash or an
	// angle-bracket that turned out not to form a complete tag), the original
	// bytes go through untouched — no json.Marshal, and no JSON normalization
	// of a body we did not modify.
	if !sanitizeJSONValue(payload, "") {
		c.Request.Body = io.NopCloser(bytes.NewReader(raw))
		return
	}
	cleaned, err := json.Marshal(payload)
	if err != nil {
		c.Request.Body = io.NopCloser(bytes.NewReader(raw))
		return
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(cleaned))
	c.Request.ContentLength = int64(len(cleaned))
	c.Request.Header.Set("Content-Length", strconv.FormatInt(int64(len(cleaned)), 10))
}

func isJSONContentType(contentType string) bool {
	if contentType == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "application/json")
	}
	return strings.EqualFold(mediaType, "application/json")
}

// sanitizeJSONValue strips HTML from every string in value in place and reports
// whether it changed anything. The changed flag lets the caller skip a
// json.Marshal re-encode when the body carried no strippable markup (issue
// #241).
func sanitizeJSONValue(value any, fieldName string) bool {
	changed := false
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == xssPasswordField {
				continue
			}
			switch childValue := child.(type) {
			case string:
				if cleaned := stripHTML(childValue); cleaned != childValue {
					typed[key] = cleaned
					changed = true
				}
			default:
				if sanitizeJSONValue(child, key) {
					changed = true
				}
			}
		}
	case []any:
		for i, child := range typed {
			switch childValue := child.(type) {
			case string:
				if fieldName == xssPasswordField {
					continue
				}
				if cleaned := stripHTML(childValue); cleaned != childValue {
					typed[i] = cleaned
					changed = true
				}
			default:
				if sanitizeJSONValue(child, fieldName) {
					changed = true
				}
			}
		}
	}
	return changed
}

// stripHTML removes HTML tags and discards content inside dangerous elements.
// Plain strings without "<" or ">" are returned unchanged. Strings that
// contain "<" / ">" but no complete tag (no closing ">", e.g. "a<b") are
// also returned unchanged so comparison text is not truncated.
//
// An unclosed skip element (for example `<script src=x>...`) strips the tag
// itself but keeps the following text (issue #201). Text seen while
// skipDepth > 0 is buffered into skipBuf rather than dropped. skipStarts is
// a stack of checkpoints into skipBuf, one per currently-open skip element:
// pushed with the length of skipBuf at the moment that element opened,
// popped and truncated back to on its matching close. That discards exactly
// the content opened-and-closed by that one element, however deep it is
// nested inside other still-open skip elements — a single shared skipDepth
// counter is not enough, since script/style/textarea/noscript/iframe are
// HTML "raw text" elements (see below) and can never really nest, but
// object/embed are ordinary elements and can: for `<object><object>x
// </object>y`, the inner </object> must discard "x" on its own, without
// waiting for (or depending on) the outer <object> ever closing too.
//
// Whatever remains in skipBuf when the tokenizer runs out of input
// (skipDepth still > 0 at ErrorToken, nothing left open ever closed) is
// flushed to the output — re-tokenized from scratch, see below. Self-closing
// skip tags do not enter skip mode.
//
// Re-tokenizing applies the same skip logic again, so a nested dangerous
// element that finds its own closing tag inside the buffer is still
// discarded — only text that is not inside any closed dangerous element,
// anywhere in the chain of unclosed wrappers, survives to the output.
//
// The flush re-tokenizes rather than emitting the buffer verbatim, and it
// must do so with the real tokenizer, not a regexp: script/style/textarea/
// noscript/iframe are HTML "raw text" elements, so golang.org/x/net/html
// scans them for a literal closing tag and, finding none, hands back
// everything up to EOF as one opaque, never-tokenized TextToken — it can
// still contain complete, attribute-quoted tags that were never parsed. A
// naive regexp strip (completeHTMLTag) does not track quoting, so a `>`
// inside a quoted attribute value (`onerror="if(1>0){...}"`) ends the match
// early and leaks the rest of the tag as text — a classic tag-stripper
// bypass. Re-tokenizing lets the real HTML tokenizer parse the attribute
// correctly instead.
//
// maxUnclosedSkipRecursion bounds that re-tokenizing to a fixed number of
// rounds so a pathological chain of unclosed raw-text tags
// (`<script><script>...`) cannot force unbounded recursive re-tokenizing of
// a string shrinking by one tag per round — that would be quadratic in
// input size. Past the cap, the remaining tail reverts to the pre-#201
// fail-closed default (discarded) instead of being trusted as plain text;
// legitimate content practically never nests this deep.
func stripHTML(s string) string {
	return stripHTMLUnclosed(s, maxUnclosedSkipRecursion)
}

const maxUnclosedSkipRecursion = 4

func stripHTMLUnclosed(s string, budget int) string {
	if !strings.ContainsAny(s, "<>") {
		return s
	}
	if !completeHTMLTag.MatchString(s) {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))
	var skipBuf []byte
	var skipStarts []int
	tokenizer := html.NewTokenizer(strings.NewReader(s))
	skipDepth := 0
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			if skipDepth > 0 && budget > 0 {
				b.WriteString(stripHTMLUnclosed(string(skipBuf), budget-1))
			}
			return b.String()
		case html.TextToken:
			if skipDepth == 0 {
				b.Write(tokenizer.Text())
			} else {
				skipBuf = append(skipBuf, tokenizer.Text()...)
			}
		case html.StartTagToken:
			name, _ := tokenizer.TagName()
			if _, skip := xssSkipElementContent[string(name)]; skip {
				skipDepth++
				skipStarts = append(skipStarts, len(skipBuf))
			}
		case html.SelfClosingTagToken:
			// Self-closing skip tags have no body; do not raise skipDepth.
		case html.EndTagToken:
			name, _ := tokenizer.TagName()
			if _, skip := xssSkipElementContent[string(name)]; skip && skipDepth > 0 {
				skipDepth--
				start := skipStarts[len(skipStarts)-1]
				skipStarts = skipStarts[:len(skipStarts)-1]
				skipBuf = skipBuf[:start]
			}
		}
	}
}
