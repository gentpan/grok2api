package api

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/gentpan/grok2api/internal/account"
	"github.com/gentpan/grok2api/internal/platform"
)

const chatBody = `{"model":"grok-4.20-fast","stream":false,"messages":[{"role":"user","content":"hi"}]}`
const chatBodyStream = `{"model":"grok-4.20-fast","stream":true,"messages":[{"role":"user","content":"hi"}]}`

// scriptedPosts builds a postStream handler from a per-call script. A script
// entry is either frames to stream or an error.
type scriptStep struct {
	frames []string
	err    error
}

func scripted(steps ...scriptStep) func(postCall) (io.ReadCloser, error) {
	var mu sync.Mutex
	i := 0
	return func(postCall) (io.ReadCloser, error) {
		mu.Lock()
		defer mu.Unlock()
		st := steps[len(steps)-1]
		if i < len(steps) {
			st = steps[i]
		}
		i++
		if st.err != nil {
			return nil, st.err
		}
		return sseBody(st.frames...), nil
	}
}

func TestChatNonStreamSuccess(t *testing.T) {
	e := newTestEnv(t, "", map[string][]string{"basic": {tokA}})
	e.up.postStream = scripted(scriptStep{frames: []string{webTextFrame("Hello"), webTextFrame(" world"), webSoftStopFrame}})

	w := e.post("/v1/chat/completions", chatBody, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	m := jsonBody(t, w)
	msg := m["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "Hello world" || msg["role"] != "assistant" {
		t.Fatalf("message = %v", msg)
	}
	if got := e.up.usedTokens(); len(got) != 1 || got[0] != tokA {
		t.Fatalf("upstream tokens = %v", got)
	}
}

func TestChatStreamSuccess(t *testing.T) {
	e := newTestEnv(t, "", map[string][]string{"basic": {tokA}})
	e.up.postStream = scripted(scriptStep{frames: []string{webTextFrame("Hello"), webTextFrame(" world")}})

	w := e.post("/v1/chat/completions", chatBodyStream, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	frames := sseDataFrames(w.Body.String())
	if len(frames) < 4 || frames[len(frames)-1] != "[DONE]" {
		t.Fatalf("frames = %v", frames)
	}
	if !strings.Contains(frames[0], `"content":"Hello"`) || !strings.Contains(frames[1], `"content":" world"`) {
		t.Fatalf("text frames = %v", frames[:2])
	}
	if !strings.Contains(frames[len(frames)-2], `"finish_reason":"stop"`) {
		t.Fatalf("no finish chunk before [DONE]: %v", frames)
	}
}

func TestChatRateLimitFailsOverToSecondAccount(t *testing.T) {
	e := newTestEnv(t, "[retry]\nmax_retries = 1\n", map[string][]string{"basic": {tokA, tokB}})
	e.up.postStream = scripted(
		scriptStep{err: platform.UpstreamError("slow down", 429, "")},
		scriptStep{frames: []string{webTextFrame("ok")}},
	)

	w := e.post("/v1/chat/completions", chatBody, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	used := e.up.usedTokens()
	if len(used) != 2 || used[0] == used[1] {
		t.Fatalf("expected two different accounts, got %v", used)
	}
}

func TestChatInvalidCredentialsExpiresAccountAndRetries(t *testing.T) {
	e := newTestEnv(t, "[retry]\nmax_retries = 1\n", map[string][]string{"basic": {tokA, tokB}})
	e.up.postStream = scripted(
		scriptStep{err: platform.UpstreamError("bad", 401, "invalid-credentials")},
		scriptStep{frames: []string{webTextFrame("ok")}},
	)

	w := e.post("/v1/chat/completions", chatBody, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	used := e.up.usedTokens()
	if len(used) != 2 {
		t.Fatalf("used = %v", used)
	}
	if st := e.status(used[0]); st != account.StatusExpired {
		t.Fatalf("failed account status = %q, want expired", st)
	}
	if st := e.status(used[1]); st != account.StatusActive {
		t.Fatalf("good account status = %q", st)
	}
}

func TestChatMaxRetriesZeroDoesNotFailOver(t *testing.T) {
	e := newTestEnv(t, "[retry]\nmax_retries = 0\n", map[string][]string{"basic": {tokA, tokB}})
	e.up.postStream = scripted(
		scriptStep{err: platform.UpstreamError("slow down", 429, "")},
		scriptStep{frames: []string{webTextFrame("ok")}},
	)
	w := e.post("/v1/chat/completions", chatBody, nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", w.Code, w.Body.String())
	}
	if n := len(e.up.usedTokens()); n != 1 {
		t.Fatalf("upstream calls = %d, want 1", n)
	}
}

func TestChatEmptyPoolFallsBackToCallersOwnToken(t *testing.T) {
	e := newTestEnv(t, "", nil)
	e.up.postStream = scripted(scriptStep{frames: []string{webTextFrame("ok")}})
	w := e.post("/v1/chat/completions", chatBody, map[string]string{"Authorization": "Bearer sso-own-token-0123456789"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if got := e.up.usedTokens(); len(got) != 1 || got[0] != "sso-own-token-0123456789" {
		t.Fatalf("upstream tokens = %v", got)
	}
}

func TestChatEmptyPoolWithoutTokenIs429(t *testing.T) {
	e := newTestEnv(t, "", nil)
	w := e.post("/v1/chat/completions", chatBody, nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if code := errorCode(t, w); code != "rate_limit_exceeded" {
		t.Fatalf("code = %q", code)
	}
	if len(e.up.usedTokens()) != 0 {
		t.Fatal("upstream was called without any account")
	}
}

func TestChatBYOCallerNeverDrawsFromPool(t *testing.T) {
	e := newTestEnv(t, "[app]\napi_key = \"gateway-key\"\n", map[string][]string{"basic": {tokA}})
	e.up.postStream = scripted(scriptStep{frames: []string{webTextFrame("ok")}})
	w := e.post("/v1/chat/completions", chatBody, map[string]string{"Authorization": "Bearer byo-sso-token-0123456789"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	got := e.up.usedTokens()
	if len(got) != 1 || got[0] != "byo-sso-token-0123456789" {
		t.Fatalf("BYO caller must only use its own token, got %v", got)
	}

	// A gateway key, in contrast, draws from the pool and is never forwarded.
	e.up.mu.Lock()
	e.up.tokens = nil
	e.up.mu.Unlock()
	w = e.post("/v1/chat/completions", chatBody, map[string]string{"Authorization": "Bearer gateway-key"})
	if w.Code != http.StatusOK {
		t.Fatalf("gateway key status = %d: %s", w.Code, w.Body.String())
	}
	if got := e.up.usedTokens(); len(got) != 1 || got[0] != tokA {
		t.Fatalf("gateway key tokens = %v", got)
	}
}

func TestChatUnknownModelIs400(t *testing.T) {
	e := newTestEnv(t, "", nil)
	w := e.post("/v1/chat/completions", `{"model":"nope","messages":[{"role":"user","content":"x"}]}`, nil)
	if w.Code != http.StatusBadRequest || errorCode(t, w) != "model_not_found" {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}
