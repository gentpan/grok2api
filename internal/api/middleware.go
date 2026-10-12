package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// bodyLimitMiddleware rejects request bodies larger than max bytes.
func bodyLimitMiddleware(max int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, max)
		}
		c.Next()
	}
}

// configReloadMiddleware re-checks the config files on every request.
//
// TODO(integrate): replace with config.Subscribe-driven reload; the stat per
// request goes away then.
func (s *Server) configReloadMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		_ = s.config().Load()
		c.Next()
	}
}

// corsMiddleware adds permissive CORS headers.
func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "*")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}
