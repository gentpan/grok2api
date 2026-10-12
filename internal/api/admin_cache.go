package api

import (
	"errors"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/gentpan/grok2api/internal/platform"
	"github.com/gentpan/grok2api/internal/storage"
)

// --- Local media cache ---

func errMediaUnavailable() *platform.AppError {
	return platform.NewAppError("local media cache not available", platform.ErrServer, "media_unavailable", http.StatusServiceUnavailable)
}

func errInvalidCacheType(name string) *platform.AppError {
	return platform.ValidationErrorCode("Unknown cache type '"+name+"' (use image or video)", "type", "invalid_cache_type")
}

func (s *Server) handleCacheStats(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"local_image": s.cacheStatsFor(storage.MediaImage),
		"local_video": s.cacheStatsFor(storage.MediaVideo),
	})
}

func (s *Server) cacheStatsFor(mediaType storage.MediaType) map[string]any {
	dir, err := s.mediaDir(mediaType)
	if err != nil {
		return gin.H{"count": 0, "size_bytes": 0, "error": err.Error()}
	}
	entries, _ := os.ReadDir(dir)
	count := 0
	var size int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		count++
		size += info.Size()
	}
	limitMB := s.config().GetInt("cache.local."+string(mediaType)+"_max_mb", 0)
	limitBytes := int64(limitMB) * 1024 * 1024
	usageRatio := 0.0
	usagePercent := 0.0
	if limitBytes > 0 {
		usageRatio = float64(size) / float64(limitBytes)
		usagePercent = usageRatio * 100.0
	}
	return gin.H{
		"count":         count,
		"size_mb":       float64(size) / 1024.0 / 1024.0,
		"size_bytes":    size,
		"limit_mb":      limitMB,
		"limit_bytes":   limitBytes,
		"limited":       limitBytes > 0,
		"usage_ratio":   usageRatio,
		"usage_percent": usagePercent,
	}
}

func (s *Server) handleCacheList(c *gin.Context) {
	cacheType := c.Query("cache_type")
	if cacheType == "" {
		cacheType = c.Query("type")
	}
	mediaType, ok := mediaTypeFromName(cacheType)
	if !ok {
		writeAppError(c, errInvalidCacheType(cacheType))
		return
	}
	page := parseIntQuery(c, "page", 1)
	pageSize := parseIntQuery(c, "page_size", 1000)
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 1000
	}
	dir, err := s.mediaDir(mediaType)
	if err != nil {
		writeAppError(c, err)
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusOK, gin.H{"status": "success", "total": 0, "page": page, "page_size": pageSize, "items": []any{}})
			return
		}
		writeAppError(c, err)
		return
	}
	type fileItem struct {
		name       string
		size       int64
		modifiedAt float64
	}
	items := []fileItem{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, fileItem{name: e.Name(), size: info.Size(), modifiedAt: float64(info.ModTime().UnixMilli())})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].modifiedAt != items[j].modifiedAt {
			return items[i].modifiedAt > items[j].modifiedAt
		}
		return items[i].name < items[j].name
	})
	total := len(items)
	start := (page - 1) * pageSize
	end := start + pageSize
	if start > total {
		start = total
	}
	if end > total {
		end = total
	}
	out := []map[string]any{}
	for _, it := range items[start:end] {
		out = append(out, map[string]any{
			"name":        it.name,
			"size_bytes":  it.size,
			"modified_at": it.modifiedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"status":    "success",
		"total":     total,
		"page":      page,
		"page_size": pageSize,
		"items":     out,
	})
}

func (s *Server) handleCacheClear(c *gin.Context) {
	var body struct {
		Type string `json:"type"`
	}
	// The body is optional: an empty request clears the image cache.
	if err := c.ShouldBindJSON(&body); err != nil && !errors.Is(err, io.EOF) {
		writeAppError(c, platform.ValidationError("Invalid JSON body: "+err.Error(), "body"))
		return
	}
	mediaType, ok := mediaTypeFromName(body.Type)
	if !ok {
		writeAppError(c, errInvalidCacheType(body.Type))
		return
	}
	if s.Media == nil {
		writeAppError(c, errMediaUnavailable())
		return
	}
	removed, err := s.Media.Clear(mediaType)
	if err != nil {
		writeAppError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status": "success",
		"result": gin.H{"removed": removed},
	})
}

func (s *Server) handleCacheItemDelete(c *gin.Context) {
	var body struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := readJSON(c, &body); err != nil {
		writeAppError(c, err)
		return
	}
	if body.Name == "" {
		writeAppError(c, platform.ValidationErrorCode("Missing file name", "name", "missing_file_name"))
		return
	}
	mediaType, ok := mediaTypeFromName(body.Type)
	if !ok {
		writeAppError(c, errInvalidCacheType(body.Type))
		return
	}
	if s.Media == nil {
		writeAppError(c, errMediaUnavailable())
		return
	}
	deleted, err := s.Media.Delete(mediaType, body.Name)
	if err != nil {
		writeAppError(c, platform.ValidationErrorCode(err.Error(), "name", "invalid_file_name"))
		return
	}
	if !deleted {
		writeAppError(c, platform.ValidationErrorCode("File not found", "name", "file_not_found"))
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status": "success",
		"result": gin.H{"deleted": body.Name},
	})
}

func (s *Server) handleCacheItemsDelete(c *gin.Context) {
	var body struct {
		Type  string   `json:"type"`
		Names []string `json:"names"`
	}
	if err := readJSON(c, &body); err != nil {
		writeAppError(c, err)
		return
	}
	clean := []string{}
	for _, n := range body.Names {
		if n = strings.TrimSpace(n); n != "" {
			clean = append(clean, n)
		}
	}
	if len(clean) == 0 {
		writeAppError(c, platform.ValidationErrorCode("Missing file names", "names", "missing_file_names"))
		return
	}
	mediaType, ok := mediaTypeFromName(body.Type)
	if !ok {
		writeAppError(c, errInvalidCacheType(body.Type))
		return
	}
	if s.Media == nil {
		writeAppError(c, errMediaUnavailable())
		return
	}
	deleted, missing := 0, 0
	for _, name := range clean {
		ok, err := s.Media.Delete(mediaType, name)
		if err != nil || !ok {
			missing++
			continue
		}
		deleted++
	}
	c.JSON(http.StatusOK, gin.H{
		"status": "success",
		"result": gin.H{"deleted": deleted, "missing": missing},
	})
}
