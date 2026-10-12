package api

import (
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/gentpan/grok2api/internal/jsonstore"
	"github.com/gentpan/grok2api/internal/logger"
	"github.com/gentpan/grok2api/internal/platform"
)

// usageRetentionDays bounds how much history is kept in memory and on disk.
const usageRetentionDays = 60

// usageSaveDelay is how long counters may stay unflushed.
const usageSaveDelay = 20 * time.Second

// usageDefaultWindowDays is the window of /admin/api/usage without ?days=.
const usageDefaultWindowDays = 14

const (
	// usageModelKey is the gin context key handlers use to report the model a
	// request was for (the middleware cannot see inside the body cheaply).
	usageModelKey = "usage_model"
	// usageFailedKey lets a handler flag a request as failed although the HTTP
	// status is 200 (a stream that broke after its headers were sent).
	usageFailedKey = "usage_failed"
)

type modelStat struct {
	Requests int `json:"requests"`
	Errors   int `json:"errors"`
}

type dayStat struct {
	Requests int                   `json:"requests"`
	Errors   int                   `json:"errors"`
	TotalMs  int64                 `json:"total_ms"`
	ByModel  map[string]*modelStat `json:"by_model"`
}

// usageFile is the persisted shape: {"since":"2006-01-02","days":{...}}.
type usageFile struct {
	Since string              `json:"since"`
	Days  map[string]*dayStat `json:"days"`
}

// usageRecorder keeps per-day request counters, persisted as one JSON file.
type usageRecorder struct {
	f   *jsonstore.File[usageFile]
	now func() time.Time
}

func newUsageRecorder(path string) *usageRecorder {
	r := &usageRecorder{now: time.Now}
	f, err := jsonstore.Open(path, jsonstore.Config[usageFile]{
		Debounce: usageSaveDelay,
		OnError:  func(err error) { logger.Warnf("usage stats persist failed: error=%v", err) },
		Normalize: func(d *usageFile) {
			if d.Days == nil {
				d.Days = map[string]*dayStat{}
			}
			if d.Since == "" {
				d.Since = time.Now().Format("2006-01-02")
			}
		},
	})
	if err != nil {
		logger.Warnf("usage stats unreadable, starting empty: error=%v", err)
	}
	r.f = f
	return r
}

// record counts one finished request.
func (r *usageRecorder) record(modelName string, status int, d time.Duration) {
	if modelName == "" {
		modelName = "(unknown)"
	}
	day := r.now().Format("2006-01-02")
	failed := status >= 400
	r.f.Mutate(func(u *usageFile) {
		ds := u.Days[day]
		if ds == nil {
			ds = &dayStat{ByModel: map[string]*modelStat{}}
			u.Days[day] = ds
			r.pruneLocked(u)
		}
		if ds.ByModel == nil {
			ds.ByModel = map[string]*modelStat{}
		}
		ds.Requests++
		ds.TotalMs += d.Milliseconds()
		ms := ds.ByModel[modelName]
		if ms == nil {
			ms = &modelStat{}
			ds.ByModel[modelName] = ms
		}
		ms.Requests++
		if failed {
			ds.Errors++
			ms.Errors++
		}
	})
}

func (r *usageRecorder) pruneLocked(u *usageFile) {
	cutoff := r.now().AddDate(0, 0, -usageRetentionDays).Format("2006-01-02")
	for d := range u.Days {
		if d < cutoff {
			delete(u.Days, d)
		}
	}
}

// flush writes pending counters now.
func (r *usageRecorder) flush() {
	if r == nil {
		return
	}
	if err := r.f.Flush(); err != nil {
		logger.Warnf("usage stats persist failed: error=%v", err)
	}
}

// snapshot returns the last n days (oldest first) and per-model totals.
func (r *usageRecorder) snapshot(n int) gin.H {
	if n <= 0 || n > usageRetentionDays {
		n = usageDefaultWindowDays
	}
	var out gin.H
	r.f.View(func(u *usageFile) {
		days := make([]gin.H, 0, n)
		totals := map[string]*modelStat{}
		var reqs, errs int
		var totalMs int64
		now := r.now()
		for i := n - 1; i >= 0; i-- {
			d := now.AddDate(0, 0, -i).Format("2006-01-02")
			ds := u.Days[d]
			row := gin.H{"date": d, "requests": 0, "errors": 0, "avg_ms": 0}
			if ds != nil {
				avg := int64(0)
				if ds.Requests > 0 {
					avg = ds.TotalMs / int64(ds.Requests)
				}
				row = gin.H{"date": d, "requests": ds.Requests, "errors": ds.Errors, "avg_ms": avg}
				reqs += ds.Requests
				errs += ds.Errors
				totalMs += ds.TotalMs
				for m, st := range ds.ByModel {
					t := totals[m]
					if t == nil {
						t = &modelStat{}
						totals[m] = t
					}
					t.Requests += st.Requests
					t.Errors += st.Errors
				}
			}
			days = append(days, row)
		}
		models := make([]gin.H, 0, len(totals))
		for m, st := range totals {
			models = append(models, gin.H{"model": m, "requests": st.Requests, "errors": st.Errors})
		}
		sort.Slice(models, func(i, j int) bool {
			a, b := models[i]["requests"].(int), models[j]["requests"].(int)
			if a != b {
				return a > b
			}
			return models[i]["model"].(string) < models[j]["model"].(string)
		})
		avg := int64(0)
		if reqs > 0 {
			avg = totalMs / int64(reqs)
		}
		out = gin.H{
			"since":  u.Since,
			"window": n,
			"totals": gin.H{"requests": reqs, "errors": errs, "avg_ms": avg},
			"days":   days,
			"models": models,
		}
	})
	return out
}

// noteUsageModel lets a handler report which model the request targets.
func noteUsageModel(c *gin.Context, name string) {
	if name != "" {
		c.Set(usageModelKey, name)
	}
}

// markUsageFailed flags the request as failed for the usage counters even
// though its HTTP status cannot say so any more (a stream that broke after
// the 200 header).
func markUsageFailed(c *gin.Context) { c.Set(usageFailedKey, true) }

// usageMiddleware records every request that reaches an API handler.
func (s *Server) usageMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		if s.Usage == nil {
			return
		}
		// Only count calls that actually did work (POST generation endpoints).
		if c.Request.Method != http.MethodPost {
			return
		}
		name, _ := c.Get(usageModelKey)
		modelName, _ := name.(string)
		if modelName == "" {
			return // rejected before a model was known (e.g. bad JSON)
		}
		status := c.Writer.Status()
		if failed, _ := c.Get(usageFailedKey); failed == true && status < 400 {
			status = http.StatusBadGateway
		}
		s.Usage.record(modelName, status, time.Since(start))
	}
}

// handleUsageGet serves GET /admin/api/usage?days=N.
func (s *Server) handleUsageGet(c *gin.Context) {
	n := usageDefaultWindowDays
	if v := c.Query("days"); v != "" {
		if x, err := strconv.Atoi(v); err == nil {
			n = x
		}
	}
	if s.Usage == nil {
		writeAppError(c, platform.NewAppError("usage stats unavailable", platform.ErrServer, "usage_unavailable", http.StatusServiceUnavailable))
		return
	}
	c.JSON(http.StatusOK, s.Usage.snapshot(n))
}
