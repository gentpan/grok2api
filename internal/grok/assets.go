package grok

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/gentpan/grok2api/internal/config"
	"github.com/gentpan/grok2api/internal/platform"
)

// AssetUploadResult is the response from POST /rest/app-chat/upload-file.
type AssetUploadResult struct {
	FileID  string `json:"fileMetadataId"`
	FileURI string `json:"fileUri"`
}

// extensionMIME maps file extensions to MIME types (best-effort inference).
var extensionMIME = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".webp": "image/webp",
	".mp4":  "video/mp4",
	".webm": "video/webm",
}

// inferContentType returns the MIME type for rawURL based on its extension.
func inferContentType(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return extensionMIME[strings.ToLower(path.Ext(u.Path))]
}

// parseDataURI splits a data URI into (filename, base64Content, mimeType).
// Returns an error when the URI is malformed.
func parseDataURI(dataURI string) (filename, b64, mime string, err error) {
	if !strings.HasPrefix(dataURI, "data:") {
		return "", "", "", platform.ValidationError("File input must be a URL or data URI", "content")
	}
	comma := strings.IndexByte(dataURI, ',')
	if comma < 0 {
		return "", "", "", platform.ValidationError("Malformed data URI: missing comma separator", "content")
	}
	header := dataURI[:comma]
	b64 = dataURI[comma+1:]
	if !strings.Contains(header, ";base64") {
		return "", "", "", platform.ValidationError("Data URI must be base64-encoded", "content")
	}
	mime = strings.TrimSpace(header[len("data:"):strings.Index(header, ";")])
	if mime == "" {
		mime = "application/octet-stream"
	}
	b64 = stripWhitespace(b64)
	if b64 == "" {
		return "", "", "", platform.ValidationError("Data URI has empty payload", "content")
	}
	ext := "bin"
	if i := strings.IndexByte(mime, '/'); i >= 0 {
		ext = mime[i+1:]
	}
	return "file." + ext, b64, mime, nil
}

func stripWhitespace(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r':
			return -1
		}
		return r
	}, s)
}

// configSeconds reads a timeout (in seconds) from config.
func configSeconds(key string, def int) time.Duration {
	return time.Duration(config.Global().GetInt(key, def)) * time.Second
}

// UploadFile uploads base64-encoded content to Grok and returns the file
// metadata ID used as a chat attachment reference.
func UploadFile(ctx context.Context, t *Transport, token, filename, mime, b64 string) (AssetUploadResult, error) {
	body, err := json.Marshal(map[string]any{
		"fileName":     filename,
		"fileMimeType": mime,
		"content":      b64,
	})
	if err != nil {
		return AssetUploadResult{}, platform.UpstreamError("asset upload encode failed: "+err.Error(), 500, "")
	}
	resp, err := t.PostJSON(ctx, AssetsUpload, token, body, WithTimeout(configSeconds("asset.upload_timeout", 60)))
	if err != nil {
		return AssetUploadResult{}, err
	}
	result := AssetUploadResult{}
	if v, ok := resp["fileMetadataId"].(string); ok {
		result.FileID = v
	} else if v, ok := resp["fileId"].(string); ok {
		result.FileID = v
	}
	if v, ok := resp["fileUri"].(string); ok {
		result.FileURI = v
	}
	return result, nil
}

// UploadFromInput parses fileInput (URL or data URI), fetches remote bytes
// when needed, then uploads to Grok. Remote URLs must be public http(s)
// addresses: loopback, private and link-local targets are refused.
func UploadFromInput(ctx context.Context, t *Transport, token, fileInput string) (AssetUploadResult, error) {
	if !isURL(fileInput) {
		filename, b64, mime, err := parseDataURI(fileInput)
		if err != nil {
			return AssetUploadResult{}, err
		}
		return UploadFile(ctx, t, token, filename, mime, b64)
	}
	if err := assetFetchGuard(ctx, fileInput); err != nil {
		return AssetUploadResult{}, err
	}
	// Fetch the remote URL through the transport and re-upload as base64.
	reader, err := t.GetBytes(ctx, fileInput, token, WithTimeout(configSeconds("asset.upload_timeout", 60)))
	if err != nil {
		return AssetUploadResult{}, err
	}
	defer reader.Close()
	raw, err := io.ReadAll(io.LimitReader(reader, maxFetchBytes+1))
	if err != nil {
		return AssetUploadResult{}, platform.UpstreamError("asset fetch read failed: "+err.Error(), 502, "")
	}
	if len(raw) > maxFetchBytes {
		return AssetUploadResult{}, platform.ValidationError(
			fmt.Sprintf("Remote file exceeds the %d MiB limit", maxFetchBytes>>20), "content")
	}
	mime := inferContentType(fileInput)
	if mime == "" {
		mime = "application/octet-stream"
	}
	filename := path.Base(fileInput)
	if filename == "" || filename == "/" || filename == "." {
		filename = "download"
	}
	return UploadFile(ctx, t, token, filename, mime, base64.StdEncoding.EncodeToString(raw))
}

// ListAssets returns the asset list for token.
func ListAssets(ctx context.Context, t *Transport, token string) (map[string]any, error) {
	return t.GetJSON(ctx, AssetsListURL, token, WithTimeout(configSeconds("asset.list_timeout", 60)))
}

// DeleteAsset deletes a single asset by ID.
func DeleteAsset(ctx context.Context, t *Transport, token, assetID string) (map[string]any, error) {
	return t.DeleteJSON(ctx, assetDeleteURL(assetID), token, WithTimeout(configSeconds("asset.delete_timeout", 60)))
}

// assetDeleteURL is the delete endpoint of one asset; the id is escaped so it
// can never alter the path or query.
func assetDeleteURL(assetID string) string {
	return AssetsDeleteURL + "/" + url.PathEscape(assetID)
}

func isURL(value string) bool {
	u, err := url.Parse(value)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// assetFetchGuard decides whether a caller-supplied URL may be fetched. It is
// a variable so tests can allow a local httptest server.
var assetFetchGuard = defaultAssetFetchGuard

func defaultAssetFetchGuard(ctx context.Context, rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return platform.ValidationError("Remote file URL must be an http(s) URL", "content")
	}
	if u.User != nil {
		return platform.ValidationError("Remote file URL must not contain credentials", "content")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	deny := func() error {
		return platform.ValidationError("Remote file URL points to a private or local address", "content")
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return deny()
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if blockedAddr(addr) {
			return deny()
		}
		return nil
	}
	// Name: reject when it resolves to a blocked address. A failed lookup is
	// not a rejection — the egress proxy may be the one resolving it.
	lookupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupNetIP(lookupCtx, "ip", host)
	if err != nil {
		return nil
	}
	for _, a := range addrs {
		if blockedAddr(a) {
			return deny()
		}
	}
	return nil
}

// cgnatPrefix is the carrier-grade NAT range (100.64.0.0/10), not covered by
// netip.Addr.IsPrivate.
var cgnatPrefix = netip.MustParsePrefix("100.64.0.0/10")

func blockedAddr(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsMulticast() || a.IsUnspecified() || cgnatPrefix.Contains(a)
}
