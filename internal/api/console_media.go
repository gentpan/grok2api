package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/gentpan/grok2api/internal/account"
	"github.com/gentpan/grok2api/internal/grok"
	"github.com/gentpan/grok2api/internal/model"
	"github.com/gentpan/grok2api/internal/platform"
)

// noConsoleAccountsMessage is the 429 text when no console account is left.
const noConsoleAccountsMessage = "No available Console SSO accounts"

// consoleCall runs one console.x.ai media call on an account (pooled or the
// caller's own token) and answers with its JSON result.
func (s *Server) consoleCall(c *gin.Context, caller Caller, spec *model.Spec,
	call func(ctx context.Context, token string) (map[string]any, error)) {
	if s.Transport == nil {
		writeAppError(c, errUpstreamUnavailable())
		return
	}
	var result map[string]any
	_, err := s.acquire(c.Request.Context(), caller, spec, acquireOptions{NoAccountsMessage: noConsoleAccountsMessage},
		func(ctx context.Context, lease *account.Lease) (bool, error) {
			r, err := call(ctx, lease.Token)
			if err != nil {
				return false, err
			}
			result = r
			return false, nil
		})
	if err != nil {
		writeAppError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

func (s *Server) consoleImageGeneration(c *gin.Context, caller Caller, spec *model.Spec, prompt string, n int, size, responseFormat, aspectRatio, resolution string) {
	if strings.TrimSpace(prompt) == "" {
		writeAppError(c, platform.ValidationError("Missing prompt", "prompt"))
		return
	}
	if n <= 0 {
		n = 1
	}
	if n > maxConsoleImages {
		writeAppError(c, platform.ValidationError("n must be between 1 and 10", "n"))
		return
	}
	format, err := normalizeConsoleImageFormat(responseFormat)
	if err != nil {
		writeAppError(c, platform.ValidationError(err.Error(), "response_format"))
		return
	}
	ratio, err := resolveConsoleAspectRatio(aspectRatio, size)
	if err != nil {
		writeAppError(c, platform.ValidationError(err.Error(), "size"))
		return
	}
	resolution, err = normalizeConsoleResolution(resolution)
	if err != nil {
		writeAppError(c, platform.ValidationError(err.Error(), "resolution"))
		return
	}
	payload := map[string]any{
		"model":           grok.ConsoleModels[spec.ModelName],
		"prompt":          prompt,
		"n":               n,
		"response_format": format,
	}
	if ratio != "" {
		payload["aspect_ratio"] = ratio
	}
	if resolution != "" {
		payload["resolution"] = resolution
	}
	s.consoleCall(c, caller, spec, func(ctx context.Context, token string) (map[string]any, error) {
		return s.Transport.ConsoleImageGeneration(ctx, token, payload)
	})
}

func (s *Server) consoleImageEdit(c *gin.Context, caller Caller, spec *model.Spec, prompt string, uploads []uploadedImage) {
	responseFormat, err := normalizeConsoleImageFormat(c.Request.FormValue("response_format"))
	if err != nil {
		writeAppError(c, platform.ValidationError(err.Error(), "response_format"))
		return
	}
	n := 1
	if raw := strings.TrimSpace(c.Request.FormValue("n")); raw != "" {
		if parsed, parseErr := parseIntStr(raw); parseErr == nil {
			n = parsed
		}
	}
	if n < 1 || n > maxConsoleImages {
		writeAppError(c, platform.ValidationError("n must be between 1 and 10", "n"))
		return
	}
	if len(uploads) == 0 || len(uploads) > maxConsoleEditImages {
		writeAppError(c, platform.ValidationError("Console image edit requires 1 to 3 images", "image"))
		return
	}
	images := make([]map[string]any, 0, len(uploads))
	for _, u := range uploads {
		images = append(images, map[string]any{"type": "image_url", "url": u.dataURI()})
	}
	ratio, err := resolveConsoleAspectRatio(c.Request.FormValue("aspect_ratio"), c.Request.FormValue("size"))
	if err != nil {
		writeAppError(c, platform.ValidationError(err.Error(), "size"))
		return
	}
	resolution, err := normalizeConsoleResolution(c.Request.FormValue("resolution"))
	if err != nil {
		writeAppError(c, platform.ValidationError(err.Error(), "resolution"))
		return
	}
	payload := map[string]any{
		"model":           grok.ConsoleModels[spec.ModelName],
		"prompt":          prompt,
		"n":               n,
		"response_format": responseFormat,
	}
	if len(images) == 1 {
		payload["image"] = images[0]
	} else {
		payload["images"] = images
	}
	if ratio != "" {
		payload["aspect_ratio"] = ratio
	}
	if resolution != "" {
		payload["resolution"] = resolution
	}
	s.consoleCall(c, caller, spec, func(ctx context.Context, token string) (map[string]any, error) {
		return s.Transport.ConsoleImageEdit(ctx, token, payload)
	})
}

func normalizeConsoleImageFormat(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "url", nil
	}
	if value != "url" && value != "b64_json" {
		return "", fmt.Errorf("response_format must be url or b64_json")
	}
	return value, nil
}

func normalizeConsoleResolution(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "", nil
	}
	if value != "1k" && value != "2k" {
		return "", fmt.Errorf("resolution must be 1k or 2k")
	}
	return value, nil
}

func resolveConsoleAspectRatio(aspectRatio, size string) (string, error) {
	value := strings.ToLower(strings.TrimSpace(aspectRatio))
	if value == "" {
		value = strings.ToLower(strings.TrimSpace(size))
	}
	if value == "" || value == "auto" {
		return "", nil
	}
	values := map[string]string{
		"1:1": "1:1", "16:9": "16:9", "9:16": "9:16", "4:3": "4:3", "3:4": "3:4",
		"3:2": "3:2", "2:3": "2:3", "2:1": "2:1", "1:2": "1:2",
		"1024x1024": "1:1", "1280x720": "16:9", "720x1280": "9:16",
		"1792x1024": "3:2", "1536x1024": "3:2", "1024x1792": "2:3", "1024x1536": "2:3",
	}
	if ratio := values[value]; ratio != "" {
		return ratio, nil
	}
	return "", fmt.Errorf("unsupported aspect_ratio or size")
}

func resolveConsoleVideoSize(size string) (resolution, ratio string) {
	switch strings.ToLower(strings.TrimSpace(size)) {
	case "1280x720", "720p", "16:9":
		return "720p", "16:9"
	case "720x1280", "9:16", "":
		return "720p", "9:16"
	case "854x480", "480p":
		return "480p", "16:9"
	case "480x854":
		return "480p", "9:16"
	default:
		return "720p", "9:16"
	}
}

func numberAsInt(value any) (int, bool) {
	switch number := value.(type) {
	case int:
		return number, true
	case float64:
		return int(number), true
	case int64:
		return int(number), true
	}
	return 0, false
}
