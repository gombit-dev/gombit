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

// jsonMediaType is the only media type the sanitizer decodes. Its length
// doubles as the fast-path prefix window below.
const jsonMediaType = "application/json"

// isJSONContentType reports whether contentType names the JSON media type.
//
// mime.ParseMediaType is not free: it allocates its parameter map
// unconditionally on the success path (mime/mediatype.go), so even the exact
// string "application/json" costs one alloc on every POST/PUT/PATCH carrying a
// body. The fast path below skips that parse for the two shapes that carry
// essentially all real traffic (issue #269).
//
// The fast path never returns false, and it returns true only where the media
// type is unambiguously "application/json": the whole string, or everything
// before the ';' that ends the subtype. Everything else is delegated to
// isJSONContentTypeSlow, which stays the only place a content type is actually
// decided — a longer subtype ("application/jsonx"), a structured suffix
// ("application/vnd.api+json"), a cased spelling with no parameters
// ("Application/JSON", accepted there via ToLower), leading whitespace, a tab
// delimiter, an unparseable tail ("application/json bogus"), and the empty
// string. FuzzIsJSONContentType asserts that the two never disagree.
func isJSONContentType(contentType string) bool {
	if contentType == jsonMediaType {
		return true
	}
	// Only ';' is accepted as the delimiter. It is the one byte that ends the
	// subtype under RFC 2045 grammar, so "application/json" before it is the
	// media type no matter how the parameters that follow are spelled — even
	// malformed ones, which the slow path still accepts. A space does not carry
	// that guarantee: "application/json bogus" fails the media-type parse
	// outright and survives only through the prefix fallback below, so
	// accelerating it would make this path depend on that fallback's exact
	// looseness. It is left to the slow path deliberately.
	if len(contentType) > len(jsonMediaType) &&
		contentType[len(jsonMediaType)] == ';' &&
		strings.EqualFold(contentType[:len(jsonMediaType)], jsonMediaType) {
		return true
	}
	return isJSONContentTypeSlow(contentType)
}

// isJSONContentTypeSlow is the full-parse decision, unchanged from before the
// fast path landed: it stays the single place a content type is actually
// decided.
func isJSONContentTypeSlow(contentType string) bool {
	if contentType == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		// A malformed parameter ("application/json; charset", no "=") fails the
		// parse even though the media type itself is fine, so fall back to a
		// prefix match and still sanitize those bodies. Deliberately fail-safe:
		// an over-eager yes only costs a decode, and an undecodable body is
		// handed to the handler untouched (sanitizeJSONBody above).
		return strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), jsonMediaType)
	}
	return strings.EqualFold(mediaType, jsonMediaType)
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

// stripHTML removes HTML tags and discards the content of dangerous elements
// (xssSkipElementContent). Strings holding no "<" or ">" are returned
// unchanged, and so are strings that hold them without forming a complete tag
// ("a<b"): golang.org/x/net/html reads `"<" + letter` as a start tag and would
// silently eat the rest of the string (issue #118).
//
// An unclosed dangerous element strips the tag but keeps the text that follows
// (issue #201); stripHTMLUnclosed does that work.
func stripHTML(s string) string {
	if !strings.ContainsAny(s, "<>") {
		return s
	}
	// The #118 gate covers the string the user actually submitted, and only
	// that one — stripHTMLUnclosed explains why recovered skip content must
	// never be admitted by it.
	if !completeHTMLTag.MatchString(s) {
		return s
	}
	return stripHTMLUnclosed(s, maxUnclosedSkipRecursion)
}

const maxUnclosedSkipRecursion = 4

// stripHTMLUnclosed tokenizes s, buffering the text of open skip elements so
// that one which never closes can flush that text instead of dropping it
// (issue #201), and re-tokenizes what it flushes, at most budget times.
//
// Text seen while skipDepth > 0 goes to skipBuf. skipStarts is a stack of
// offsets into skipBuf, one per open skip element, each recording skipBuf's
// length when that element opened; the element's matching close pops the
// offset and truncates back to it, discarding exactly that element's content
// however deep it sits inside other still-open skip elements. A single shared
// depth counter is not enough: script/style/textarea/noscript/iframe are HTML
// "raw text" elements and can never really nest, but object and embed are
// ordinary elements and can — in `<object><object>x</object>y`, the inner
// close must discard "x" whether or not the outer one ever closes.
//
// Whatever is still buffered at EOF goes back through this function rather
// than being written out verbatim. It has to: a raw-text element missing its
// closing tag makes the tokenizer hand back everything up to EOF as one
// opaque, never-parsed TextToken, so the buffer can still hold complete tags.
// Re-tokenizing applies the same skip logic to them, so a dangerous element
// that does find its close inside the buffer is still discarded.
//
// That second pass must use the tokenizer, never completeHTMLTag. The regexp
// is the #118 gate for user-typed comparison text, not a stripper; as an
// admission gate it would emit live every tag it fails to recognize, and it
// misses two classes the tokenizer handles — an attribute not preceded by
// whitespace (`<script/src=x>`, where HTML5 reconsumes the "/" and reads
// `<script src=x>`), and a ">" inside a quoted attribute value
// (`onerror="if(1>0){...}"`), which ends the match early and leaks the rest of
// the tag as text. Anything still holding "<" or ">" is therefore
// re-tokenized unconditionally, which costs the #118 protection inside
// recovered text: every `"<" + letter` through the next ">" goes, so
// `<script>if (a<b && c>d) return` yields "if (ad) return". That is markup
// smuggled through a raw-text element, not the comparison text #118 covers.
//
// budget bounds the re-tokenizing: each unclosed raw-text tag buys one more
// round on a string only one tag shorter, so an unbounded chain
// (`<script><script>...`) would be quadratic in input size. Past the cap the
// tail reverts to the pre-#201 default and is discarded; legitimate content
// does not nest this deep.
func stripHTMLUnclosed(s string, budget int) string {
	if !strings.ContainsAny(s, "<>") {
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
