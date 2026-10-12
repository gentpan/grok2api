package api

import (
	"fmt"
	"sync"
	"time"
)

// videoJobRetention is how long finished jobs stay queryable in memory.
const videoJobRetention = time.Hour

// videoJobError is the failure detail of a job.
type videoJobError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// videoJob is one asynchronous video generation. It is owned by the caller
// that created it: only that caller may read it back.
type videoJob struct {
	mu sync.RWMutex

	ID          string
	Object      string
	CreatedAt   int64
	Status      string
	Model       string
	Progress    int
	Prompt      string
	Seconds     int
	Size        string
	Quality     string
	VideoURL    string
	CompletedAt *int64
	Error       *videoJobError

	// Owner is Caller.OwnerID() of the creator.
	Owner       string
	contentPath string
}

// Video job states.
const (
	videoQueued     = "queued"
	videoInProgress = "in_progress"
	videoCompleted  = "completed"
	videoFailed     = "failed"
)

// videoJobs is the in-memory job registry of one Server.
type videoJobs struct {
	mu        sync.RWMutex
	m         map[string]*videoJob
	retention time.Duration
	now       func() time.Time
}

func newVideoJobs(retention time.Duration) *videoJobs {
	if retention <= 0 {
		retention = videoJobRetention
	}
	return &videoJobs{m: map[string]*videoJob{}, retention: retention, now: time.Now}
}

// add registers a job, evicting finished jobs past their retention first so
// the map cannot grow forever.
func (v *videoJobs) add(job *videoJob) {
	v.mu.Lock()
	defer v.mu.Unlock()
	cutoff := v.now().Add(-v.retention).Unix()
	for id, j := range v.m {
		j.mu.RLock()
		done := j.CompletedAt != nil && *j.CompletedAt < cutoff
		j.mu.RUnlock()
		if done {
			delete(v.m, id)
		}
	}
	v.m[job.ID] = job
}

// get returns the job with the given id if it exists and belongs to owner.
// A job of another owner is indistinguishable from a missing one.
func (v *videoJobs) get(id, owner string) *videoJob {
	v.mu.RLock()
	defer v.mu.RUnlock()
	j := v.m[id]
	if j == nil || j.Owner != owner {
		return nil
	}
	return j
}

func (j *videoJob) setProgress(n int) {
	j.mu.Lock()
	if n > j.Progress {
		j.Progress = n
	}
	j.mu.Unlock()
}

func (j *videoJob) start() {
	j.mu.Lock()
	j.Status = videoInProgress
	j.Progress = 1
	j.mu.Unlock()
}

// complete marks the job finished. contentPath is the local copy ("" when the
// video was not cached).
func (j *videoJob) complete(videoURL, contentPath string, now time.Time) {
	ts := now.Unix()
	j.mu.Lock()
	j.Status = videoCompleted
	j.Progress = 100
	j.CompletedAt = &ts
	j.VideoURL = videoURL
	j.contentPath = contentPath
	j.mu.Unlock()
}

func (j *videoJob) fail(message string, now time.Time) {
	ts := now.Unix()
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.Status == videoCompleted || j.Status == videoFailed {
		return
	}
	j.Status = videoFailed
	j.CompletedAt = &ts
	j.Error = &videoJobError{Code: "video_generation_failed", Message: message}
}

// content returns the status and local content path.
func (j *videoJob) content() (status, path string) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.Status, j.contentPath
}

// toDict renders the job in the OpenAI video object shape.
func (j *videoJob) toDict() map[string]any {
	j.mu.RLock()
	defer j.mu.RUnlock()
	m := map[string]any{
		"id": j.ID, "object": j.Object, "created_at": j.CreatedAt,
		"status": j.Status, "model": j.Model, "progress": j.Progress,
		"prompt": j.Prompt, "seconds": fmt.Sprintf("%d", j.Seconds),
		"size": j.Size, "quality": j.Quality,
	}
	if j.VideoURL != "" {
		m["video_url"] = j.VideoURL
	}
	if j.CompletedAt != nil {
		m["completed_at"] = *j.CompletedAt
	}
	if j.Error != nil {
		m["error"] = j.Error
	}
	return m
}
