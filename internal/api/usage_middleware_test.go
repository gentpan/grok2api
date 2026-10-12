package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestUsageMiddlewareCountsOnlyModelledPosts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := newUsageRecorder(filepath.Join(t.TempDir(), "usage.json"))
	s := &Server{Usage: rec}
	e := gin.New()
	e.Use(s.usageMiddleware())
	e.POST("/ok", func(c *gin.Context) { noteUsageModel(c, "grok-x"); c.Status(http.StatusOK) })
	e.POST("/bad", func(c *gin.Context) { noteUsageModel(c, "grok-x"); c.Status(http.StatusBadGateway) })
	e.POST("/nomodel", func(c *gin.Context) { c.Status(http.StatusBadRequest) })
	e.GET("/get", func(c *gin.Context) { noteUsageModel(c, "grok-x"); c.Status(http.StatusOK) })

	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodPost, "/ok", nil),
		httptest.NewRequest(http.MethodPost, "/bad", nil),
		httptest.NewRequest(http.MethodPost, "/nomodel", nil),
		httptest.NewRequest(http.MethodGet, "/get", nil),
	} {
		e.ServeHTTP(httptest.NewRecorder(), req)
	}
	tot := rec.snapshot(1)["totals"].(gin.H)
	if tot["requests"] != 2 || tot["errors"] != 1 {
		t.Fatalf("totals = %v, want 2 requests / 1 error", tot)
	}
}
