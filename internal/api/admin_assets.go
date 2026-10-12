package api

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/gentpan/grok2api/internal/account"
	"github.com/gentpan/grok2api/internal/platform"
)

// assetField returns the first non-empty string value among keys.
func assetField(item map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, _ := item[k].(string); v != "" {
			return v
		}
	}
	return ""
}

// assetItems returns the asset objects of a ListAssets response.
func assetItems(resp map[string]any) []map[string]any {
	raw, _ := resp["assets"].([]any)
	if raw == nil {
		raw, _ = resp["items"].([]any)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, it := range raw {
		if item, ok := it.(map[string]any); ok {
			out = append(out, item)
		}
	}
	return out
}

// extractAssetItems returns the list of asset IDs from a ListAssets response.
func extractAssetItems(resp map[string]any) []string {
	items := assetItems(resp)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if id := assetField(item, "id", "assetId"); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// extractAssetRows returns a normalized list of asset rows from a ListAssets
// response (or an error row when the listing failed).
func extractAssetRows(token string, resp map[string]any, err error) map[string]any {
	if err != nil {
		return map[string]any{
			"token":  token,
			"masked": platform.MaskToken(token),
			"count":  0,
			"assets": []any{},
			"error":  err.Error(),
		}
	}
	items := assetItems(resp)
	rows := make([]map[string]any, 0, len(items))
	for _, item := range items {
		size := 0
		if v, ok := item["fileSize"].(float64); ok {
			size = int(v)
		} else if v, ok := item["size"].(float64); ok {
			size = int(v)
		}
		rows = append(rows, map[string]any{
			"id":           assetField(item, "id", "assetId"),
			"name":         assetField(item, "fileName", "name"),
			"file_path":    assetField(item, "filePath", "file_path"),
			"content_type": assetField(item, "contentType", "content_type"),
			"size":         size,
			"created_at":   assetField(item, "createdAt", "created_at"),
		})
	}
	return map[string]any{
		"token":  token,
		"masked": platform.MaskToken(token),
		"count":  len(rows),
		"assets": rows,
	}
}

// clearTokenAssets deletes every upstream asset of one account.
func (s *Server) clearTokenAssets(ctx context.Context, token string) (int, error) {
	resp, err := s.Transport.ListAssets(ctx, token)
	if err != nil {
		return 0, err
	}
	deleted := 0
	for _, assetID := range extractAssetItems(resp) {
		if _, err := s.Transport.DeleteAsset(ctx, token, assetID); err == nil {
			deleted++
		}
	}
	return deleted, nil
}

// --- Assets ---

func (s *Server) handleAssetsList(c *gin.Context) {
	if s.Repo == nil {
		writeAppError(c, errRepositoryUnavailable())
		return
	}
	if s.Transport == nil {
		writeAppError(c, errUpstreamUnavailable())
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	recs, err := s.listAllAccounts(ctx)
	if err != nil {
		writeAppError(c, err)
		return
	}
	var mu sync.Mutex
	results := []map[string]any{}
	totalAssets := 0
	forEachBounded(recs, assetListConcurrency, func(rec *account.Record) {
		resp, err := s.Transport.ListAssets(ctx, rec.Token)
		row := extractAssetRows(rec.Token, resp, err)
		mu.Lock()
		defer mu.Unlock()
		results = append(results, row)
		if count, _ := row["count"].(int); count > 0 {
			totalAssets += count
		}
	})
	c.JSON(http.StatusOK, gin.H{
		"tokens":       results,
		"total_assets": totalAssets,
	})
}

func (s *Server) handleAssetsDeleteItem(c *gin.Context) {
	var body struct {
		Token   string `json:"token"`
		AssetID string `json:"asset_id"`
	}
	if err := readJSON(c, &body); err != nil {
		writeAppError(c, err)
		return
	}
	token := platform.SanitizeToken(body.Token)
	if token == "" || body.AssetID == "" {
		writeAppError(c, platform.ValidationError("Missing token or asset_id", "body"))
		return
	}
	if s.Transport == nil {
		writeAppError(c, errUpstreamUnavailable())
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	if _, err := s.Transport.DeleteAsset(ctx, token, body.AssetID); err != nil {
		writeAppError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "success"})
}

func (s *Server) handleAssetsClearToken(c *gin.Context) {
	var body struct {
		Token string `json:"token"`
	}
	if err := readJSON(c, &body); err != nil {
		writeAppError(c, err)
		return
	}
	token := platform.SanitizeToken(body.Token)
	if token == "" {
		writeAppError(c, platform.ValidationError("Missing token", "token"))
		return
	}
	if s.Transport == nil {
		writeAppError(c, errUpstreamUnavailable())
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	deleted, err := s.clearTokenAssets(ctx, token)
	if err != nil {
		writeAppError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "success", "deleted": deleted})
}
