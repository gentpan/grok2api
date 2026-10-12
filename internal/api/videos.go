package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/gentpan/grok2api/internal/account"
	"github.com/gentpan/grok2api/internal/grok"
	"github.com/gentpan/grok2api/internal/logger"
	"github.com/gentpan/grok2api/internal/model"
	"github.com/gentpan/grok2api/internal/platform"
)

const (
	// videoJobTimeout bounds one video job, including failover attempts.
	videoJobTimeout = 10 * time.Minute
	// defaultVideoSeconds is the clip length when the request names none.
	defaultVideoSeconds = 6
	// maxConsoleVideoSeconds is the longest console clip.
	maxConsoleVideoSeconds = 15
	// consoleVideoPollInterval is how often a console job asks for status.
	consoleVideoPollInterval = 2 * time.Second
	// maxCachedVideoBytes bounds a video downloaded for the local cache.
	maxCachedVideoBytes = 512 << 20
	// maxVideoFormBytes bounds the in-memory part of the create form.
	maxVideoFormBytes = 50 << 20
	// defaultVideoSize is used when the request names none.
	defaultVideoSize = "720x1280"
)

// webVideoLengths are the clip lengths of the grok.com video model.
var webVideoLengths = []int{6, 10, 12, 16, 20}

func isValidVideoLength(n int) bool {
	for _, v := range webVideoLengths {
		if v == n {
			return true
		}
	}
	return false
}

// parseIntStr parses a plain decimal integer: optional '-', then ASCII digits.
// Unlike strconv.Atoi it rejects a leading '+'; it also rejects overflow.
func parseIntStr(s string) (int, error) {
	digits := strings.TrimPrefix(s, "-")
	if digits == "" {
		return 0, fmt.Errorf("empty integer")
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, fmt.Errorf("invalid integer %q", s)
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid integer %q", s)
	}
	return n, nil
}

// handleVideoCreate queues an asynchronous video job owned by the caller.
func (s *Server) handleVideoCreate(c *gin.Context) {
	if err := c.Request.ParseMultipartForm(maxVideoFormBytes); err != nil {
		writeAppError(c, platform.ValidationError("Invalid multipart form: "+err.Error(), "body"))
		return
	}
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
	if !spec.IsVideo() {
		writeAppError(c, platform.ValidationErrorCode("Model '"+modelName+"' is not a video model", "model", "invalid_model"))
		return
	}
	seconds := defaultVideoSeconds
	if v := c.Request.FormValue("seconds"); v != "" {
		n, err := parseIntStr(v)
		if err != nil {
			writeAppError(c, platform.ValidationError("Invalid seconds", "seconds"))
			return
		}
		if spec.ModeId == model.ModeConsole {
			if n < 1 || n > maxConsoleVideoSeconds {
				writeAppError(c, platform.ValidationError("Console video seconds must be between 1 and 15", "seconds"))
				return
			}
		} else if !isValidVideoLength(n) {
			writeAppError(c, platform.ValidationError(
				fmt.Sprintf("seconds must be one of %v", webVideoLengths), "seconds"))
			return
		}
		seconds = n
	}
	size := c.Request.FormValue("size")
	if size == "" {
		size = defaultVideoSize
	}
	caller := callerFrom(c.Request.Context())
	job := &videoJob{
		ID:        "video_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:24],
		Object:    "video",
		CreatedAt: time.Now().Unix(),
		Status:    videoQueued,
		Model:     modelName,
		Prompt:    prompt,
		Seconds:   seconds,
		Size:      size,
		Quality:   "standard",
		Owner:     caller.OwnerID(),
	}
	s.videoJobStore().add(job)
	s.startVideoJob(job, caller, spec)
	c.JSON(http.StatusOK, job.toDict())
}

// handleVideoGet serves GET /v1/videos/:id and /v1/videos/:id/content. A job
// is visible to the caller that created it only.
func (s *Server) handleVideoGet(c *gin.Context) {
	id := c.Param("id")
	job := s.videoJobStore().get(id, callerFrom(c.Request.Context()).OwnerID())
	if job == nil {
		writeAppError(c, platform.ValidationErrorCode("Video '"+id+"' not found", "video_id", "video_not_found"))
		return
	}
	if !strings.HasSuffix(c.Request.URL.Path, "/content") {
		c.JSON(http.StatusOK, job.toDict())
		return
	}
	status, path := job.content()
	switch {
	case status == videoFailed:
		writeAppError(c, platform.NewAppError("Video generation failed", platform.ErrUpstream, "video_failed", http.StatusConflict))
	case status != videoCompleted:
		writeAppError(c, platform.NewAppError("Video content is not ready yet", platform.ErrUpstream, "video_not_ready", http.StatusConflict))
	case path == "":
		writeAppError(c, platform.NewAppError(
			"The video was generated but is not cached on this server; download it from video_url",
			platform.ErrUpstream, "video_content_unavailable", http.StatusBadGateway))
	default:
		c.Header("Content-Type", "video/mp4")
		c.Header("Content-Disposition", `inline; filename="`+id+`.mp4"`)
		c.File(path)
	}
}

// startVideoJob runs the job in the background on behalf of caller. The
// caller is passed explicitly: the request context it came from is gone by
// the time the job runs, and a missing identity must never widen access.
func (s *Server) startVideoJob(job *videoJob, caller Caller, spec *model.Spec) {
	s.background("video-job "+job.ID, videoJobTimeout, func(ctx context.Context) {
		job.start()
		var failure string
		if spec.ModeId == model.ModeConsole {
			failure = s.runConsoleVideoJob(ctx, caller, job, spec)
		} else {
			failure = s.runWebVideoJob(ctx, caller, job, spec)
		}
		if failure != "" {
			job.fail(failure, time.Now())
		}
	})
}

// recoverJob turns a panic inside a job runner into a failed job.
func recoverJob(job *videoJob, failure *string) {
	if r := recover(); r != nil {
		logger.Errorf("video job panicked: job=%s panic=%v", job.ID, r)
		*failure = "internal error"
	}
}

// failureMessage composes the job error text from the stage that failed.
func failureMessage(stage string, err error) string {
	if stage == "" {
		return err.Error()
	}
	return stage + ": " + err.Error()
}

// cacheVideo downloads a finished video into the local media cache and
// returns its path ("" when it could not be cached).
func (s *Server) cacheVideo(ctx context.Context, jobID string, fetch func(context.Context) (io.ReadCloser, error)) string {
	if s.Media == nil {
		return ""
	}
	rc, err := fetch(ctx)
	if err != nil {
		logger.Warnf("video not cached: job=%s error=%v", jobID, err)
		return ""
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, maxCachedVideoBytes))
	if err != nil || len(raw) == 0 {
		logger.Warnf("video not cached: job=%s read_error=%v bytes=%d", jobID, err, len(raw))
		return ""
	}
	path, err := s.Media.SaveVideo(raw, strings.TrimPrefix(jobID, "video_"))
	if err != nil {
		logger.Warnf("video not cached: job=%s save_error=%v", jobID, err)
		return ""
	}
	return path
}

// runWebVideoJob generates the video through the grok.com chat stream. It
// returns the failure message ("" on success).
func (s *Server) runWebVideoJob(ctx context.Context, caller Caller, job *videoJob, spec *model.Spec) (failure string) {
	defer recoverJob(job, &failure)
	if s.Transport == nil {
		return errUpstreamUnavailable().Message
	}
	stage := ""
	var videoURL, contentPath string
	_, err := s.acquire(ctx, caller, spec, acquireOptions{}, func(ctx context.Context, lease *account.Lease) (bool, error) {
		stage = "video upstream"
		payload := grok.BuildChatPayload(job.Prompt+" --mode=normal", model.ModeId(lease.ModeID), nil, nil, nil, map[string]any{
			"enable_pro": false,
			"preset":     "normal",
		})
		body, err := json.Marshal(payload)
		if err != nil {
			return false, platform.UpstreamError("encode video payload: "+err.Error(), 500, "")
		}
		rc, err := s.Transport.PostStream(ctx, grok.Chat, lease.Token, body, grok.WithReferer("https://grok.com/imagine"))
		if err != nil {
			return false, err
		}
		src := newWebSource(ctx, rc)
		defer src.Close()
		// From here the upstream is generating (and billing) the video, so a
		// failure is final: retrying would start a second generation on
		// another account while the first one is orphaned.
		for {
			evs, err := src.Next()
			for _, ev := range evs {
				if ev.Kind != evImageProgress {
					continue
				}
				if n, perr := parseIntStr(ev.Content); perr == nil {
					job.setProgress(n)
				}
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				return true, err
			}
		}
		urls := src.Meta().ImageURLs
		if len(urls) == 0 || urls[0][0] == "" {
			return true, platform.UpstreamError("no video URL in upstream response", 502, "")
		}
		videoURL = urls[0][0]
		contentPath = s.cacheVideo(ctx, job.ID, func(ctx context.Context) (io.ReadCloser, error) {
			return s.Transport.GetBytes(ctx, videoURL, lease.Token)
		})
		return false, nil
	})
	if err != nil {
		return failureMessage(stage, err)
	}
	job.complete(videoURL, contentPath, time.Now())
	return ""
}

// runConsoleVideoJob generates the video through console.x.ai (create, then
// poll). It returns the failure message ("" on success).
func (s *Server) runConsoleVideoJob(ctx context.Context, caller Caller, job *videoJob, spec *model.Spec) (failure string) {
	defer recoverJob(job, &failure)
	if s.Transport == nil {
		return errUpstreamUnavailable().Message
	}
	stage := ""
	var videoURL, contentPath, generationFailure string
	_, err := s.acquire(ctx, caller, spec, acquireOptions{NoAccountsMessage: noConsoleAccountsMessage},
		func(ctx context.Context, lease *account.Lease) (bool, error) {
			resolution, ratio := resolveConsoleVideoSize(job.Size)
			payload := map[string]any{
				"model":      "grok-imagine-video",
				"prompt":     job.Prompt,
				"duration":   job.Seconds,
				"resolution": resolution,
			}
			if ratio != "" {
				payload["aspect_ratio"] = ratio
			}
			stage = "Console video create"
			created, err := s.Transport.ConsoleVideoGeneration(ctx, lease.Token, payload)
			if err != nil {
				return false, err
			}
			requestID, _ := created["request_id"].(string)
			if strings.TrimSpace(requestID) == "" {
				generationFailure = "Console video create response missing request_id"
				return false, nil
			}

			stage = "Console video poll"
			ticker := time.NewTicker(consoleVideoPollInterval)
			defer ticker.Stop()
			for {
				status, err := s.Transport.ConsoleVideoStatus(ctx, lease.Token, requestID)
				if err != nil {
					// The generation exists upstream: a poll failure must not
					// start a second one on another account.
					return true, err
				}
				if progress, ok := numberAsInt(status["progress"]); ok {
					job.setProgress(min(progress, 99))
				}
				state, _ := status["status"].(string)
				switch strings.ToLower(strings.TrimSpace(state)) {
				case "done", "completed", "succeeded", "success", "ready":
					video, _ := status["video"].(map[string]any)
					url, _ := video["url"].(string)
					if strings.TrimSpace(url) == "" {
						generationFailure = "Console video completed without URL"
						return false, nil
					}
					videoURL = url
					contentPath = s.cacheVideo(ctx, job.ID, func(ctx context.Context) (io.ReadCloser, error) {
						return s.Transport.DownloadConsoleVideo(ctx, url)
					})
					return false, nil
				case "failed", "expired", "cancelled", "canceled", "error":
					generationFailure = "Console video generation failed"
					return false, nil
				case "pending", "processing", "in_progress", "queued", "":
				default:
					generationFailure = "invalid Console video status: " + state
					return false, nil
				}
				select {
				case <-ctx.Done():
					return false, ctxError(ctx)
				case <-ticker.C:
				}
			}
		})
	switch {
	case err != nil:
		return failureMessage(stage, err)
	case generationFailure != "":
		return generationFailure
	}
	job.complete(videoURL, contentPath, time.Now())
	return ""
}
