package api

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"

	"github.com/gentpan/grok2api/internal/account"
	"github.com/gentpan/grok2api/internal/platform"
	"github.com/gentpan/grok2api/internal/storage"
)

// maxAccountPageSize is the largest page the account repository serves: it
// clamps ListQuery.PageSize to this value (account/txt.go), so asking for more
// silently truncates. Callers that need every account must paginate.
const maxAccountPageSize = 2000

// maxBatchConcurrency bounds the ?concurrency= knob of the batch endpoints.
const maxBatchConcurrency = 80

// defaultBatchConcurrency is used when ?concurrency= is absent.
const defaultBatchConcurrency = 50

// assetListConcurrency bounds the parallel upstream asset listings.
const assetListConcurrency = 20

// listAllAccounts returns every non-deleted account, paginating through the
// repository's page limit. Ordering is deterministic (UpdatedAt, then token):
// pages are fetched in token order, which is unique, so no record is skipped or
// repeated even when the repository's default sort has ties.
func (s *Server) listAllAccounts(ctx context.Context) ([]*account.Record, error) {
	var all []*account.Record
	for page := 1; ; page++ {
		p, err := s.Repo.ListAccounts(ctx, account.ListQuery{
			Page: page, PageSize: maxAccountPageSize, SortBy: "token",
		})
		if err != nil {
			return nil, err
		}
		all = append(all, p.Items...)
		if page >= p.TotalPages || len(p.Items) == 0 {
			break
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].UpdatedAt != all[j].UpdatedAt {
			return all[i].UpdatedAt < all[j].UpdatedAt
		}
		return all[i].Token < all[j].Token
	})
	return all, nil
}

// sanitizeTokenList normalises tokens and drops blanks and duplicates,
// preserving the first-seen order.
func sanitizeTokenList(raw []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, t := range raw {
		t = platform.SanitizeToken(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

func parseIntQuery(c *gin.Context, key string, def int) int {
	v := c.Query(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return n
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// batchConcurrency reads the ?concurrency= knob of the batch endpoints.
func batchConcurrency(c *gin.Context) int {
	return clampInt(parseIntQuery(c, "concurrency", defaultBatchConcurrency), 1, maxBatchConcurrency)
}

// forEachBounded runs fn for every item with at most conc running at once and
// waits for all of them. A panic in fn is contained to that item.
func forEachBounded[T any](items []T, conc int, fn func(T)) {
	if conc < 1 {
		conc = 1
	}
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	for _, it := range items {
		wg.Add(1)
		sem <- struct{}{}
		go func(it T) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() { _ = recover() }()
			fn(it)
		}(it)
	}
	wg.Wait()
}

// mediaTypeFromName maps the admin API's cache type to a storage media type.
// An empty name means "image" (the panel's default tab).
func mediaTypeFromName(name string) (storage.MediaType, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "image":
		return storage.MediaImage, true
	case "video":
		return storage.MediaVideo, true
	}
	return "", false
}

// mediaDir resolves the on-disk directory of one media type.
func (s *Server) mediaDir(t storage.MediaType) (string, error) {
	if t == storage.MediaVideo {
		return storage.VideoFilesDir()
	}
	return storage.ImageFilesDir()
}
