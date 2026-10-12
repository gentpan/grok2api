package grok

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gentpan/grok2api/internal/platform"
)

func statusServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func resetPending(p *clientPool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.resetPending
}

func appErrorOf(t *testing.T, err error) *platform.AppError {
	t.Helper()
	appErr, ok := asAppError(err)
	if !ok {
		t.Fatalf("err = %v (%T), want *platform.AppError", err, err)
	}
	return appErr
}

// Every verb shares one status -> error mapping and one reset policy.
func TestEveryVerbMapsNonSuccessAndFlagsReset(t *testing.T) {
	srv := statusServer(t, 403, `{"error":"blocked","detail":"中文"}`)
	ctx := context.Background()

	verbs := map[string]func(*Transport) error{
		"PostJSON":   func(tr *Transport) error { _, err := tr.PostJSON(ctx, srv.URL, "tok", []byte(`{}`)); return err },
		"GetJSON":    func(tr *Transport) error { _, err := tr.GetJSON(ctx, srv.URL, "tok"); return err },
		"DeleteJSON": func(tr *Transport) error { _, err := tr.DeleteJSON(ctx, srv.URL, "tok"); return err },
		"PostStream": func(tr *Transport) error { _, err := tr.PostStream(ctx, srv.URL, "tok", []byte(`{}`)); return err },
		"GetBytes":   func(tr *Transport) error { _, err := tr.GetBytes(ctx, srv.URL, "tok"); return err },
		"PostGRPCWeb": func(tr *Transport) error {
			_, _, err := tr.PostGRPCWeb(ctx, srv.URL, "tok", []byte{0, 0, 0, 0, 0})
			return err
		},
	}
	for name, call := range verbs {
		t.Run(name, func(t *testing.T) {
			tr := newTestTransport(t)
			appErr := appErrorOf(t, call(tr))
			if appErr.Status != 403 || !strings.HasPrefix(appErr.Message, "Upstream returned 403") {
				t.Fatalf("err = %#v", appErr)
			}
			if !strings.Contains(appErr.Body, "blocked") {
				t.Fatalf("body not carried: %q", appErr.Body)
			}
			if !resetPending(tr.pool) {
				t.Fatal("403 did not flag the client for reset")
			}
		})
	}
}

func TestStreamErrorMessageEchoesBodyOthersDoNot(t *testing.T) {
	srv := statusServer(t, 500, "boom")
	tr := newTestTransport(t)
	_, err := tr.PostStream(context.Background(), srv.URL, "tok", []byte(`{}`))
	if got := appErrorOf(t, err).Message; got != "Upstream returned 500: boom" {
		t.Fatalf("stream message = %q", got)
	}
	_, err = tr.GetJSON(context.Background(), srv.URL, "tok")
	if got := appErrorOf(t, err).Message; got != "Upstream returned 500" {
		t.Fatalf("json message = %q", got)
	}
	if resetPending(tr.pool) {
		t.Fatal("500 flagged a reset although only 403 is configured")
	}
}

func TestResetStatusCodesComeFromConfig(t *testing.T) {
	useConfig(t, "[retry]\nreset_session_status_codes = [\"429\", \"x\", \" 503 \"]\n")
	srv := statusServer(t, 429, "slow")
	tr := newTestTransport(t)
	if _, err := tr.GetJSON(context.Background(), srv.URL, "tok"); err == nil {
		t.Fatal("expected error")
	}
	if !resetPending(tr.pool) {
		t.Fatal("configured status 429 did not flag a reset")
	}
	if _, ok := tr.pool.resetOn[503]; !ok {
		t.Fatalf("resetOn = %v", tr.pool.resetOn)
	}
}

func TestSuccessDoesNotFlagReset(t *testing.T) {
	srv := statusServer(t, 200, `{"ok":true}`)
	tr := newTestTransport(t)
	out, err := tr.GetJSON(context.Background(), srv.URL, "tok")
	if err != nil || out["ok"] != true {
		t.Fatalf("out=%v err=%v", out, err)
	}
	if resetPending(tr.pool) {
		t.Fatal("200 flagged a reset")
	}
}

func TestEmptyAndInvalidJSONBodies(t *testing.T) {
	empty := statusServer(t, 200, "  ")
	tr := newTestTransport(t)
	out, err := tr.GetJSON(context.Background(), empty.URL, "tok")
	if err != nil || len(out) != 0 {
		t.Fatalf("empty body: out=%v err=%v", out, err)
	}
	bad := statusServer(t, 200, "<html>")
	_, err = tr.GetJSON(context.Background(), bad.URL, "tok")
	appErr := appErrorOf(t, err)
	if !strings.HasPrefix(appErr.Message, "failed to decode JSON body") || appErr.Body != "<html>" {
		t.Fatalf("err = %#v", appErr)
	}
}

// Console DPoP token exchange, the video download path and the HTML fetch all
// go through the same pool, so a 403 there flags the right client too.
func TestDPoPTokenExchangeFlagsResetOn403(t *testing.T) {
	srv := statusServer(t, 403, `{"error":"no"}`)
	allowLocalCookieHosts(t)
	tr := newTestTransport(t)
	_, err := tr.PostJSON(context.Background(), srv.URL+"/v1/images/generations", "tok", []byte(`{}`), WithConsoleMode())
	appErr := appErrorOf(t, err)
	if appErr.Status != 403 || !strings.Contains(appErr.Message, "DPoP token endpoint returned 403") {
		t.Fatalf("err = %#v", appErr)
	}
	if !resetPending(tr.pool) {
		t.Fatal("DPoP token 403 did not flag the client for reset")
	}
	if tr.dpop.size() != 0 {
		t.Fatal("failed exchange cached a session")
	}
}

func TestStatsigHTMLFetchUsesItsOwnPoolAndFlagsReset(t *testing.T) {
	srv := statusServer(t, 403, "<html>challenge</html>")
	tr := newTestTransport(t)
	tr.baseOverride = srv.URL
	seed, hex, err := tr.fetchStatsigPairFromHTML(context.Background(), proxyProfile{})
	if err != nil || seed != "" || hex != "" {
		t.Fatalf("seed=%q hex=%q err=%v", seed, hex, err)
	}
	if !resetPending(tr.htmlPool) {
		t.Fatal("HTML fetch 403 did not flag its pool")
	}
	if resetPending(tr.pool) {
		t.Fatal("HTML fetch flagged the API pool")
	}
}

func TestHTMLFetchJarIsIsolatedFromAPIClient(t *testing.T) {
	var cookie atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.SetCookie(w, &http.Cookie{Name: "anon", Value: "from-html", Path: "/"})
			_, _ = w.Write([]byte("<html></html>"))
			return
		}
		cookie.Store(r.Header.Get("Cookie"))
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()
	tr := newTestTransport(t)
	tr.baseOverride = srv.URL
	if _, _, err := tr.fetchStatsigPairFromHTML(context.Background(), proxyProfile{}); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.GetJSON(context.Background(), srv.URL+"/api", "tok"); err != nil {
		t.Fatal(err)
	}
	if c, _ := cookie.Load().(string); strings.Contains(c, "anon") {
		t.Fatalf("HTML page cookie leaked into API request: %q", c)
	}
}

func TestSetProxyAndResetApplyToBothPools(t *testing.T) {
	tr := newTestTransport(t)
	tr.SetProxy("socks5://127.0.0.1:1080")
	for name, p := range map[string]*clientPool{"api": tr.pool, "html": tr.htmlPool} {
		p.mu.Lock()
		got, pending, force := p.proxyURL, p.resetPending, p.forceReset
		p.mu.Unlock()
		if got != "socks5h://127.0.0.1:1080" || !pending || !force {
			t.Errorf("%s pool: proxy=%q pending=%v force=%v", name, got, pending, force)
		}
	}
	// Re-applying the same proxy is not a reset.
	tr.pool.mu.Lock()
	tr.pool.resetPending, tr.pool.forceReset = false, false
	tr.pool.mu.Unlock()
	tr.SetProxy("socks5://127.0.0.1:1080")
	if resetPending(tr.pool) {
		t.Fatal("unchanged proxy flagged a reset")
	}
	tr.Reset()
	if !resetPending(tr.pool) || !resetPending(tr.htmlPool) {
		t.Fatal("Reset did not flag both pools")
	}
	tr.Close() // must be safe on a transport that never connected
}

func TestEgressProxyURLEnvOverridesConfig(t *testing.T) {
	useConfig(t, "[proxy.egress]\nproxy_url = \"socks://cfg.example:1\"\n")
	t.Setenv("PROXY_HTTP", "")
	if got := egressProxyURL(); got != "socks5h://cfg.example:1" {
		t.Fatalf("config proxy = %q", got)
	}
	t.Setenv("PROXY_HTTP", "socks4://env.example:2")
	if got := egressProxyURL(); got != "socks4a://env.example:2" {
		t.Fatalf("env proxy = %q", got)
	}
	// The imagine stream shares that single source.
	if got := NewImagineStream("t").proxyURL; got != "socks4a://env.example:2" {
		t.Fatalf("imagine proxy = %q", got)
	}
}

// --- header deadline -------------------------------------------------------

func TestHeaderDeadlineStopBeforeFireNeverCancels(t *testing.T) {
	var cancelled atomic.Bool
	hd := startHeaderDeadline(30*time.Millisecond, func() { cancelled.Store(true) })
	if !hd.stop() {
		t.Fatal("stop reported fired before the deadline")
	}
	time.Sleep(80 * time.Millisecond)
	if cancelled.Load() {
		t.Fatal("deadline fired after stop")
	}
}

func TestHeaderDeadlineStopAfterFireReportsIt(t *testing.T) {
	var cancelled atomic.Bool
	hd := startHeaderDeadline(10*time.Millisecond, func() { cancelled.Store(true) })
	time.Sleep(60 * time.Millisecond)
	if hd.stop() {
		t.Fatal("stop returned true although the deadline had fired")
	}
	if !cancelled.Load() {
		t.Fatal("deadline did not cancel")
	}
}

func TestStreamHeaderTimeoutFailsFast(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	tr := newTestTransport(t)
	start := time.Now()
	_, err := tr.PostStream(context.Background(), srv.URL, "tok", []byte(`{}`), WithTimeout(150*time.Millisecond))
	if err == nil {
		t.Fatal("expected a header timeout")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("header timeout took %s", time.Since(start))
	}
	var appErr *platform.AppError
	if !errors.As(err, &appErr) || appErr.Status != 502 {
		t.Fatalf("err = %v", err)
	}
}

func TestPostJSONSendsBodyAndHeadersToPlainHost(t *testing.T) {
	var gotBody, gotCT, gotCookie atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody.Store(string(b))
		gotCT.Store(r.Header.Get("Content-Type"))
		gotCookie.Store(r.Header.Get("Cookie"))
		_, _ = w.Write([]byte(`{"a":1}`))
	}))
	defer srv.Close()
	tr := newTestTransport(t)
	out, err := tr.PostJSON(context.Background(), srv.URL, "secret", []byte(`{"x":2}`),
		WithExtraHeaders(http.Header{"X-Extra": []string{"1"}}))
	if err != nil || out["a"] != float64(1) {
		t.Fatalf("out=%v err=%v", out, err)
	}
	if gotBody.Load() != `{"x":2}` || gotCT.Load() != "application/json" || gotCookie.Load() != "" {
		t.Fatalf("body=%v ct=%v cookie=%v", gotBody.Load(), gotCT.Load(), gotCookie.Load())
	}
}
