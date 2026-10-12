package api

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/gentpan/grok2api/internal/account"
	"github.com/gentpan/grok2api/internal/platform"
)

// --- System endpoints ---

func (s *Server) handleStorageGet(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"type": "jsonl"})
}

func errDirectoryNotInitialised() *platform.AppError {
	return platform.NewAppError("directory not initialised", platform.ErrServer, "directory_not_initialised", http.StatusServiceUnavailable)
}

func errRefreshUnavailable() *platform.AppError {
	return platform.NewAppError("refresh service not available", platform.ErrServer, "no_refresh", http.StatusServiceUnavailable)
}

func errRepositoryUnavailable() *platform.AppError {
	return platform.NewAppError("account repository not initialised", platform.ErrServer, "repository_not_initialised", http.StatusServiceUnavailable)
}

func (s *Server) handleStatusGet(c *gin.Context) {
	if s.Directory == nil {
		writeAppError(c, errDirectoryNotInitialised())
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status":             "ok",
		"size":               s.Directory.Size(),
		"revision":           s.Directory.Revision(),
		"selection_strategy": string(s.Directory.Strategy()),
	})
}

func (s *Server) handleSync(c *gin.Context) {
	if s.Directory == nil {
		writeAppError(c, errDirectoryNotInitialised())
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	changed, err := s.Directory.SyncIfChanged(ctx)
	if err != nil {
		writeAppError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"changed":  changed,
		"revision": s.Directory.Revision(),
	})
}

// applySelectionStrategy mirrors the startup mapping: account.refresh.enabled
// selects quota mode, otherwise random. It returns the effective strategy.
func (s *Server) applySelectionStrategy() account.Strategy {
	enabled := s.config().GetBool("account.refresh.enabled", false)
	if s.Directory == nil {
		if enabled {
			return account.StrategyQuota
		}
		return account.StrategyRandom
	}
	if d, ok := s.Directory.(*account.Directory); ok {
		maxInflight := s.config().GetInt("account.selection.max_inflight", account.DefaultMaxInflight)
		return account.ReconcileRuntime(d, enabled, maxInflight)
	}
	target := account.StrategyRandom
	if enabled {
		target = account.StrategyQuota
	}
	s.Directory.SetStrategy(target)
	return target
}
