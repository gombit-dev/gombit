package framework

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/gombit-dev/gombit/contract"
)

// abortNotFound answers a request for which no route exists with the D10 404
// envelope (issue #438), as enableMethodNotAllowed does for 405. Gin's default
// 404 is a text/plain "404 page not found", which an API client parsing the
// error body as JSON (the generated TypeScript client's unwrap) cannot read, and
// which carries no request_id to correlate the failure with.
func abortNotFound(c *gin.Context) {
	env := contract.WithContext(c.Request.Context(), contract.NotFound(http.StatusText(http.StatusNotFound)))
	c.AbortWithStatusJSON(env.GetStatus(), env)
}

// recoverWithEnvelope is the runtime stack's panic recovery. It is
// gin.CustomRecovery, so it logs the panic and its stack and handles a broken
// client connection exactly as gin.Recovery does, but it answers a recovered
// panic with the D10 500 envelope instead of an empty body with no
// Content-Type (issue #438). When the handler had already started its response
// before panicking, it only aborts: anything written now would be appended to
// that partial response.
func recoverWithEnvelope() gin.HandlerFunc {
	return gin.CustomRecovery(func(c *gin.Context, _ any) {
		if c.Writer.Written() {
			c.Abort()
			return
		}
		env := contract.WithContext(c.Request.Context(), contract.Internal(http.StatusText(http.StatusInternalServerError)))
		c.AbortWithStatusJSON(env.GetStatus(), env)
	})
}
