package grok

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gentpan/grok2api/internal/platform"
	tlsclient "github.com/gentpan/grok2api/internal/tlsclient"
)

// allowLocalCookieHosts lets tests exercise the credentialed request path
// against a plain-http httptest server.
func allowLocalCookieHosts(t *testing.T) {
	t.Helper()
	prev := cookieHostTrusted
	cookieHostTrusted = func(*url.URL) bool { return true }
	t.Cleanup(func() { cookieHostTrusted = prev })
}

func newTestTransport(t *testing.T) *Transport {
	t.Helper()
	tr, err := NewTransport()
	if err != nil {
		t.Fatal(err)
	}
	tr.pool.proxyURL = ""
	tr.htmlPool.proxyURL = ""
	return tr
}

// A stream must be allowed to outlive WithTimeout (which only bounds the wait
// for response headers) and must not be cut by the client-level deadline.
func TestPostStreamOutlivesRequestTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		for i := 0; i < 8; i++ {
			fmt.Fprintf(w, "data: %d\n\n", i)
			fl.Flush()
			time.Sleep(150 * time.Millisecond)
		}
	}))
	defer server.Close()

	tr := newTestTransport(t)
	body, err := tr.PostStream(context.Background(), server.URL, "tok", []byte(`{}`), WithTimeout(300*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	start := time.Now()
	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("stream cut after %s: %v", time.Since(start), err)
	}
	if time.Since(start) < 600*time.Millisecond {
		t.Fatalf("stream finished too quickly (%s); test is not exercising a slow stream", time.Since(start))
	}
	if got := strings.Count(string(raw), "data:"); got != 8 {
		t.Fatalf("received %d events, want 8: %q", got, raw)
	}
}

// Cancelling the caller's context must abort the upstream request.
func TestPostStreamContextCancelAbortsUpstream(t *testing.T) {
	upstreamDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		fl := w.(http.Flusher)
		defer close(upstreamDone)
		for {
			if _, err := fmt.Fprint(w, "data: tick\n\n"); err != nil {
				return
			}
			fl.Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}))
	defer server.Close()

	tr := newTestTransport(t)
	ctx, cancel := context.WithCancel(context.Background())
	body, err := tr.PostStream(ctx, server.URL, "tok", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	buf := make([]byte, 64)
	if _, err := body.Read(buf); err != nil {
		t.Fatal(err)
	}
	cancel()

	select {
	case <-upstreamDone:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream request was not aborted after ctx cancel")
	}
	readErr := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, body)
		readErr <- err
	}()
	select {
	case err := <-readErr:
		if err == nil {
			t.Fatal("expected an error reading a cancelled stream")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("body read did not return after ctx cancel")
	}
}

// Non-stream requests keep their timeout, now covering the body read too.
func TestPostJSONTimeoutStillApplies(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer server.Close()

	tr := newTestTransport(t)
	start := time.Now()
	_, err := tr.PostJSON(context.Background(), server.URL, "tok", []byte(`{}`), WithTimeout(300*time.Millisecond))
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("timeout took %s", time.Since(start))
	}
}

// A client-side cancellation must not make the shared client rebuild.
func TestCallerCancelDoesNotResetClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // lets the server notice the client going away
		<-r.Context().Done()
	}))
	defer server.Close()

	tr := newTestTransport(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := tr.PostJSON(ctx, server.URL, "tok", []byte(`{}`)); err == nil {
		t.Fatal("expected error")
	}
	tr.pool.mu.Lock()
	pending := tr.pool.resetPending
	tr.pool.mu.Unlock()
	if pending {
		t.Fatal("caller cancellation marked the client for reset")
	}
}

func TestUntrustedHostGetsNoCredentials(t *testing.T) {
	var gotCookie, gotStatsig atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie.Store(r.Header.Get("Cookie"))
		gotStatsig.Store(r.Header.Get("x-statsig-id"))
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	tr := newTestTransport(t)
	body, err := tr.GetBytes(context.Background(), server.URL+"/x.png", "secret-sso")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, body)
	body.Close()
	if c, _ := gotCookie.Load().(string); c != "" {
		t.Fatalf("cookie leaked to untrusted host: %q", c)
	}
	if s, _ := gotStatsig.Load().(string); s != "" {
		t.Fatalf("statsig header sent to untrusted host: %q", s)
	}

	// Console credentials (SSO + DPoP) must be refused outright.
	_, err = tr.PostJSON(context.Background(), server.URL+"/v1/x", "secret-sso", []byte(`{}`), WithConsoleMode())
	var appErr *platform.AppError
	if !errors.As(err, &appErr) || !strings.Contains(appErr.Message, "untrusted host") {
		t.Fatalf("console request to untrusted host: err = %v", err)
	}
}

func TestDefaultCookieHostTrusted(t *testing.T) {
	cases := map[string]bool{
		"https://grok.com/rest/x":             true,
		"https://assets.grok.com/users/a":     true,
		"https://console.x.ai/v1/responses":   true,
		"https://accounts.x.ai/a":             true,
		"http://grok.com/rest/x":              false,
		"https://grok.com.evil.test/":         false,
		"https://evilgrok.com/":               false,
		"https://x.ai.evil.test/":             false,
		"https://grok.com@evil.test/":         false,
		"https://127.0.0.1:8080/":             false,
		"https://169.254.169.254/latest/meta": false,
	}
	for raw, want := range cases {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := defaultCookieHostTrusted(u); got != want {
			t.Errorf("%s: trusted = %v, want %v", raw, got, want)
		}
	}
}

func TestCookieHeaderSentOnceAndJarDeduped(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Cookie"))
		mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "sso", Value: "from-server", Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "__cf_bm", Value: "bm", Path: "/"})
	}))
	defer server.Close()

	client := tlsclient.New()
	do := func() {
		req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
		req.Header.Set("Cookie", "sso=mine; extra=1")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	do()
	do()
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("requests = %d", len(seen))
	}
	if seen[0] != "sso=mine; extra=1" {
		t.Fatalf("first Cookie header = %q", seen[0])
	}
	// Caller's sso wins over the jar's; jar-only cookies are still replayed.
	if seen[1] != "sso=mine; extra=1; __cf_bm=bm" {
		t.Fatalf("second Cookie header = %q", seen[1])
	}
}

func TestClientLevelTimeoutCutsBodyUnlessRaised(t *testing.T) {
	// Documents why Transport raises the client-level deadline: tls-client's
	// timeout also covers body reads.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		for i := 0; i < 25; i++ {
			fmt.Fprint(w, "x")
			fl.Flush()
			time.Sleep(100 * time.Millisecond)
		}
	}))
	defer server.Close()

	read := func(c *tlsclient.Client) (int, error) {
		req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
		resp, err := c.Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		return len(b), err
	}
	short, err := tlsclient.NewClient(tlsclient.WithTimeout(1))
	if err != nil {
		t.Fatal(err)
	}
	if n, err := read(short); err == nil {
		t.Fatalf("1s client timeout did not cut a 2.5s stream (read %d bytes)", n)
	}
	long, err := tlsclient.NewClient(tlsclient.WithTimeout(clientBackstopSeconds))
	if err != nil {
		t.Fatal(err)
	}
	if n, err := read(long); err != nil || n != 25 {
		t.Fatalf("backstop client read n=%d err=%v", n, err)
	}
}

func TestEnsureClientResetRateLimitAndClose(t *testing.T) {
	tr := newTestTransport(t)
	c1, err := tr.pool.get()
	if err != nil {
		t.Fatal(err)
	}
	// A failure-triggered reset right after a build is deferred...
	tr.pool.markReset()
	c2, _ := tr.pool.get()
	if c2 != c1 {
		t.Fatal("client rebuilt inside the minimum reset interval")
	}
	// ...and applied once the interval has passed.
	tr.pool.mu.Lock()
	tr.pool.lastBuild = time.Now().Add(-2 * minAutoResetInterval)
	tr.pool.mu.Unlock()
	c3, _ := tr.pool.get()
	if c3 == c1 {
		t.Fatal("client was not rebuilt after the interval")
	}
	// An explicit Reset bypasses the interval.
	tr.Reset()
	c4, _ := tr.pool.get()
	if c4 == c3 {
		t.Fatal("explicit Reset did not rebuild")
	}
}

func TestInvalidProxyReturnsErrorInsteadOfPanic(t *testing.T) {
	tr := newTestTransport(t)
	tr.pool.proxyURL = "ftp://127.0.0.1:1"
	if _, err := tr.pool.get(); err == nil {
		t.Skip("tls-client accepted the proxy URL; nothing to assert")
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("PostJSON panicked: %v", r)
		}
	}()
	_, err := tr.PostJSON(context.Background(), "https://grok.com/x", "tok", []byte(`{}`))
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestDPoPLoaderPanicReleasesWaiters(t *testing.T) {
	tr := newTestTransport(t)
	// A zero tlsclient.Client panics inside Do, i.e. inside the DPoP fetch.
	tr.pool.client = &tlsclient.Client{}

	func() {
		defer func() { _ = recover() }()
		_, _ = tr.getDPoPSession(context.Background(), "tok", "http://127.0.0.1:1", proxyProfile{})
	}()
	tr.dpop.mu.Lock()
	pending := len(tr.dpop.loading)
	tr.dpop.mu.Unlock()
	if pending != 0 {
		t.Fatalf("dpop loading still has %d entries after a panic", pending)
	}

	// A later caller must not block on the dead loader.
	tr.pool.client = nil
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := tr.getDPoPSession(ctx, "tok", "http://127.0.0.1:1", proxyProfile{})
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a prompt connection error", err)
	}
}

func allowLocalAssetFetch(t *testing.T) {
	t.Helper()
	prev := assetFetchGuard
	assetFetchGuard = func(context.Context, string) error { return nil }
	t.Cleanup(func() { assetFetchGuard = prev })
}

func TestUploadFromInputRejectsOversizedRemoteFile(t *testing.T) {
	allowLocalAssetFetch(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := make([]byte, 1<<20)
		for i := 0; i < maxFetchBytes>>20+2; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	tr := newTestTransport(t)
	_, err := UploadFromInput(context.Background(), tr, "tok", server.URL+"/big.png")
	var appErr *platform.AppError
	if !errors.As(err, &appErr) || !strings.Contains(appErr.Message, "limit") {
		t.Fatalf("err = %v, want size limit error", err)
	}
}

func TestTruncUTF8(t *testing.T) {
	s := "中文标题ABC"
	for n := 0; n <= len(s)+1; n++ {
		got := truncUTF8(s, n)
		if len(got) > n && n >= 0 {
			t.Fatalf("n=%d len=%d", n, len(got))
		}
		if !strings.HasPrefix(s, got) {
			t.Fatalf("n=%d got %q is not a prefix", n, got)
		}
		for _, r := range got {
			if r == '�' {
				t.Fatalf("n=%d produced invalid rune in %q", n, got)
			}
		}
	}
	if got := truncBody([]byte("中文中文"), 7); got != "中文" {
		t.Fatalf("truncBody = %q", got)
	}
}

func TestBuildSetBirthPayloadConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if buildSetBirthPayload()["birthDate"] == "" {
					t.Error("empty birth date")
				}
			}
		}()
	}
	wg.Wait()
}

func TestNewConversationTrackerNonPositiveTTL(t *testing.T) {
	ct := NewConversationTracker(0)
	ct.Set("tok", "c", "r")
	if ct.Get("tok") != nil {
		t.Fatal("zero ttl entry should be stale")
	}
}
