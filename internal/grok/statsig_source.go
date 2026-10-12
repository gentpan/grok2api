package grok

import (
	"context"
	"encoding/base64"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/gentpan/grok2api/internal/grok/statsig"
)

const (
	// statsigRefreshInterval is the minimum gap between HTML refresh attempts.
	statsigRefreshInterval = 30 * time.Minute
	// statsigRefreshTimeout bounds one background HTML fetch.
	statsigRefreshTimeout = 30 * time.Second
)

// statsigFetchFunc fetches a fresh (seed, HEX) pair from grok.com's HTML. A
// zero result (empty seed) means the page carried none.
type statsigFetchFunc func(ctx context.Context, p proxyProfile) (seed, hex string, err error)

// statsigSource decides which (seed, HEX) pair x-statsig-id values are minted
// from and keeps it fresh. It replaces what used to be three layers of
// process-global caches.
//
// Priority:
//  1. proxy.clearance.statsig_id — a full fixed value (manual override).
//  2. proxy.clearance.statsig_seed + statsig_hex — a configured genuine pair.
//     Authoritative: the HTML refresh is not run while it is set.
//  3. proxy.clearance.statsig_from_html — a pair derived from the seed in
//     grok.com's <meta name="grok-site‑verification">, refreshed in the
//     background at most every statsigRefreshInterval.
//  4. The embedded default pair of the statsig package.
//  5. A legacy error-format fake if generation fails.
type statsigSource struct {
	gen *statsig.Generator

	mu          sync.Mutex
	cfgSeed     string // config pair last applied
	cfgHEX      string
	htmlSeed    string // HTML pair last applied
	htmlHEX     string
	lastAttempt time.Time
	refreshing  bool
	wg          sync.WaitGroup
}

func newStatsigSource() *statsigSource {
	return &statsigSource{gen: statsig.NewGenerator()}
}

// id returns the x-statsig-id for (pathname, method). It never blocks on the
// network: when an HTML refresh is due it is started in the background and the
// current pair is used for this request.
func (s *statsigSource) id(p proxyProfile, pathname, method string, fetch statsigFetchFunc) string {
	if p.StatsigID != "" {
		return p.StatsigID
	}
	switch {
	case p.StatsigSeed != "" && p.StatsigHEX != "":
		s.applyConfigPair(p.StatsigSeed, p.StatsigHEX)
	case p.StatsigFromHTML && fetch != nil:
		s.maybeRefresh(p, fetch)
	}
	if pathname == "" {
		pathname = "/rest/app-chat/conversations/new"
	}
	if method == "" {
		method = "POST"
	}
	if v, err := s.gen.Generate(pathname, method, time.Now().Unix()); err == nil && v != "" {
		return v
	}
	return statsigFallback()
}

// applyConfigPair pushes a configured pair into the generator when it changed.
func (s *statsigSource) applyConfigPair(seed, hex string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if seed == s.cfgSeed && hex == s.cfgHEX {
		return
	}
	if err := s.gen.SetPair(seed, hex); err == nil {
		s.cfgSeed, s.cfgHEX = seed, hex
	}
}

// maybeRefresh starts a background refresh when one is due and none is
// running. The mutex is held only for the bookkeeping, never during IO.
func (s *statsigSource) maybeRefresh(p proxyProfile, fetch statsigFetchFunc) {
	s.mu.Lock()
	if s.refreshing || (!s.lastAttempt.IsZero() && time.Since(s.lastAttempt) < statsigRefreshInterval) {
		s.mu.Unlock()
		return
	}
	s.refreshing = true
	s.lastAttempt = time.Now()
	s.wg.Add(1)
	s.mu.Unlock()

	go func() {
		defer s.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), statsigRefreshTimeout)
		defer cancel()
		seed, hex, err := fetch(ctx, p)

		s.mu.Lock()
		defer s.mu.Unlock()
		s.refreshing = false
		if err != nil || seed == "" || hex == "" {
			return
		}
		if seed == s.htmlSeed && hex == s.htmlHEX {
			return
		}
		if err := s.gen.SetPair(seed, hex); err == nil {
			s.htmlSeed, s.htmlHEX = seed, hex
		}
	}()
}

// wait blocks until background refreshes have finished (tests, shutdown).
func (s *statsigSource) wait() { s.wg.Wait() }

// statsigFallback is the legacy error-format value used only if real
// generation fails (kept so requests still carry a plausibly-shaped header).
func statsigFallback() string {
	var msg string
	if randInt(2) == 0 {
		r := randString(5, lowerAlphaDigits)
		msg = fmt.Sprintf("x1:TypeError: Cannot read properties of null (reading 'children[\\'%s\\']')", r)
	} else {
		r := randString(10, lowerAlpha)
		msg = fmt.Sprintf("x1:TypeError: Cannot read properties of undefined (reading '%s')", r)
	}
	return base64.StdEncoding.EncodeToString([]byte(msg))
}

const (
	lowerAlpha       = "abcdefghijklmnopqrstuvwxyz"
	lowerAlphaDigits = "abcdefghijklmnopqrstuvwxyz0123456789"
)

func randInt(n int) int {
	if n <= 0 {
		return 0
	}
	return rand.IntN(n)
}

func randString(n int, charset string) string {
	if n <= 0 || charset == "" {
		return ""
	}
	var sb strings.Builder
	for i := 0; i < n; i++ {
		sb.WriteByte(charset[randInt(len(charset))])
	}
	return sb.String()
}
