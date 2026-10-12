package grok

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/gentpan/grok2api/internal/platform"
	tlsclient "github.com/gentpan/grok2api/internal/tlsclient"
)

const (
	// clientBackstopSeconds is the tls-client level hard deadline. It is only a
	// backstop: it also serves as the dial/CONNECT timeout and cannot be 0, and
	// it covers body reads, so it must be longer than any legitimate stream.
	// Real deadlines come from the request context (see Transport.do).
	clientBackstopSeconds = 1800

	// minAutoResetInterval limits how often failures (403s, transport errors)
	// may rebuild the shared client; explicit Reset()/SetProxy() bypass it.
	minAutoResetInterval = 30 * time.Second
)

// clientPool owns one lazily built tls-client together with its proxy and the
// policy for when it must be rebuilt (new proxy, explicit Reset, or a
// failure signal such as a 403). Rebuilding replaces the client and releases
// the keep-alive connections of the old one; requests already in flight on the
// old client run to completion.
type clientPool struct {
	mu       sync.Mutex
	client   *tlsclient.Client
	proxyURL string

	// resetOn is the set of upstream statuses that flag the client for a
	// rebuild. It is immutable after construction.
	resetOn map[int]struct{}

	resetPending bool
	// forceReset marks a reset that must bypass minAutoResetInterval
	// (explicit Reset()/SetProxy()); lastBuild is when client was built.
	forceReset bool
	lastBuild  time.Time
}

func newClientPool(proxyURL string, resetOn map[int]struct{}) *clientPool {
	return &clientPool{proxyURL: proxyURL, resetOn: resetOn}
}

// get returns the current client, rebuilding it first when a reset is due.
func (p *clientPool) get() (*tlsclient.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client != nil {
		if !p.resetPending {
			return p.client, nil
		}
		// Failure-triggered resets are rate limited so a burst of 403s (or a
		// flapping proxy) cannot rebuild the client for every request. The
		// pending flag stays set, so the rebuild happens once the interval ends.
		if !p.forceReset && time.Since(p.lastBuild) < minAutoResetInterval {
			return p.client, nil
		}
	}
	opts := []tlsclient.Option{tlsclient.WithTimeout(clientBackstopSeconds)}
	if p.proxyURL != "" {
		opts = append(opts, tlsclient.WithProxy(p.proxyURL))
	}
	c, err := tlsclient.NewClient(opts...)
	if err != nil {
		return nil, err
	}
	old := p.client
	p.client = c
	p.resetPending = false
	p.forceReset = false
	p.lastBuild = time.Now()
	if old != nil {
		// In-flight requests on the old client keep running; only parked
		// keep-alive connections are released.
		old.CloseIdleConnections()
	}
	return c, nil
}

// setProxy changes the egress proxy; the next get rebuilds the client.
func (p *clientPool) setProxy(proxyURL string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if proxyURL == p.proxyURL {
		return
	}
	p.proxyURL = proxyURL
	p.resetPending = true
	p.forceReset = true
}

// reset forces a rebuild on the next get, bypassing the rate limit.
func (p *clientPool) reset() {
	p.mu.Lock()
	p.resetPending = true
	p.forceReset = true
	p.mu.Unlock()
}

// markReset flags the client for a (rate limited) rebuild.
func (p *clientPool) markReset() {
	p.mu.Lock()
	p.resetPending = true
	p.mu.Unlock()
}

// markResetIfStatus flags the client when status is one of the configured
// reset codes.
func (p *clientPool) markResetIfStatus(status int) {
	if _, ok := p.resetOn[status]; ok {
		p.markReset()
	}
}

// close releases idle connections of the current client.
func (p *clientPool) close() {
	p.mu.Lock()
	c := p.client
	p.mu.Unlock()
	if c != nil {
		c.CloseIdleConnections()
	}
}

// do sends req on the pooled client. This is the one place where transport
// failures and reset-worthy statuses feed the rebuild policy, so every caller
// (API verbs, DPoP token exchange, media download, HTML fetch) gets it.
//
// callerCtx is the caller's own context: a failure caused by the caller going
// away is not evidence that the shared client is broken and does not flag a
// reset.
func (p *clientPool) do(callerCtx context.Context, req *http.Request) (*http.Response, error) {
	client, err := p.get()
	if err != nil {
		return nil, platform.UpstreamError("transport init failed: "+err.Error(), 502, "")
	}
	resp, err := client.Do(req)
	if err != nil {
		if callerCtx.Err() == nil {
			p.markReset()
		}
		return nil, errTransport(err)
	}
	p.markResetIfStatus(resp.StatusCode)
	return resp, nil
}
