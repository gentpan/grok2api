package api

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/gentpan/grok2api/internal/platform"
)

// fileIDRE matches a valid local media file ID (UUID-style hex with dashes).
var fileIDRE = regexp.MustCompile(`^[0-9a-fA-F\-]{16,36}$`)

// handleFileImage serves a cached image by file ID (public).
func (s *Server) handleFileImage(c *gin.Context) {
	id := strings.TrimSpace(c.Query("id"))
	if !fileIDRE.MatchString(id) {
		writeAppError(c, platform.ValidationError("Invalid file id", "id"))
		return
	}
	dir, err := s.imageDir()
	if err != nil {
		writeAppError(c, platform.UpstreamError("image dir: "+err.Error(), 500, ""))
		return
	}
	for _, ext := range []string{".jpg", ".png"} {
		path := filepath.Join(dir, id+ext)
		if _, err := os.Stat(path); err == nil {
			mime := "image/jpeg"
			if ext == ".png" {
				mime = "image/png"
			}
			c.Header("Content-Type", mime)
			c.File(path)
			return
		}
	}
	writeAppError(c, platform.ValidationErrorCode("Image not found", "id", "file_not_found"))
}

// handleFileVideo serves a cached video by file ID (public).
func (s *Server) handleFileVideo(c *gin.Context) {
	id := strings.TrimSpace(c.Query("id"))
	if !fileIDRE.MatchString(id) {
		writeAppError(c, platform.ValidationError("Invalid file id", "id"))
		return
	}
	dir, err := s.videoDir()
	if err != nil {
		writeAppError(c, platform.UpstreamError("video dir: "+err.Error(), 500, ""))
		return
	}
	path := filepath.Join(dir, id+".mp4")
	if _, err := os.Stat(path); err != nil {
		writeAppError(c, platform.ValidationErrorCode("Video not found", "id", "file_not_found"))
		return
	}
	c.Header("Content-Type", "video/mp4")
	c.Header("Content-Disposition", `inline; filename="`+id+`.mp4"`)
	c.File(path)
}
