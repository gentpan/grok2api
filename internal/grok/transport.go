package grok

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gentpan/grok2api/internal/config"
	"github.com/gentpan/grok2api/internal/platform"
)

// Transport is the HTTP client for upstream Grok requests. It uses
// tlsclient.Client for TLS fingerprinting (Chrome_146) and converts
// everything to standard *http.Response so callers never see fhttp.
//
// Responsibilities are split over small collaborators:
//
//	pool     shared tls-client, proxy and rebuild policy   (client_pool.go)
//	dpop     Console DPoP session cache                    (dpop_store.go)
//	statsig  x-statsig-id source                           (statsig_source.go)
//
// Transport itself only builds requests (headers per host class) and maps
// responses to results and errors.
type Transport struct {
	pool *clientPool
	// htmlPool serves the anonymous grok.com page fetch used for statsig. It
	// is separate so the page's Set-Cookie headers stay out of the jar that
	// authenticated API requests share.
	htmlPool *clientPool
	dpop     *dpopStore
	statsig  *statsigSource

	// baseOverride replaces Base for the Gateway and the HTML fetch. Tests
	// only.
	baseOverride string
}

const (
	// maxFetchBytes caps bodies fetched from caller-influenced URLs.
	maxFetchBytes = 30 << 20

	// maxJSONResponseBytes caps a buffered JSON response.
	maxJSONResponseBytes = 64 << 20

	// defaultRequestTimeout bounds a request when WithTimeout is not given.
	defaultRequestTimeout = 120 * time.Second
)

// NewTransport builds a Transport from config.
func NewTransport() (*Transport, error) {
	proxyURL := egressProxyURL()
	reset := resetStatusCodes(config.Global().GetList("retry.reset_session_status_codes", []string{"403"}))
	return &Transport{
		pool:     newClientPool(proxyURL, reset),
		htmlPool: newClientPool(proxyURL, reset),
		dpop:     newDPoPStore(),
		statsig:  newStatsigSource(),
	}, nil
}

// egressProxyURL resolves the upstream proxy: PROXY_HTTP overrides
// proxy.egress.proxy_url. This is the single proxy source for every upstream
// connection (HTTP, Gateway WebSocket, imagine WebSocket, HTML fetch).
func egressProxyURL() string {
	proxyURL := strings.TrimSpace(config.Global().GetStr("proxy.egress.proxy_url", ""))
	if env := strings.TrimSpace(os.Getenv("PROXY_HTTP")); env != "" {
		proxyURL = env
	}
	return normalizeProxyURL(proxyURL)
}

func resetStatusCodes(codes []string) map[int]struct{} {
	reset := make(map[int]struct{}, len(codes))
	for _, c := range codes {
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(c), "%d", &n); err == nil {
			reset[n] = struct{}{}
		}
	}
	return reset
}

// SetProxy changes the egress proxy; connections are rebuilt on next use.
func (t *Transport) SetProxy(proxyURL string) {
	n := normalizeProxyURL(strings.TrimSpace(proxyURL))
	t.pool.setProxy(n)
	t.htmlPool.setProxy(n)
}

// ReloadConfig re-reads the runtime-changeable settings from config (today the
// egress proxy). It is cheap and idempotent: an unchanged proxy keeps the
// current connections. retry.reset_session_status_codes is read once at
// construction and needs a restart.
func (t *Transport) ReloadConfig() {
	t.SetProxy(egressProxyURL())
}

// Reset discards the shared client so the next request opens fresh connections.
func (t *Transport) Reset() {
	t.pool.reset()
	t.htmlPool.reset()
}

// Close releases idle upstream connections. The Transport stays usable.
func (t *Transport) Close() {
	t.pool.close()
	t.htmlPool.close()
}

// baseURL is the grok.com origin (Base unless a test overrides it).
func (t *Transport) baseURL() string {
	if t.baseOverride != "" {
		return t.baseOverride
	}
	return Base
}

// --- request pipeline ------------------------------------------------------

// cancelOnClose ties a request context's cancel func to the lifetime of the
// response body, so the context stays alive while the caller reads (streams)
// and is released when the body is closed.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (b *cancelOnClose) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.cancel)
	return err
}

// headerDeadline bounds only the wait for response headers of a stream. A
// plain time.AfterFunc(cancel) races: it can fire between Do returning a
// healthy response and Stop(), cutting the body of a good stream. Here stop
// reports whether the deadline had already fired, so the caller decides
// deterministically.
type headerDeadline struct {
	mu      sync.Mutex
	timer   *time.Timer
	stopped bool
	fired   bool
}

func startHeaderDeadline(d time.Duration, cancel context.CancelFunc) *headerDeadline {
	h := &headerDeadline{}
	h.timer = time.AfterFunc(d, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.stopped {
			return
		}
		h.fired = true
		cancel()
	})
	return h
}

// stop disarms the deadline. It returns false when the deadline already
// fired (and cancelled the request context).
func (h *headerDeadline) stop() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopped = true
	h.timer.Stop()
	return !h.fired
}

// do builds and sends one request with the caller's context.
//
// For non-stream requests o.timeout bounds the whole exchange including the
// body read. For stream requests o.timeout only bounds the wait for response
// headers; the body then lives until the caller's context ends or the returned
// body is closed. body == nil means no request body.
func (t *Transport) do(ctx context.Context, method, urlStr, token string, body []byte, o reqOpts, stream bool) (*http.Response, error) {
	var (
		reqCtx   context.Context
		cancel   context.CancelFunc
		deadline *headerDeadline
	)
	switch {
	case o.timeout <= 0:
		reqCtx, cancel = context.WithCancel(ctx)
	case stream:
		reqCtx, cancel = context.WithCancel(ctx)
		deadline = startHeaderDeadline(o.timeout, cancel)
	default:
		reqCtx, cancel = context.WithTimeout(ctx, o.timeout)
	}
	fail := func(err error) (*http.Response, error) {
		if deadline != nil {
			deadline.stop()
		}
		cancel()
		return nil, err
	}

	profile := resolveProxyProfile()
	attempts := 1
	if o.consoleMode {
		// A rejected DPoP session is dropped and the request retried once.
		attempts = 2
	}
	for attempt := 0; attempt < attempts; attempt++ {
		resp, sessionToken, err := t.doOnce(reqCtx, ctx, method, urlStr, token, body, o, profile)
		if err != nil {
			return fail(err)
		}
		retry := o.consoleMode && resp.StatusCode == http.StatusUnauthorized && attempt+1 < attempts
		if !retry {
			if deadline != nil && !deadline.stop() {
				_ = resp.Body.Close()
				cancel()
				return nil, errTransport(context.DeadlineExceeded)
			}
			resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
			return resp, nil
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		t.invalidateDPoPSession(token, consoleRequestBase(urlStr), sessionToken)
	}
	return fail(platform.UpstreamError("Console DPoP retry state invalid", 502, ""))
}

// cookieHostTrusted reports whether SSO/cf cookies and statsig headers may be
// sent to u. It is a variable so tests can allow a local httptest server.
var cookieHostTrusted = defaultCookieHostTrusted

func defaultCookieHostTrusted(u *url.URL) bool {
	if u == nil || !strings.EqualFold(u.Scheme, "https") {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	for _, domain := range []string{"grok.com", "x.ai"} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

// buildRequest assembles the *http.Request for one attempt. The header set is
// chosen by host class: Console (DPoP, refused for untrusted hosts), trusted
// grok.com/x.ai REST (cookies + statsig), or plain (no credentials at all).
func (t *Transport) buildRequest(ctx context.Context, method, urlStr, token string, payload []byte, o reqOpts, profile proxyProfile) (*http.Request, error) {
	parsedURL, _ := url.Parse(urlStr)
	trusted := cookieHostTrusted(parsedURL)

	var headers http.Header
	switch {
	case o.consoleMode && !trusted:
		return nil, errUntrustedHost()
	case o.consoleMode:
		headers = profile.consoleHeaders(token, o.contentType)
	case trusted:
		sid := t.statsig.id(profile, pathOf(urlStr), method, t.fetchStatsigPairFromHTML)
		headers = profile.httpHeaders(token, o.contentType, sid)
	default:
		// Not a grok.com / x.ai host: never attach SSO/cf cookies or statsig.
		headers = profile.plainHeaders(o.contentType, payload != nil)
	}
	for k, vs := range o.extraHeaders {
		for _, v := range vs {
			headers.Set(k, v)
		}
	}

	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, urlStr, body)
	if err != nil {
		return nil, platform.UpstreamError("build request failed: "+err.Error(), 502, "")
	}
	req.Header = headers
	return req, nil
}

// doOnce sends one attempt. ctx is the per-request context; callerCtx is the
// caller's own context, used to avoid treating a client-side cancellation as a
// transport failure worth resetting the shared client for.
func (t *Transport) doOnce(ctx, callerCtx context.Context, method, urlStr, token string, payload []byte, o reqOpts, profile proxyProfile) (*http.Response, string, error) {
	req, err := t.buildRequest(ctx, method, urlStr, token, payload, o, profile)
	if err != nil {
		return nil, "", err
	}
	var sessionToken string
	if o.consoleMode {
		session, err := t.getDPoPSession(ctx, token, consoleRequestBase(urlStr), profile)
		if err != nil {
			return nil, "", err
		}
		if !strings.HasSuffix(req.URL.Path, "/responses") {
			req.Header.Del("x-cluster")
		}
		if err := applyDPoPAuthorization(req, session); err != nil {
			return nil, "", err
		}
		sessionToken = session.accessToken
	}
	resp, err := t.pool.do(callerCtx, req)
	if err != nil {
		return nil, "", err
	}
	return resp, sessionToken, nil
}

// --- helpers ---------------------------------------------------------------

// truncUTF8 shortens s to at most n bytes without splitting a UTF-8 rune.
func truncUTF8(s string, n int) string {
	if n < 0 {
		n = 0
	}
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func normalizeProxyURL(u string) string {
	if u == "" {
		return ""
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return u
	}
	switch strings.ToLower(parsed.Scheme) {
	case "socks":
		return "socks5h://" + strings.TrimPrefix(u, "socks://")
	case "socks5":
		return "socks5h://" + strings.TrimPrefix(u, "socks5://")
	case "socks4":
		return "socks4a://" + strings.TrimPrefix(u, "socks4://")
	}
	return u
}
