package grok

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gentpan/grok2api/internal/platform"
)

// consoleMediaTimeout bounds one Console media API call (generation can be
// slow).
const consoleMediaTimeout = 5 * time.Minute

// postConsoleJSON marshals payload and POSTs it to a Console resource.
func (t *Transport) postConsoleJSON(ctx context.Context, endpoint, token, what string, payload map[string]any) (map[string]any, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode Console %s payload: %w", what, err)
	}
	return t.PostJSON(ctx, endpoint, token, body, WithConsoleMode(), WithTimeout(consoleMediaTimeout))
}

// ConsoleImageGeneration calls the standard Console image generation resource.
func (t *Transport) ConsoleImageGeneration(ctx context.Context, token string, payload map[string]any) (map[string]any, error) {
	return t.postConsoleJSON(ctx, ConsoleImageGeneration, token, "image", payload)
}

// ConsoleImageEdit calls the standard Console image edit resource.
func (t *Transport) ConsoleImageEdit(ctx context.Context, token string, payload map[string]any) (map[string]any, error) {
	return t.postConsoleJSON(ctx, ConsoleImageEdits, token, "image edit", payload)
}

// ConsoleVideoGeneration creates a standard Console video job.
func (t *Transport) ConsoleVideoGeneration(ctx context.Context, token string, payload map[string]any) (map[string]any, error) {
	return t.postConsoleJSON(ctx, ConsoleVideoGeneration, token, "video", payload)
}

// ConsoleVideoStatus polls a standard Console video job.
func (t *Transport) ConsoleVideoStatus(ctx context.Context, token, requestID string) (map[string]any, error) {
	endpoint := ConsoleVideos + "/" + url.PathEscape(strings.TrimSpace(requestID))
	return t.GetJSON(ctx, endpoint, token, WithConsoleMode(), WithTimeout(consoleMediaTimeout))
}

// ConsoleVideoStatusAt polls a Console video job against a custom endpoint.
// It keeps the DPoP binding tied to that URL, which protocol tests rely on.
func (t *Transport) ConsoleVideoStatusAt(ctx context.Context, endpoint, token string) (map[string]any, error) {
	return t.GetJSON(ctx, endpoint, token, WithConsoleMode(), WithTimeout(consoleMediaTimeout))
}

// DownloadConsoleVideo downloads a completed video from the trusted vidgen.x.ai
// CDN. The request shares the transport's client, so a CDN 403 triggers the
// same session reset as any other upstream call; redirects are never followed,
// so a redirect (3xx) is reported as a failure rather than chased off-host.
func (t *Transport) DownloadConsoleVideo(ctx context.Context, rawURL string) (io.ReadCloser, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.User != nil || !trustedConsoleVideoHost(parsed.Hostname()) {
		return nil, platform.UpstreamError("untrusted Console video URL", 502, "")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, platform.UpstreamError("build Console video request failed: "+err.Error(), 502, "")
	}
	req.Header = resolveProxyProfile().mediaHeaders()
	resp, err := t.pool.do(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return nil, platform.UpstreamError(
			fmt.Sprintf("Console video download returned %d", resp.StatusCode), resp.StatusCode, truncBody(body, 400))
	}
	return resp.Body, nil
}

func trustedConsoleVideoHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	return host == "vidgen.x.ai" || strings.HasSuffix(host, ".vidgen.x.ai")
}
