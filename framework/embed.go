package framework

import (
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/gin-gonic/gin"
)

// WithEmbeddedFrontend stores fsys and, when it contains index.html at its
// root, installs a Gin NoRoute handler after framework routes so unmatched
// GET paths serve the SPA. Huma /api/*, /openapi.json, /docs, and the
// probe routes still win because they are registered before NoRoute runs.
//
// If fsys has no index.html (the gombit new placeholder embed), NoRoute is
// not installed and unknown paths keep their current 404. Split deploy is
// the default (C5); embedding is opt-in via gombit build --embed.
func WithEmbeddedFrontend(fsys fs.FS) Option {
	return func(app *App) error {
		if fsys == nil {
			return errors.New("framework: nil embedded frontend")
		}
		app.embeddedFrontend = fsys
		return nil
	}
}

func mountEmbeddedFrontend(router *gin.Engine, fsys fs.FS, apiPrefix string, storageOrigins ...string) {
	if router == nil || fsys == nil {
		return
	}
	if !hasIndexHTML(fsys) {
		return
	}
	router.NoRoute(embeddedFrontendHandler(fsys, apiPrefix, spaCSP(storageOrigins)))
}

func hasIndexHTML(fsys fs.FS) bool {
	info, err := fs.Stat(fsys, "index.html")
	return err == nil && info != nil && !info.IsDir()
}

func embeddedFrontendHandler(fsys fs.FS, apiPrefix string, csp []string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}

		urlPath := path.Clean("/" + c.Request.URL.Path)
		if isReservedFrontendPath(urlPath, apiPrefix) {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}

		name := strings.TrimPrefix(urlPath, "/")
		if name == "index.html" {
			serveIndexHTML(c, fsys, apiPrefix, csp)
			return
		}
		if name != "" && name != "." && fs.ValidPath(name) {
			if serveEmbeddedFile(c, fsys, name, csp) {
				return
			}
		}

		// SPA fallback: only GET and HEAD reach this handler at all (the
		// guard above), and serveIndexHTML writes via writeBytes, which
		// already skips the body for HEAD — so no method check belongs here.
		serveIndexHTML(c, fsys, apiPrefix, csp)
	}
}

func isReservedFrontendPath(urlPath, apiPrefix string) bool {
	switch urlPath {
	case "/livez", "/readyz", "/metrics",
		"/openapi.json", "/openapi.yaml",
		"/openapi-3.0.json", "/openapi-3.0.yaml",
		"/docs":
		return true
	}
	if urlPath == "/api" || strings.HasPrefix(urlPath, "/api/") {
		return true
	}
	if urlPath == "/admin" || strings.HasPrefix(urlPath, "/admin/") {
		return true
	}
	if strings.HasPrefix(urlPath, "/docs/") {
		return true
	}
	prefix := path.Clean("/" + strings.TrimSpace(apiPrefix))
	if prefix != "/" && prefix != "." && prefix != "/api" {
		if urlPath == prefix || strings.HasPrefix(urlPath, prefix+"/") {
			return true
		}
	}
	return false
}

// spaContentSecurityPolicy is the Content-Security-Policy for HTML documents
// the framework serves through its own handlers — the embedded SPA index.html
// and the admin SPA. It is looser than the API default (which is
// default-src 'none'; frame-ancestors 'none') so --ui mui + --embed can load
// Roboto and Emotion-injected <style> tags. script-src stays 'self' (hashed
// Vite modules; no unsafe-inline scripts). JSON API and probe responses keep
// the strict API policy set in securityHeadersMiddleware.
const spaContentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com; img-src 'self' data:; connect-src 'self'"

var spaContentSecurityPolicyValue = []string{spaContentSecurityPolicy}

// spaCSP is the SPA Content-Security-Policy, with storageOrigins (where the
// store's URLs point, when that is another origin: an S3 bucket, a CDN)
// allowed for images and requests, so a page can show a stored image and
// upload directly to the store.
func spaCSP(storageOrigins []string) []string {
	if len(storageOrigins) == 0 {
		return spaContentSecurityPolicyValue
	}
	extra := " " + strings.Join(storageOrigins, " ")
	csp := strings.Replace(spaContentSecurityPolicy, "img-src 'self' data:", "img-src 'self' data:"+extra, 1)
	csp = strings.Replace(csp, "connect-src 'self'", "connect-src 'self'"+extra, 1)
	return []string{csp}
}

// applyBrowserSecurityHeaders promotes a response from the API/JSON security
// baseline (set by securityHeadersMiddleware) to the full browser policy that
// an HTML document rendered in a browser needs (issue #267 / PERF-9): the
// looser SPA Content-Security-Policy, plus Referrer-Policy and the legacy
// X-Frame-Options: DENY for user agents that do not honor frame-ancestors.
// X-Content-Type-Options and Strict-Transport-Security are already set by the
// middleware and carry through unchanged.
//
// It replaces map entries (never writing through the shared read-only slices),
// so the shared-value contract in securityHeadersMiddleware still holds. This
// is not on the JSON hot path, so the map growth it may cause past the 8-header
// threshold is acceptable here — the allocation budget in issue #267 is about
// the default API response, not HTML documents.
func applyBrowserSecurityHeaders(c *gin.Context, csp []string) {
	header := c.Writer.Header()
	header["Content-Security-Policy"] = csp
	header["Referrer-Policy"] = referrerPolicyValue
	header["X-Frame-Options"] = frameOptionsValue
}

func serveEmbeddedFile(c *gin.Context, fsys fs.FS, name string, csp []string) bool {
	f, err := fsys.Open(name)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil || info.IsDir() {
		return false
	}

	// The security policy follows the content type actually served, not a file
	// name (issue #434): every HTML document in the embedded build (a
	// multi-page Vite app's /about/index.html, a static /legal.html) is a page
	// rendered in a browser and needs the browser policy, or the API policy's
	// default-src 'none' blocks its scripts, styles and images. The type is
	// decided here, the way http.ServeContent would decide it, and set on the
	// response so ServeContent uses it unchanged.
	ctype := mime.TypeByExtension(path.Ext(name))

	if rs, ok := f.(io.ReadSeeker); ok {
		if ctype == "" {
			sniffed, err := sniffContentType(rs)
			if err != nil {
				return false
			}
			ctype = sniffed
		}
		if isHTMLContentType(ctype) {
			applyBrowserSecurityHeaders(c, csp)
		}
		c.Writer.Header().Set("Content-Type", ctype)
		http.ServeContent(c.Writer, c.Request, info.Name(), info.ModTime(), rs)
		return true
	}

	data, err := io.ReadAll(f)
	if err != nil {
		return false
	}
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	if isHTMLContentType(ctype) {
		applyBrowserSecurityHeaders(c, csp)
	}
	writeBytes(c, ctype, data)
	return true
}

// sniffContentType detects the type of a file with no recognised extension
// from its first 512 bytes, as http.ServeContent does, and rewinds it.
func sniffContentType(rs io.ReadSeeker) (string, error) {
	var buf [512]byte
	n, _ := io.ReadFull(rs, buf[:])
	if _, err := rs.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	return http.DetectContentType(buf[:n]), nil
}

// isHTMLContentType reports whether ctype is an HTML document type, the
// responses that need the browser security policy.
func isHTMLContentType(ctype string) bool {
	mediaType, _, err := mime.ParseMediaType(ctype)
	if err != nil {
		return false
	}
	return mediaType == "text/html" || mediaType == "application/xhtml+xml"
}

func serveIndexHTML(c *gin.Context, fsys fs.FS, apiPrefix string, csp []string) {
	data, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	data = injectAPIPrefixHTML(data, apiPrefix)
	applyBrowserSecurityHeaders(c, csp)
	writeBytes(c, "text/html; charset=utf-8", data)
}
