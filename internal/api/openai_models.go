package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/gentpan/grok2api/internal/model"
	"github.com/gentpan/grok2api/internal/platform"
)

// handleModels returns the list of models currently available given the
// runtime account pools.
func (s *Server) handleModels(c *gin.Context) {
	created := 1700000000
	out := []map[string]any{}
	for _, spec := range s.modelRegistry().ListEnabled() {
		if !s.modelAvailable(spec) {
			continue
		}
		out = append(out, map[string]any{
			"id": spec.ModelName, "object": "model", "created": created,
			"owned_by": "xai", "name": spec.PublicName,
		})
	}
	c.JSON(http.StatusOK, gin.H{"object": "list", "data": out})
}

// handleModelGet returns a single model by id.
func (s *Server) handleModelGet(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		writeAppError(c, platform.ValidationError("Missing model id", "model"))
		return
	}
	spec, ok := s.modelRegistry().Resolve(id)
	if !ok {
		writeAppError(c, platform.NewAppError("Model '"+id+"' not found", platform.ErrValidation, "model_not_found", http.StatusNotFound))
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"id": spec.ModelName, "object": "model", "created": 1700000000,
		"owned_by": "xai", "name": spec.PublicName,
	})
}

// modelAvailable checks that at least one of the spec's pool candidates is
// present in the runtime directory and supports the spec's mode.
func (s *Server) modelAvailable(spec *model.Spec) bool {
	if s.Directory == nil {
		return false
	}
	if spec.ModeId == model.ModeConsole {
		for _, mode := range []int{int(model.ModeConsole)} {
			if s.Directory.HasPoolMode(0, mode) {
				return true
			}
		}
		return false
	}
	for _, p := range spec.PoolCandidates() {
		if s.Directory.HasPoolMode(p, int(spec.ModeId)) {
			return true
		}
	}
	return false
}
