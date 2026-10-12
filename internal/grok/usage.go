package grok

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/gentpan/grok2api/internal/account"
	"github.com/gentpan/grok2api/internal/platform"
)

// usageModes are the quota windows polled for an account, in mode-id order.
var usageModes = []int{0, 1, 2, 3, 4}

// modeNames maps modeID -> the request modelName for /rest/rate-limits.
var modeNames = map[int]string{
	0: "auto",
	1: "fast",
	2: "expert",
	3: "heavy",
	4: "grok-420-computer-use-sa",
}

// defaultWindowSecs is the fallback window duration when the API omits it.
var defaultWindowSecs = map[int]int{
	0: 7200,  // auto   — 2 h
	1: 86400, // fast   — 24 h
	2: 7200,  // expert — 2 h
	3: 7200,  // heavy  — 2 h
	4: 7200,  // grok_4_3 — 2 h
}

const (
	// usageFallbackWindowSecs applies to modes absent from defaultWindowSecs.
	usageFallbackWindowSecs = 72000
	// usageRequestTimeout bounds one rate-limit call (per the Python
	// implementation).
	usageRequestTimeout = 25 * time.Second
	usageHTTPTimeout    = 20 * time.Second
)

// parseRateLimits converts the flat rate-limits body to a ModeQuota. Returns
// nil when required fields are missing.
func parseRateLimits(body map[string]any, fallbackWindowSecs int) *account.ModeQuota {
	remaining, ok := body["remainingQueries"].(float64)
	if !ok {
		return nil
	}
	total := int(remaining)
	if t, ok := body["totalQueries"].(float64); ok {
		total = int(t)
	}
	window := fallbackWindowSecs
	if w, ok := body["windowSizeSeconds"].(float64); ok && int(w) > 0 {
		window = int(w)
	}
	syncedAt := platform.NowMs()
	resetAt := syncedAt + int64(window)*1000
	return &account.ModeQuota{
		Remaining: int(remaining),
		Total:     total,
		WindowSec: window,
		ResetAtMs: &resetAt,
	}
}

// UsageFetcher implements account.UsageFetcher by hitting /rest/rate-limits.
type UsageFetcher struct {
	transport *Transport
}

// NewUsageFetcher wraps a Transport with the UsageFetcher interface.
func NewUsageFetcher(t *Transport) *UsageFetcher { return &UsageFetcher{transport: t} }

// FetchAllQuotas fetches quota windows for all supported modes concurrently.
// It returns {modeID: ModeQuota} for every successful response, or nil if no
// mode produced data. A single invalid-credentials error short-circuits and is
// returned so the caller can mark the account expired.
func (f *UsageFetcher) FetchAllQuotas(ctx context.Context, token, _ string, _ bool) (map[int]account.ModeQuota, error) {
	type result struct {
		quota *account.ModeQuota
		err   error
	}
	results := make([]result, len(usageModes))
	var wg sync.WaitGroup
	for i, modeID := range usageModes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q, err := f.fetchOne(ctx, token, modeID)
			results[i] = result{quota: q, err: err}
		}()
	}
	wg.Wait()

	// Surface the first invalid-credentials error to the caller.
	for _, r := range results {
		if r.err != nil && isInvalidCredentials(r.err) {
			return nil, r.err
		}
	}
	out := map[int]account.ModeQuota{}
	for i, r := range results {
		if r.quota != nil {
			out[usageModes[i]] = *r.quota
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// FetchModeQuota fetches the quota for a single mode.
func (f *UsageFetcher) FetchModeQuota(ctx context.Context, token, _ string, modeID int) (*account.ModeQuota, error) {
	return f.fetchOne(ctx, token, modeID)
}

func (f *UsageFetcher) fetchOne(ctx context.Context, token string, modeID int) (*account.ModeQuota, error) {
	modeName := modeNames[modeID]
	if modeName == "" {
		modeName = "auto"
	}
	payload := []byte(fmt.Sprintf(`{"modelName":%q}`, modeName))
	reqCtx, cancel := context.WithTimeout(ctx, usageRequestTimeout)
	defer cancel()
	body, err := f.transport.PostJSON(reqCtx, RateLimits, token, payload, WithTimeout(usageHTTPTimeout))
	if err != nil {
		return nil, err
	}
	fallback := defaultWindowSecs[modeID]
	if fallback == 0 {
		fallback = usageFallbackWindowSecs
	}
	// A body lacking the required fields yields (nil, nil): "no data".
	return parseRateLimits(body, fallback), nil
}
