package framework

import (
	"bytes"
	"encoding/json"
	"html"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

const apiPrefixPlaceholder = "__GOMBIT_API_PREFIX__"

// mountAdminSPA registers explicit GET/HEAD routes for /admin and
// /admin/*filepath. Those win over Huma and over WithEmbeddedFrontend
// NoRoute. If fsys has no index.html, this is a no-op (same as M5-5).
func mountAdminSPA(router *gin.Engine, fsys fs.FS, apiPrefix string, storageOrigins ...string) {
	if router == nil || fsys == nil {
		return
	}
	if !hasIndexHTML(fsys) {
		return
	}
	handler := adminSPAHandler(fsys, apiPrefix, spaCSP(storageOrigins))
	router.GET("/admin", handler)
	router.HEAD("/admin", handler)
	router.GET("/admin/*filepath", handler)
	router.HEAD("/admin/*filepath", handler)
}

func adminSPAHandler(fsys fs.FS, apiPrefix string, csp []string) gin.HandlerFunc {
	prefix := normalizeAPIPrefix(apiPrefix)
	return func(c *gin.Context) {
		if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
			abortNotFound(c)
			return
		}

		urlPath := path.Clean("/" + c.Request.URL.Path)
		if urlPath != "/admin" && !strings.HasPrefix(urlPath, "/admin/") {
			abortNotFound(c)
			return
		}

		rel := strings.TrimPrefix(urlPath, "/admin")
		rel = strings.TrimPrefix(rel, "/")

		if rel == "config.json" {
			serveAdminRuntimeConfig(c, prefix)
			return
		}
		if rel == "" || rel == "." || rel == "index.html" {
			serveAdminIndexHTML(c, fsys, prefix, csp)
			return
		}
		if fs.ValidPath(rel) {
			if serveEmbeddedFile(c, fsys, rel, csp) {
				return
			}
		}
		serveAdminIndexHTML(c, fsys, prefix, csp)
	}
}

func normalizeAPIPrefix(apiPrefix string) string {
	prefix := strings.TrimSuffix(strings.TrimSpace(apiPrefix), "/")
	if prefix == "" {
		return "/api/v1"
	}
	return prefix
}

func serveAdminRuntimeConfig(c *gin.Context, apiPrefix string) {
	body, err := json.Marshal(map[string]string{"api_prefix": apiPrefix})
	if err != nil {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	writeBytes(c, "application/json; charset=utf-8", body)
}

func serveAdminIndexHTML(c *gin.Context, fsys fs.FS, apiPrefix string, csp []string) {
	data, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		abortNotFound(c)
		return
	}
	data = injectAPIPrefixHTML(data, apiPrefix)
	applyBrowserSecurityHeaders(c, csp)
	writeBytes(c, "text/html; charset=utf-8", data)
}

func injectAPIPrefixHTML(data []byte, apiPrefix string) []byte {
	escaped := html.EscapeString(normalizeAPIPrefix(apiPrefix))
	return bytes.ReplaceAll(data, []byte(apiPrefixPlaceholder), []byte(escaped))
}

func writeBytes(c *gin.Context, contentType string, data []byte) {
	c.Header("Content-Type", contentType)
	c.Header("Content-Length", strconv.Itoa(len(data)))
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}
	c.Data(http.StatusOK, contentType, data)
}
