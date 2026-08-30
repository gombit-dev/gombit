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
// An unclosed skip element (for example `<script src=x>...`) discards the
// remainder of the string — fail-closed for truncated markup. Self-closing
// skip tags do not enter skip mode.
func stripHTML(s string) string {
	if !strings.ContainsAny(s, "<>") {
		return s
	}
	if !completeHTMLTag.MatchString(s) {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))
	tokenizer := html.NewTokenizer(strings.NewReader(s))
	skipDepth := 0
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			return b.String()
		case html.TextToken:
			if skipDepth == 0 {
				b.Write(tokenizer.Text())
			}
		case html.StartTagToken:
			name, _ := tokenizer.TagName()
			if _, skip := xssSkipElementContent[string(name)]; skip {
				skipDepth++
			}
		case html.SelfClosingTagToken:
			// Self-closing skip tags have no body; do not raise skipDepth.
		case html.EndTagToken:
			name, _ := tokenizer.TagName()
			if _, skip := xssSkipElementContent[string(name)]; skip && skipDepth > 0 {
				skipDepth--
			}
		}
	}
}
