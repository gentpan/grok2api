package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/gentpan/grok2api/internal/config"
	"github.com/gentpan/grok2api/internal/platform"
)

// --- Config ---

func (s *Server) handleConfigGet(c *gin.Context) {
	c.JSON(http.StatusOK, s.config().Raw())
}

func (s *Server) handleConfigUpdate(c *gin.Context) {
	var patch map[string]any
	if err := readJSON(c, &patch); err != nil {
		writeAppError(c, err)
		return
	}
	if err := validatePatch(patch); err != nil {
		writeAppError(c, err)
		return
	}
	cfg := s.config()
	if err := cfg.Update(patch); err != nil {
		writeAppError(c, platform.UpstreamError("config update failed: "+err.Error(), 500, ""))
		return
	}
	_ = cfg.Load()
	strategy := s.applySelectionStrategy()
	c.JSON(http.StatusOK, gin.H{
		"status":             "success",
		"message":            "配置已更新",
		"selection_strategy": string(strategy),
	})
}

// validatePatch rejects startup-only config paths from runtime patches.
func validatePatch(patch map[string]any) error {
	// Expand nested patches to dotted leaf paths: checking only the top-level
	// keys would let {"account":{"local":{"path":...}}} slip through.
	for _, k := range config.FlattenKeys(patch) {
		if config.IsStartupOnlyConfigKey(k) {
			return platform.ValidationErrorCode(
				"Config key '"+k+"' is reserved for startup", k, "startup_only_config")
		}
	}
	return nil
}
