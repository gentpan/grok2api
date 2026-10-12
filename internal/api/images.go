package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/gentpan/grok2api/internal/account"
	"github.com/gentpan/grok2api/internal/grok"
	"github.com/gentpan/grok2api/internal/model"
	"github.com/gentpan/grok2api/internal/platform"
)

const (
	// maxLiteImages caps n for the chat-based lite image model (each image is
	// one upstream chat).
	maxLiteImages = 4
	// maxWSImages caps n for WebSocket image models.
	maxWSImages = 10
	// maxEditFormBytes bounds the in-memory part of an upload form.
	maxEditFormBytes = 50 << 20
	// maxEditImageBytes bounds one uploaded image.
	maxEditImageBytes = 30 << 20
	// maxFetchedImageBytes bounds an image downloaded for b64_json.
	maxFetchedImageBytes = 50 << 20
	// imageFetchTimeout bounds downloading one image for b64_json.
	imageFetchTimeout = 30 * time.Second
	// maxConsoleEditImages is how many images a console edit accepts.
	maxConsoleEditImages = 3
	// maxConsoleImages caps n for console image generation / edit.
	maxConsoleImages = 10
)

const emptyImageMessage = "Image generation completed but no image URL was returned (may be rate-limited or moderated)"

// acquireNonEmpty is acquire for operations whose success can still be an
// empty result (an image model that answers but renders nothing, e.g. when
// the prompt is moderated). An empty result does not mark the account failed;
// it earns one more try on another pooled account. If the result stays empty
// the request fails with a 502 carrying emptyMsg.
//
// run reports (empty, err); on a non-empty result the caller reads whatever
// run captured.
func (s *Server) acquireNonEmpty(ctx context.Context, caller Caller, spec *model.Spec, emptyMsg string,
	run func(ctx context.Context, lease *account.Lease) (empty bool, err error)) error {
	var exclude []string
	for pass := 0; pass < 2; pass++ {
		empty := false
		res, err := s.acquire(ctx, caller, spec, acquireOptions{Exclude: exclude}, func(ctx context.Context, lease *account.Lease) (bool, error) {
			e, err := run(ctx, lease)
			empty = e
			return false, err
		})
		if err != nil {
			if pass > 0 && res.Token == "" {
				// No second account to ask: the truthful outcome is the empty
				// first answer, not "no available accounts".
				break
			}
			return err
		}
		if !empty {
			return nil
		}
		if res.BYO || !caller.PoolAllowed {
			break
		}
		exclude = append(exclude, res.Token)
	}
	return platform.UpstreamError(emptyMsg, 502, "")
}

// imageResult renders one image of an OpenAI images response.
func (s *Server) imageEntry(ctx context.Context, url, responseFormat string) map[string]any {
	if responseFormat == "b64_json" {
		if b64, err := s.fetchImageBase64(ctx, url); err == nil {
			return map[string]any{"b64_json": b64}
		}
	}
	return map[string]any{"url": url}
}

// fetchImageBase64 downloads an image through the upstream transport (it
// carries the clearance cookies assets.grok.com wants) and base64-encodes it.
func (s *Server) fetchImageBase64(ctx context.Context, url string) (string, error) {
	if s.Transport == nil {
		return "", errUpstreamUnavailable()
	}
	ctx, cancel := context.WithTimeout(ctx, imageFetchTimeout)
	defer cancel()
	body, err := s.Transport.GetBytes(ctx, url, "")
	if err != nil {
		return "", err
	}
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, maxFetchedImageBytes))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// --- POST /v1/images/generations ---

func (s *Server) handleImageGenerations(c *gin.Context) {
	var req struct {
		Model          string `json:"model"`
		Prompt         string `json:"prompt"`
		N              int    `json:"n,omitempty"`
		Size           string `json:"size,omitempty"`
		ResponseFormat string `json:"response_format,omitempty"`
		AspectRatio    string `json:"aspect_ratio,omitempty"`
		Resolution     string `json:"resolution,omitempty"`
	}
	if err := readJSON(c, &req); err != nil {
		writeAppError(c, err)
		return
	}
	noteUsageModel(c, req.Model)
	spec, ok := s.modelRegistry().Resolve(req.Model)
	if !ok {
		writeAppError(c, errModelNotFound(req.Model))
		return
	}
	if !spec.IsImage() {
		writeAppError(c, platform.ValidationErrorCode("Model '"+req.Model+"' is not an image model", "model", "invalid_model"))
		return
	}
	caller := callerFrom(c.Request.Context())
	if spec.ModeId == model.ModeConsole {
		s.consoleImageGeneration(c, caller, spec, req.Prompt, req.N, req.Size, req.ResponseFormat, req.AspectRatio, req.Resolution)
		return
	}
	n := req.N
	if n <= 0 {
		n = 1
	}
	responseFormat := req.ResponseFormat
	if responseFormat == "" {
		responseFormat = "url"
	}
	if grok.IsWSImageModel(spec.ModelName) {
		s.wsImageGenerations(c, caller, spec, req.Prompt, n, req.Size, responseFormat)
		return
	}

	// Lite model: chat-based generation, one upstream chat per image.
	if n > maxLiteImages {
		n = maxLiteImages
	}
	urls, err := s.liteImageBatch(c.Request.Context(), caller, spec, "Drawing: "+req.Prompt, n)
	if err != nil {
		writeAppError(c, err)
		return
	}
	out := []map[string]any{}
	for _, url := range urls {
		out = append(out, s.imageEntry(c.Request.Context(), url, responseFormat))
	}
	c.JSON(http.StatusOK, gin.H{"created": time.Now().Unix(), "data": out})
}

// liteImageBatch runs n concurrent chat-based image generations and returns
// the URLs it got, in request order. It fails only when none succeeded.
func (s *Server) liteImageBatch(ctx context.Context, caller Caller, spec *model.Spec, prompt string, n int) ([]string, error) {
	if n <= 0 {
		n = 1
	}
	results := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					errs[idx] = platform.NewAppError("internal error", platform.ErrServer, "internal_error", http.StatusInternalServerError)
				}
			}()
			urls, err := s.captureImages(ctx, caller, spec, prompt, nil)
			if err != nil {
				errs[idx] = err
				return
			}
			results[idx] = urls[0]
		}(i)
	}
	wg.Wait()

	out := make([]string, 0, n)
	for _, u := range results {
		if u != "" {
			out = append(out, u)
		}
	}
	if len(out) == 0 {
		for _, err := range errs {
			if err != nil {
				return nil, err
			}
		}
		return nil, platform.UpstreamError(emptyImageMessage, 502, "")
	}
	return out, nil
}

// captureImages generates images through the web chat stream (the same path
// as /v1/chat/completions) and returns their URLs. The streaming path is used
// because grok's non-streaming responses sometimes omit the image URL even
// when progress reaches 100%.
func (s *Server) captureImages(ctx context.Context, caller Caller, spec *model.Spec, message string, fileInputs []string) ([]string, error) {
	var urls []string
	err := s.acquireNonEmpty(ctx, caller, spec, emptyImageMessage, func(ctx context.Context, lease *account.Lease) (bool, error) {
		got, err := s.captureImageURLsOnce(ctx, lease, message, fileInputs)
		if err != nil {
			return false, err
		}
		urls = got
		return len(got) == 0, nil
	})
	if err != nil {
		return nil, err
	}
	return urls, nil
}

// captureImageURLsOnce runs one web chat attempt and collects image URLs.
func (s *Server) captureImageURLsOnce(ctx context.Context, lease *account.Lease, message string, fileInputs []string) ([]string, error) {
	if s.Transport == nil {
		return nil, errUpstreamUnavailable()
	}
	payload := grok.BuildChatPayload(message, model.ModeId(lease.ModeID), fileInputs, nil, nil, nil)
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, platform.UpstreamError("encode payload: "+err.Error(), 500, "")
	}
	ctx, cancel := context.WithTimeout(ctx, chatAttemptTimeout)
	defer cancel()
	rc, err := s.Transport.PostStream(ctx, grok.Chat, lease.Token, body)
	if err != nil {
		return nil, err
	}
	src := newWebSource(ctx, rc)
	defer src.Close()

	var urls []string
	seen := map[string]bool{}
	add := func(u string) {
		if u == "" {
			return
		}
		if !strings.HasPrefix(u, "http") {
			u = grok.ImageBaseURL + strings.TrimPrefix(u, "/")
		}
		if !seen[u] {
			seen[u] = true
			urls = append(urls, u)
		}
	}
	for {
		evs, err := src.Next()
		for _, ev := range evs {
			if ev.Kind == evImage {
				add(ev.Content)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	// Also take every URL the adapter accumulated.
	for _, pair := range src.Meta().ImageURLs {
		add(pair[0])
	}
	return urls, nil
}

// wsImageGenerations serves image generation through the WS imagine endpoint.
func (s *Server) wsImageGenerations(c *gin.Context, caller Caller, spec *model.Spec, prompt string, n int, size, responseFormat string) {
	if n <= 0 {
		n = 1
	}
	if n > maxWSImages {
		n = maxWSImages
	}
	if s.Transport == nil {
		writeAppError(c, errUpstreamUnavailable())
		return
	}
	aspectRatio := grok.ResolveAspectRatio(size)
	enableNSFW := s.config().GetBool("features.enable_nsfw", true)
	enablePro := grok.IsProImageModel(spec.ModelName)

	type collected struct{ url, blob string }
	var images []collected
	err := s.acquireNonEmpty(c.Request.Context(), caller, spec, emptyImageMessage, func(ctx context.Context, lease *account.Lease) (bool, error) {
		ctx, cancel := context.WithTimeout(ctx, imageAttemptTimeout)
		defer cancel()
		images = nil
		for ev := range s.Transport.StreamImages(ctx, lease.Token, prompt, aspectRatio, n, enableNSFW, enablePro) {
			switch ev.Type {
			case grok.ImagineEventImage:
				url := ""
				if ev.URL != "" {
					url = grok.ImageBaseURL + strings.TrimPrefix(ev.URL, "/")
				}
				images = append(images, collected{url: url, blob: ev.Blob})
			case grok.ImagineEventError:
				return false, platform.UpstreamError(ev.Error, 502, "")
			}
		}
		if ctx.Err() != nil {
			return false, ctxError(ctx)
		}
		return len(images) == 0, nil
	})
	if err != nil {
		writeAppError(c, err)
		return
	}

	out := []map[string]any{}
	for i := 0; i < n && i < len(images); i++ {
		img := images[i]
		switch {
		case responseFormat != "b64_json":
			out = append(out, map[string]any{"url": img.url})
		case img.blob != "":
			out = append(out, map[string]any{"b64_json": img.blob})
		case img.url != "":
			out = append(out, s.imageEntry(c.Request.Context(), img.url, responseFormat))
		}
	}
	c.JSON(http.StatusOK, gin.H{"created": time.Now().Unix(), "data": out})
}

// --- POST /v1/images/edits ---

// uploadedImage is one validated image of an upload form.
type uploadedImage struct {
	mime string
	data []byte
}

func (u uploadedImage) dataURI() string {
	return "data:" + u.mime + ";base64," + base64.StdEncoding.EncodeToString(u.data)
}

// readUploadedImages reads and validates the image files of fields (the first
// non-empty field wins).
func readUploadedImages(form *multipart.Form, fields ...string) ([]uploadedImage, *platform.AppError) {
	var files []*multipart.FileHeader
	for _, f := range fields {
		if files = form.File[f]; len(files) > 0 {
			break
		}
	}
	out := make([]uploadedImage, 0, len(files))
	for _, fh := range files {
		f, err := fh.Open()
		if err != nil {
			return nil, platform.ValidationError("Unable to read image: "+err.Error(), "image")
		}
		raw, rerr := io.ReadAll(io.LimitReader(f, maxEditImageBytes))
		_ = f.Close()
		if rerr != nil || len(raw) == 0 {
			return nil, platform.ValidationError("Invalid image", "image")
		}
		mime := strings.TrimSpace(fh.Header.Get("Content-Type"))
		if !strings.HasPrefix(strings.ToLower(mime), "image/") {
			return nil, platform.ValidationError("Unsupported image content type", "image")
		}
		out = append(out, uploadedImage{mime: mime, data: raw})
	}
	return out, nil
}

func (s *Server) handleImageEdits(c *gin.Context) {
	if err := c.Request.ParseMultipartForm(maxEditFormBytes); err != nil {
		writeAppError(c, platform.ValidationError("Invalid multipart form: "+err.Error(), "body"))
		return
	}
	form := c.Request.MultipartForm
	modelName := strings.TrimSpace(c.Request.FormValue("model"))
	prompt := strings.TrimSpace(c.Request.FormValue("prompt"))
	if modelName == "" || prompt == "" {
		writeAppError(c, platform.ValidationError("Missing model or prompt", "body"))
		return
	}
	noteUsageModel(c, modelName)
	spec, ok := s.modelRegistry().Resolve(modelName)
	if !ok {
		writeAppError(c, errModelNotFound(modelName))
		return
	}
	if !spec.IsImageEdit() {
		writeAppError(c, platform.ValidationErrorCode("Model '"+modelName+"' is not an image-edit model", "model", "invalid_model"))
		return
	}
	caller := callerFrom(c.Request.Context())
	images, perr := readUploadedImages(form, "image[]", "image")
	if perr != nil {
		writeAppError(c, perr)
		return
	}
	if spec.ModeId == model.ModeConsole {
		s.consoleImageEdit(c, caller, spec, prompt, images)
		return
	}
	if len(images) == 0 {
		writeAppError(c, platform.ValidationError("No images provided", "image[]"))
		return
	}
	responseFormat := strings.TrimSpace(c.Request.FormValue("response_format"))
	if responseFormat == "" {
		responseFormat = "url"
	}

	content := []map[string]any{{"type": "text", "text": prompt}}
	for _, img := range images {
		content = append(content, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": img.dataURI()},
		})
	}
	message, files, perr := extractMessages([]map[string]any{{"role": "user", "content": content}})
	if perr != nil {
		writeAppError(c, perr)
		return
	}
	urls, err := s.captureImages(c.Request.Context(), caller, spec, message, files)
	if err != nil {
		writeAppError(c, err)
		return
	}
	out := []map[string]any{}
	for _, url := range urls {
		out = append(out, s.imageEntry(c.Request.Context(), url, responseFormat))
	}
	c.JSON(http.StatusOK, gin.H{"created": time.Now().Unix(), "data": out})
}
