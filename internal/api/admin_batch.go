package api

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/gentpan/grok2api/internal/platform"
)

// batchResults collects per-token outcomes keyed by the masked token.
type batchResults struct {
	mu sync.Mutex
	m  map[string]any
}

func newBatchResults() *batchResults { return &batchResults{m: map[string]any{}} }

func (b *batchResults) set(token string, v gin.H) {
	b.mu.Lock()
	b.m[platform.MaskToken(token)] = v
	b.mu.Unlock()
}

func (b *batchResults) fail(token string, err error) { b.set(token, gin.H{"error": err.Error()}) }

// runTokenBatch is the shared body of the batch endpoints: validate the token
// list, fan out with bounded concurrency, answer {"status","results"}.
func (s *Server) runTokenBatch(c *gin.Context, needRefresh bool, timeout time.Duration, op func(ctx context.Context, token string, res *batchResults)) {
	var body struct {
		Tokens []string `json:"tokens"`
	}
	if err := readJSON(c, &body); err != nil {
		writeAppError(c, err)
		return
	}
	tokens := sanitizeTokenList(body.Tokens)
	if len(tokens) == 0 {
		writeAppError(c, platform.ValidationError("No tokens provided", "tokens"))
		return
	}
	if s.Transport == nil {
		writeAppError(c, errUpstreamUnavailable())
		return
	}
	if needRefresh && s.Refresh == nil {
		writeAppError(c, errRefreshUnavailable())
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()
	res := newBatchResults()
	forEachBounded(tokens, batchConcurrency(c), func(tok string) {
		op(ctx, tok, res)
	})
	c.JSON(http.StatusOK, gin.H{"status": "success", "results": res.m})
}

func errUpstreamUnavailable() *platform.AppError {
	return platform.NewAppError("upstream transport not available", platform.ErrServer, "no_upstream", http.StatusServiceUnavailable)
}

func (s *Server) handleBatchNSFW(c *gin.Context) {
	enabled := c.Query("enabled") != "false"
	s.runTokenBatch(c, false, 5*time.Minute, func(ctx context.Context, tok string, res *batchResults) {
		if err := s.runNSFWOne(ctx, tok, enabled); err != nil {
			res.fail(tok, err)
			return
		}
		res.set(tok, gin.H{"success": true})
	})
}

func (s *Server) runNSFWOne(ctx context.Context, token string, enabled bool) error {
	if enabled {
		return s.Transport.NSFWSequence(ctx, token)
	}
	return s.Transport.DisableNSFW(ctx, token)
}

func (s *Server) handleBatchRefresh(c *gin.Context) {
	s.runTokenBatch(c, true, 5*time.Minute, func(ctx context.Context, tok string, res *batchResults) {
		refreshed, _, err := s.Refresh.RefreshTokens(ctx, []string{tok})
		if err != nil {
			res.fail(tok, err)
			return
		}
		res.set(tok, gin.H{"refreshed": refreshed > 0})
	})
}

// handleBatchCacheClear deletes every upstream asset of the given tokens (the
// panel labels it "clear cache"; the local media cache has its own endpoints).
func (s *Server) handleBatchCacheClear(c *gin.Context) {
	s.runTokenBatch(c, false, 5*time.Minute, func(ctx context.Context, tok string, res *batchResults) {
		deleted, err := s.clearTokenAssets(ctx, tok)
		if err != nil {
			res.fail(tok, err)
			return
		}
		res.set(tok, gin.H{"deleted": deleted})
	})
}
