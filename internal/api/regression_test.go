package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/gentpan/grok2api/internal/account"
	"github.com/gentpan/grok2api/internal/grok"
	"github.com/gentpan/grok2api/internal/platform"
	"github.com/gentpan/grok2api/internal/storage"
)

// --- helpers ---

// multipartBody renders text fields (and optional files) as a form.
func multipartBody(t *testing.T, fields map[string]string, files map[string][]byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	for field, data := range files {
		h := make(map[string][]string)
		h["Content-Disposition"] = []string{`form-data; name="` + field + `"; filename="x.png"`}
		h["Content-Type"] = []string{"image/png"}
		part, err := w.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = part.Write(data)
	}
	_ = w.Close()
	return &buf, w.FormDataContentType()
}

func (e *testEnv) postForm(path string, fields map[string]string, files map[string][]byte, hdr map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	body, ctype := multipartBody(e.t, fields, files)
	r := httptest.NewRequest(http.MethodPost, path, body)
	r.Header.Set("Content-Type", ctype)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.engine.ServeHTTP(w, r)
	return w
}

// waitVideo polls a job until it left queued / in_progress.
func (e *testEnv) waitVideo(id string, hdr map[string]string) map[string]any {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		w := e.do(http.MethodGet, "/v1/videos/"+id, "", hdr)
		if w.Code != http.StatusOK {
			e.t.Fatalf("GET video = %d: %s", w.Code, w.Body.String())
		}
		m := jsonBody(e.t, w)
		if st := m["status"]; st != "queued" && st != "in_progress" {
			return m
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("job never finished: %v", m)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (e *testEnv) resetTokens() {
	e.up.mu.Lock()
	e.up.tokens = nil
	e.up.mu.Unlock()
}

// failThenOK scripts PostStream: the first n calls fail with err, then succeed.
func failThenOK(n int, err error, ok ...string) func(postCall) (io.ReadCloser, error) {
	steps := make([]scriptStep, 0, n+1)
	for i := 0; i < n; i++ {
		steps = append(steps, scriptStep{err: err})
	}
	steps = append(steps, scriptStep{frames: ok})
	return scripted(steps...)
}

type fakeMedia struct {
	mu    sync.Mutex
	dir   string
	saved []string
	fail  bool
}

func (m *fakeMedia) SaveVideo(raw []byte, id string) (string, error) {
	if m.fail {
		return "", errors.New("disk full")
	}
	p := filepath.Join(m.dir, id+".mp4")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		return "", err
	}
	m.mu.Lock()
	m.saved = append(m.saved, p)
	m.mu.Unlock()
	return p, nil
}
func (m *fakeMedia) Delete(mt storage.MediaType, name string) (bool, error) { return true, nil }
func (m *fakeMedia) Clear(mt storage.MediaType) (int, error)                { return 0, nil }

// --- B1 / B2 / decision (e): no failure is ever a 200, and every path feeds accounting ---

func TestResponsesFailuresAreErrorsNotEmpty200(t *testing.T) {
	e := newTestEnv(t, "[retry]\nmax_retries = 0\n", map[string][]string{"basic": {tokA}})
	e.up.postStream = scripted(scriptStep{err: platform.UpstreamError("slow down", 429, "")})

	for _, stream := range []string{"false", "true"} {
		w := e.post("/v1/responses", `{"model":"grok-4.20-fast","input":"hi","stream":`+stream+`}`, nil)
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("stream=%s status = %d, want 429: %s", stream, w.Code, w.Body.String())
		}
		m := jsonBody(t, w)
		errObj, _ := m["error"].(map[string]any)
		if errObj == nil || errObj["message"] == nil {
			t.Fatalf("stream=%s body is not an OpenAI error envelope: %s", stream, w.Body.String())
		}
		e.resetTokens()
		e.up.postStream = scripted(scriptStep{err: platform.UpstreamError("slow down", 429, "")})
	}
}

func TestResponsesNoAccountsIs429Envelope(t *testing.T) {
	e := newTestEnv(t, "", nil)
	w := e.post("/v1/responses", `{"model":"grok-4.20-fast","input":"hi"}`, nil)
	if w.Code != http.StatusTooManyRequests || errorCode(t, w) != "rate_limit_exceeded" {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
}

func TestMessagesFailuresUseAnthropicEnvelope(t *testing.T) {
	e := newTestEnv(t, "[retry]\nmax_retries = 0\n", map[string][]string{"basic": {tokA}})
	e.up.postStream = scripted(scriptStep{err: platform.UpstreamError("slow down", 429, "")})
	for _, stream := range []string{"false", "true"} {
		w := e.post("/v1/messages", `{"model":"grok-4.20-fast","max_tokens":8,"stream":`+stream+`,"messages":[{"role":"user","content":"hi"}]}`, nil)
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("stream=%s status = %d: %s", stream, w.Code, w.Body.String())
		}
		m := jsonBody(t, w)
		errObj, _ := m["error"].(map[string]any)
		if m["type"] != "error" || errObj["type"] != "rate_limit_error" {
			t.Fatalf("stream=%s not an Anthropic envelope: %s", stream, w.Body.String())
		}
		e.up.postStream = scripted(scriptStep{err: platform.UpstreamError("slow down", 429, "")})
	}
	// Auth failures on the Anthropic route use the same envelope.
	e2 := newTestEnv(t, "[app]\napi_key = \"k\"\n", nil)
	w := e2.post("/v1/messages", `{"model":"grok-4.20-fast","messages":[]}`, nil)
	if w.Code != http.StatusUnauthorized || jsonBody(t, w)["type"] != "error" {
		t.Fatalf("unauthenticated = %d: %s", w.Code, w.Body.String())
	}
}

func TestResponsesAndMessagesFeedAccountAccounting(t *testing.T) {
	// A bad credential seen through /v1/responses expires the account, exactly
	// as it does through /v1/chat/completions.
	e := newTestEnv(t, "[retry]\nmax_retries = 0\n", map[string][]string{"basic": {tokA}})
	e.up.postStream = scripted(scriptStep{err: platform.UpstreamError("bad", 401, "invalid-credentials")})
	w := e.post("/v1/responses", `{"model":"grok-4.20-fast","input":"hi","stream":false}`, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if st := e.status(tokA); st != account.StatusExpired {
		t.Fatalf("account status = %q, want expired", st)
	}

	// Successes are fed back too (quota accounting).
	e = newTestEnv(t, "", map[string][]string{"basic": {tokA}})
	e.up.postStream = scripted(scriptStep{frames: []string{webTextFrame("ok")}})
	for _, req := range []struct{ path, body string }{
		{"/v1/responses", `{"model":"grok-4.20-fast","input":"hi","stream":false}`},
		{"/v1/messages", `{"model":"grok-4.20-fast","max_tokens":8,"stream":false,"messages":[{"role":"user","content":"hi"}]}`},
	} {
		if w := e.post(req.path, req.body, nil); w.Code != http.StatusOK {
			t.Fatalf("%s = %d: %s", req.path, w.Code, w.Body.String())
		}
	}
	if got := e.pool.kinds(tokA); len(got) != 2 || got[0] != account.FbSuccess || got[1] != account.FbSuccess {
		t.Fatalf("feedback = %v, want two successes", got)
	}
}

func TestWSImageGenerationFeedsAccounting(t *testing.T) {
	e := newTestEnv(t, "[retry]\nmax_retries = 0\n", map[string][]string{"super": {tokA}})
	e.up.images = func(token, prompt string, n int) <-chan grok.ImagineEvent {
		ch := make(chan grok.ImagineEvent, 2)
		ch <- grok.ImagineEvent{Type: grok.ImagineEventImage, ImageID: "i", URL: "/images/i.png"}
		close(ch)
		return ch
	}
	if w := e.post("/v1/images/generations", `{"model":"grok-imagine-image","prompt":"a cat"}`, nil); w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if got := e.pool.kinds(tokA); len(got) != 1 || got[0] != account.FbSuccess {
		t.Fatalf("success feedback = %v", got)
	}

	e.up.images = func(token, prompt string, n int) <-chan grok.ImagineEvent {
		ch := make(chan grok.ImagineEvent, 1)
		ch <- grok.ImagineEvent{Type: grok.ImagineEventError, Error: "boom"}
		close(ch)
		return ch
	}
	w := e.post("/v1/images/generations", `{"model":"grok-imagine-image","prompt":"a cat"}`, nil)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("error status = %d: %s", w.Code, w.Body.String())
	}
	got := e.pool.kinds(tokA)
	if len(got) != 2 || got[1] != account.FbServerError {
		t.Fatalf("failure feedback = %v", got)
	}
}

func TestStreamingChatErrorFrameAndFeedback(t *testing.T) {
	e := newTestEnv(t, "[retry]\nmax_retries = 0\n", map[string][]string{"basic": {tokA}})
	e.up.postStream = scripted(scriptStep{frames: []string{webTextFrame("partial"), webErrorFrame("Too many requests", 8)}})
	w := e.post("/v1/chat/completions", chatBodyStream, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("committed stream status = %d", w.Code)
	}
	frames := sseDataFrames(w.Body.String())
	if len(frames) < 3 || !strings.Contains(frames[len(frames)-2], `"error"`) || frames[len(frames)-1] != "[DONE]" {
		t.Fatalf("frames = %v", frames)
	}
	if got := e.pool.kinds(tokA); len(got) != 1 || got[0] != account.FbRateLimited {
		t.Fatalf("feedback = %v, want rate_limited", got)
	}

	// Same through /v1/responses: the protocol's error events, no empty 200 text.
	e = newTestEnv(t, "[retry]\nmax_retries = 0\n", map[string][]string{"basic": {tokA}})
	e.up.postStream = scripted(scriptStep{frames: []string{webTextFrame("partial"), webErrorFrame("Too many requests", 8)}})
	w = e.post("/v1/responses", `{"model":"grok-4.20-fast","input":"hi","stream":true}`, nil)
	evs := sseEvents(w.Body.String())
	if len(evs) < 2 || evs[len(evs)-2] != "error" || evs[len(evs)-1] != "response.failed" {
		t.Fatalf("responses events = %v", evs)
	}
	// And through /v1/messages.
	e = newTestEnv(t, "[retry]\nmax_retries = 0\n", map[string][]string{"basic": {tokA}})
	e.up.postStream = scripted(scriptStep{frames: []string{webTextFrame("partial"), webErrorFrame("Too many requests", 8)}})
	w = e.post("/v1/messages", `{"model":"grok-4.20-fast","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	evs = sseEvents(w.Body.String())
	if len(evs) == 0 || evs[len(evs)-1] != "error" {
		t.Fatalf("messages events = %v", evs)
	}
	if strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatal("Anthropic streams do not end with [DONE]")
	}
}

// --- B3 / B12 / decision (c): identity and BYO-token handling ---

func TestVideoJobCarriesCallerAndBYOTokenNeverDrawsFromPool(t *testing.T) {
	const byo = "byo-sso-token-0123456789abcdef"
	e := newTestEnv(t, "[app]\napi_key = \"gateway-key\"\n", map[string][]string{"super": {tokA}, "basic": {tokB}})
	e.up.postStream = scripted(scriptStep{frames: []string{webImageFrame(100, "v1", "users/x/v1.mp4")}})
	e.up.consoleVideo = func(token string, p map[string]any) (map[string]any, error) {
		return map[string]any{"request_id": "r1"}, nil
	}
	e.up.consoleStatus = func(token, id string) (map[string]any, error) {
		return map[string]any{"status": "completed", "video": map[string]any{"url": "https://vidgen.x.ai/v.mp4"}}, nil
	}
	hdr := map[string]string{"Authorization": "Bearer " + byo}

	for _, modelName := range []string{"grok-imagine-video", "grok-imagine-video-console"} {
		e.resetTokens()
		w := e.postForm("/v1/videos", map[string]string{"model": modelName, "prompt": "a cat"}, nil, hdr)
		if w.Code != http.StatusOK {
			t.Fatalf("%s create = %d: %s", modelName, w.Code, w.Body.String())
		}
		job := e.waitVideo(jsonBody(t, w)["id"].(string), hdr)
		if job["status"] != "completed" {
			t.Fatalf("%s job = %v", modelName, job)
		}
		for _, tok := range e.up.usedTokens() {
			if tok != byo {
				t.Fatalf("%s: BYO caller's job used token %q (pool token leaked)", modelName, tok)
			}
		}
		if len(e.up.usedTokens()) == 0 {
			t.Fatalf("%s: upstream never called", modelName)
		}
	}
	if got := e.pool.reservations(); len(got) != 0 {
		t.Fatalf("pool was drawn from: %v", got)
	}

	// A gateway key, in contrast, may use the pool.
	e.resetTokens()
	gw := map[string]string{"Authorization": "Bearer gateway-key"}
	w := e.postForm("/v1/videos", map[string]string{"model": "grok-imagine-video", "prompt": "a cat"}, nil, gw)
	job := e.waitVideo(jsonBody(t, w)["id"].(string), gw)
	if job["status"] != "completed" {
		t.Fatalf("gateway-key job = %v", job)
	}
	if got := e.up.usedTokens(); len(got) == 0 || got[0] != tokA {
		t.Fatalf("gateway-key job tokens = %v", got)
	}
}

func TestCallerlessContextHasNoPoolAccess(t *testing.T) {
	if c := callerFrom(context.Background()); c.PoolAllowed || c.SSOToken != "" {
		t.Fatalf("zero Caller must be deny-all, got %+v", c)
	}
	e := newTestEnv(t, "", map[string][]string{"basic": {tokA}})
	spec, _ := e.srv.modelRegistry().Resolve("grok-4.20-fast")
	_, err := e.srv.acquire(context.Background(), Caller{}, spec, acquireOptions{}, func(context.Context, *account.Lease) (bool, error) {
		t.Fatal("attempt must not run without a pool-capable or token-bearing caller")
		return false, nil
	})
	if err == nil {
		t.Fatal("expected 'no available accounts'")
	}
}

func TestFailedBYOTokenIsNotRetriedWithSameToken(t *testing.T) {
	e := newTestEnv(t, "[retry]\nmax_retries = 3\n", nil)
	e.up.postStream = scripted(scriptStep{err: platform.UpstreamError("slow down", 429, "")})
	w := e.post("/v1/chat/completions", chatBody, map[string]string{"Authorization": "Bearer sso-own-token-0123456789"})
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if n := len(e.up.usedTokens()); n != 1 {
		t.Fatalf("own token used %d times, want exactly 1", n)
	}
	if len(e.pool.feedbacks) != 0 || e.pool.releases != 0 {
		t.Fatalf("own-token attempt touched pool state: feedbacks=%v releases=%d", e.pool.feedbacks, e.pool.releases)
	}
}

func TestBYOLeaseDoesNotReleaseSameTokenPoolSlot(t *testing.T) {
	// The caller's own token happens to equal a pooled token.
	e := newTestEnv(t, "[app]\napi_key = \"gw\"\n", map[string][]string{"basic": {tokA}})
	e.up.postStream = scripted(scriptStep{frames: []string{webTextFrame("ok")}})
	w := e.post("/v1/chat/completions", chatBody, map[string]string{"Authorization": "Bearer " + tokA})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if e.pool.releases != 0 || len(e.pool.feedbacks) != 0 {
		t.Fatalf("BYO request changed pool slot state: releases=%d feedbacks=%v", e.pool.releases, e.pool.feedbacks)
	}
}

// --- decision (a): failover applies to every path ---

func TestFailoverAppliesToConsoleImagesEditsAndVideo(t *testing.T) {
	// console image generation
	e := newTestEnv(t, "[retry]\nmax_retries = 1\n", map[string][]string{"basic": {tokA, tokB}})
	calls := 0
	e.up.consoleImage = func(token string, p map[string]any) (map[string]any, error) {
		calls++
		if calls == 1 {
			return nil, platform.UpstreamError("slow down", 429, "")
		}
		return map[string]any{"data": []any{map[string]any{"url": "https://x/y.png"}}}, nil
	}
	w := e.post("/v1/images/generations", `{"model":"grok-imagine-image-console","prompt":"a cat"}`, nil)
	if w.Code != http.StatusOK || calls != 2 {
		t.Fatalf("console image: status=%d calls=%d: %s", w.Code, calls, w.Body.String())
	}

	// web image edit
	e = newTestEnv(t, "[retry]\nmax_retries = 1\n", map[string][]string{"super": {tokA, tokB}})
	e.up.postStream = failThenOK(1, platform.UpstreamError("slow down", 429, ""), webImageFrame(100, "u", "users/x/u.png"))
	w = e.postForm("/v1/images/edits", map[string]string{"model": "grok-imagine-image-edit", "prompt": "edit"},
		map[string][]byte{"image[]": []byte("\x89PNG")}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("image edit status = %d: %s", w.Code, w.Body.String())
	}
	if used := e.up.usedTokens(); len(used) != 2 || used[0] == used[1] {
		t.Fatalf("image edit tokens = %v", used)
	}

	// console video create
	e = newTestEnv(t, "[retry]\nmax_retries = 1\n", map[string][]string{"basic": {tokA, tokB}})
	creates := 0
	e.up.consoleVideo = func(token string, p map[string]any) (map[string]any, error) {
		creates++
		if creates == 1 {
			return nil, platform.UpstreamError("slow down", 429, "")
		}
		return map[string]any{"request_id": "r1"}, nil
	}
	e.up.consoleStatus = func(token, id string) (map[string]any, error) {
		return map[string]any{"status": "completed", "video": map[string]any{"url": "https://vidgen.x.ai/v.mp4"}}, nil
	}
	w = e.postForm("/v1/videos", map[string]string{"model": "grok-imagine-video-console", "prompt": "a cat"}, nil, nil)
	job := e.waitVideo(jsonBody(t, w)["id"].(string), nil)
	if job["status"] != "completed" || creates != 2 {
		t.Fatalf("console video: creates=%d job=%v", creates, job)
	}
}

// --- decision (b): refresh-on-demand at most once per request ---

func TestRefreshOnDemandRunsOncePerRequest(t *testing.T) {
	e := newTestEnv(t, "[retry]\nmax_retries = 5\n", map[string][]string{"basic": {tokA, tokB}})
	e.up.postStream = scripted(scriptStep{err: platform.UpstreamError("slow down", 429, "")})
	w := e.post("/v1/chat/completions", chatBody, nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if n := e.refresh.onDemandCalls(); n != 1 {
		t.Fatalf("refresh-on-demand ran %d times for one request, want 1", n)
	}
	if n := len(e.up.usedTokens()); n != 2 {
		t.Fatalf("upstream calls = %d, want one per account", n)
	}

	// Empty pool + a failing own token: one refresh, one upstream call.
	e = newTestEnv(t, "[retry]\nmax_retries = 3\n", nil)
	e.up.postStream = scripted(scriptStep{err: platform.UpstreamError("slow down", 429, "")})
	w = e.post("/v1/chat/completions", chatBody, map[string]string{"Authorization": "Bearer sso-own-token-0123456789"})
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d", w.Code)
	}
	if n := e.refresh.onDemandCalls(); n != 1 {
		t.Fatalf("refresh-on-demand ran %d times, want 1", n)
	}
	if n := len(e.up.usedTokens()); n != 1 {
		t.Fatalf("own token used %d times, want 1", n)
	}
}

// --- decision (d): empty image results ---

func TestEmptyImageResultRetriesOnceOnAnotherAccountWithoutPenalty(t *testing.T) {
	e := newTestEnv(t, "[retry]\nmax_retries = 0\n", map[string][]string{"basic": {tokA, tokB}})
	e.up.postStream = scripted(
		scriptStep{frames: []string{webTextFrame("moderated")}},
		scriptStep{frames: []string{webImageFrame(100, "u", "users/x/u.png")}},
	)
	w := e.post("/v1/images/generations", `{"model":"grok-imagine-image-lite","prompt":"a cat","n":1}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	used := e.up.usedTokens()
	if len(used) != 2 || used[0] == used[1] {
		t.Fatalf("tokens = %v, want two different accounts", used)
	}
	for _, f := range e.pool.feedbacks {
		if f.Kind != account.FbSuccess {
			t.Fatalf("an empty result marked an account failed: %v", e.pool.feedbacks)
		}
	}

	// Still empty on the second account: 502, no further attempts.
	e = newTestEnv(t, "[retry]\nmax_retries = 0\n", map[string][]string{"basic": {tokA, tokB, tokC}})
	e.up.postStream = scripted(scriptStep{frames: []string{webTextFrame("moderated")}})
	w = e.post("/v1/images/generations", `{"model":"grok-imagine-image-lite","prompt":"a cat","n":1}`, nil)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", w.Code, w.Body.String())
	}
	if n := len(e.up.usedTokens()); n != 2 {
		t.Fatalf("upstream calls = %d, want exactly 2", n)
	}
}

// --- B4 ---

func TestImageEditUpstreamFailureIsNotEmptySuccess(t *testing.T) {
	e := newTestEnv(t, "[retry]\nmax_retries = 0\n", map[string][]string{"super": {tokA}})
	e.up.postStream = scripted(scriptStep{err: platform.UpstreamError("slow down", 429, "")})
	w := e.postForm("/v1/images/edits", map[string]string{"model": "grok-imagine-image-edit", "prompt": "edit"},
		map[string][]byte{"image[]": []byte("\x89PNG")}, nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", w.Code, w.Body.String())
	}
}

// --- B5: gateway streaming fails over before any byte is written ---

func TestGatewayStreamFailsOverBeforeHeaders(t *testing.T) {
	e := newTestEnv(t, "[retry]\nmax_retries = 1\n", map[string][]string{"super": {tokA, tokB}})
	var mu sync.Mutex
	calls := 0
	e.up.gateway = func(token, modelName, prompt string) <-chan grok.GatewayEvent {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		ch := make(chan grok.GatewayEvent, 4)
		if n == 1 {
			ch <- grok.GatewayEvent{Err: platform.UpstreamError("Grok Gateway connect failed", 429, "")}
		} else {
			ch <- grok.GatewayEvent{Kind: grok.EventText, Content: "hello"}
			ch <- grok.GatewayEvent{Kind: grok.EventSoftStop}
		}
		close(ch)
		return ch
	}
	w := e.post("/v1/chat/completions", `{"model":"grok-4.6","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if calls != 2 {
		t.Fatalf("gateway calls = %d, want 2 (failover)", calls)
	}
	frames := sseDataFrames(w.Body.String())
	if len(frames) < 3 || !strings.Contains(frames[0], `"content":"hello"`) || strings.Contains(w.Body.String(), `"error"`) {
		t.Fatalf("frames = %v", frames)
	}
}

func TestGatewayStreamErrorBeforeHeadersIsAProperStatus(t *testing.T) {
	e := newTestEnv(t, "[retry]\nmax_retries = 0\n", map[string][]string{"super": {tokA}})
	e.up.gateway = func(token, modelName, prompt string) <-chan grok.GatewayEvent {
		ch := make(chan grok.GatewayEvent, 1)
		ch <- grok.GatewayEvent{Err: platform.UpstreamError("blocked", 403, "blocked-user")}
		close(ch)
		return ch
	}
	w := e.post("/v1/chat/completions", `{"model":"grok-4.6","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", w.Code, w.Body.String())
	}
	if st := e.status(tokA); st != account.StatusExpired {
		t.Fatalf("blocked account status = %q", st)
	}
}

// --- B6: video content is available once the job is completed ---

func TestWebVideoContentIsCachedAndServed(t *testing.T) {
	e := newTestEnv(t, "", map[string][]string{"super": {tokA}})
	media := &fakeMedia{dir: t.TempDir()}
	e.srv.Media = media
	e.up.postStream = scripted(scriptStep{frames: []string{webImageFrame(100, "v1", "users/x/v1.mp4")}})
	e.up.getBytes = func(url, token string) (io.ReadCloser, error) {
		if token != tokA {
			t.Errorf("video fetched with token %q, want the leased account", token)
		}
		return io.NopCloser(strings.NewReader("MP4DATA")), nil
	}
	w := e.postForm("/v1/videos", map[string]string{"model": "grok-imagine-video", "prompt": "a cat"}, nil, nil)
	id := jsonBody(t, w)["id"].(string)
	job := e.waitVideo(id, nil)
	if job["status"] != "completed" {
		t.Fatalf("job = %v", job)
	}
	c := e.do(http.MethodGet, "/v1/videos/"+id+"/content", "", nil)
	if c.Code != http.StatusOK || c.Body.String() != "MP4DATA" || c.Header().Get("Content-Type") != "video/mp4" {
		t.Fatalf("content = %d %q (%s)", c.Code, c.Body.String(), c.Header().Get("Content-Type"))
	}
}

func TestConsoleVideoIsNeverCompletedBeforeItsContentIsCached(t *testing.T) {
	e := newTestEnv(t, "", map[string][]string{"basic": {tokA}})
	e.srv.Media = &fakeMedia{dir: t.TempDir()}
	e.up.consoleVideo = func(token string, p map[string]any) (map[string]any, error) {
		return map[string]any{"request_id": "r1"}, nil
	}
	e.up.consoleStatus = func(token, id string) (map[string]any, error) {
		return map[string]any{"status": "completed", "video": map[string]any{"url": "https://vidgen.x.ai/v.mp4"}}, nil
	}
	e.up.consoleDL = func(url string) (io.ReadCloser, error) {
		time.Sleep(60 * time.Millisecond) // a slow download used to leave a "completed but 409" window
		return io.NopCloser(strings.NewReader("CONSOLEMP4")), nil
	}
	w := e.postForm("/v1/videos", map[string]string{"model": "grok-imagine-video-console", "prompt": "a cat"}, nil, nil)
	id := jsonBody(t, w)["id"].(string)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m := jsonBody(t, e.do(http.MethodGet, "/v1/videos/"+id, "", nil))
		if m["status"] == "completed" {
			c := e.do(http.MethodGet, "/v1/videos/"+id+"/content", "", nil)
			if c.Code != http.StatusOK || c.Body.String() != "CONSOLEMP4" {
				t.Fatalf("job completed but content = %d %q", c.Code, c.Body.String())
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("job never completed")
}

// --- B8 ---

func TestStreamEmitsExactlyOneFinishChunk(t *testing.T) {
	e := newTestEnv(t, "", map[string][]string{"basic": {tokA}})
	e.up.postStream = scripted(scriptStep{frames: []string{webTextFrame("a"), webSoftStopFrame}})
	w := e.post("/v1/chat/completions", chatBodyStream, nil)
	if n := strings.Count(w.Body.String(), `"finish_reason":"stop"`); n != 1 {
		t.Fatalf("finish chunks = %d, want 1: %s", n, w.Body.String())
	}

	// Gateway: a finish chunk even when the upstream sent no soft stop.
	e = newTestEnv(t, "", map[string][]string{"super": {tokA}})
	e.up.gateway = func(token, modelName, prompt string) <-chan grok.GatewayEvent {
		ch := make(chan grok.GatewayEvent, 2)
		ch <- grok.GatewayEvent{Kind: grok.EventText, Content: "x"}
		close(ch)
		return ch
	}
	w = e.post("/v1/chat/completions", `{"model":"grok-4.6","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if n := strings.Count(w.Body.String(), `"finish_reason":"stop"`); n != 1 {
		t.Fatalf("gateway finish chunks = %d, want 1: %s", n, w.Body.String())
	}
}

func TestClientAbortIsNotReportedAsAccountSuccess(t *testing.T) {
	e := newTestEnv(t, "[retry]\nmax_retries = 0\n", map[string][]string{"basic": {tokA}})
	pr, pw := io.Pipe()
	e.up.postStream = func(postCall) (io.ReadCloser, error) { return pr, nil }

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBodyStream)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		e.engine.ServeHTTP(w, req)
		close(done)
	}()
	_, _ = pw.Write([]byte("data: " + webTextFrame("first") + "\n\n"))
	time.Sleep(50 * time.Millisecond)
	cancel() // the client goes away mid-stream
	_ = pw.CloseWithError(context.Canceled)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not return after client abort")
	}
	for _, f := range e.pool.feedbacks {
		if f.Kind == account.FbSuccess {
			t.Fatalf("aborted request recorded as success: %v", e.pool.feedbacks)
		}
	}
	if st := e.status(tokA); st != account.StatusActive {
		t.Fatalf("an abort must not change account status, got %q", st)
	}
}

func TestUpstreamStreamBreakIsAFailureNotASuccess(t *testing.T) {
	e := newTestEnv(t, "[retry]\nmax_retries = 0\n", map[string][]string{"basic": {tokA}})
	pr, pw := io.Pipe()
	e.up.postStream = func(postCall) (io.ReadCloser, error) { return pr, nil }
	go func() {
		_, _ = pw.Write([]byte("data: " + webTextFrame("first") + "\n\n"))
		_ = pw.CloseWithError(errors.New("connection reset by peer"))
	}()
	w := e.post("/v1/chat/completions", chatBodyStream, nil)
	if !strings.Contains(w.Body.String(), "upstream stream interrupted") {
		t.Fatalf("no error frame: %s", w.Body.String())
	}
	got := e.pool.kinds(tokA)
	if len(got) != 1 || got[0] != account.FbServerError {
		t.Fatalf("feedback = %v, want server_error", got)
	}
}

// --- B9 ---

func TestExtractMessagesToolCallsAndNilContent(t *testing.T) {
	text, _, perr := extractMessages([]map[string]any{
		{"role": "user", "content": "weather?"},
		{"role": "assistant", "tool_calls": []any{map[string]any{
			"id": "c1", "type": "function",
			"function": map[string]any{"name": "get_weather", "arguments": `{"city":"Paris"}`},
		}}},
		{"role": "tool", "tool_call_id": "c1", "content": "sunny"},
		{"role": "assistant", "content": nil},
	})
	if perr != nil {
		t.Fatal(perr)
	}
	if strings.Contains(text, "<nil>") {
		t.Fatalf("prompt contains <nil>: %q", text)
	}
	want := "[user]: weather?\n\n[assistant]: tool_call get_weather({\"city\":\"Paris\"})\n\n[tool]: sunny"
	if text != want {
		t.Fatalf("text = %q\nwant   %q", text, want)
	}
}

// --- B11 ---

func TestRetryCountUsesDirectoryStrategyNotAPhantomConfigKey(t *testing.T) {
	// The (nonexistent) account.refresh.strategy key must not matter.
	useConfig(t, "[retry]\nmax_retries = 1\n[account.refresh]\nstrategy = \"random\"\n")
	if rc := newRetryConfig(globalConfig{}, account.StrategyQuota); rc.MaxRetries != 1 {
		t.Fatalf("quota strategy: MaxRetries = %d, want 1", rc.MaxRetries)
	}
	if rc := newRetryConfig(globalConfig{}, account.StrategyRandom); rc.MaxRetries != randomStrategyMinRetries {
		t.Fatalf("random strategy: MaxRetries = %d, want %d", rc.MaxRetries, randomStrategyMinRetries)
	}
	useConfig(t, "[retry]\nmax_retries = 9\n")
	if rc := newRetryConfig(globalConfig{}, account.StrategyRandom); rc.MaxRetries != 9 {
		t.Fatalf("an explicit higher max_retries must win, got %d", rc.MaxRetries)
	}
}

func TestRandomStrategyFailsOverFiveTimes(t *testing.T) {
	toks := []string{"tok-0000000000000000aaaa", "tok-0000000000000000bbbb", "tok-0000000000000000cccc",
		"tok-0000000000000000dddd", "tok-0000000000000000eeee", "tok-0000000000000000ffff", "tok-0000000000000000gggg"}
	e := newTestEnv(t, "[retry]\nmax_retries = 1\n", map[string][]string{"basic": toks})
	e.dir.SetStrategy(account.StrategyRandom)
	e.up.postStream = scripted(scriptStep{err: platform.UpstreamError("slow down", 429, "")})
	w := e.post("/v1/chat/completions", chatBody, nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d", w.Code)
	}
	if n := len(e.up.usedTokens()); n != randomStrategyMinRetries+1 {
		t.Fatalf("attempts = %d, want %d", n, randomStrategyMinRetries+1)
	}
}

// --- B14 ---

func TestAnthropicStreamIsConformant(t *testing.T) {
	e := newTestEnv(t, "", map[string][]string{"basic": {tokA}})
	e.up.postStream = scripted(scriptStep{frames: []string{webThinkFrame("hmm"), webTextFrame("Hello"), webTextFrame(" world")}})
	w := e.post("/v1/messages", `{"model":"grok-4.20-fast","max_tokens":8,"stream":true,"thinking":{"type":"enabled"},"messages":[{"role":"user","content":"hi"}]}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// Structure only: a thinking block followed by a text block. How many
	// deltas fill a block depends on the upstream adapter (it emits the rollout
	// header and the thinking tokens separately), so runs of deltas collapse;
	// incremental answer deltas are asserted on the body below.
	want := []string{"message_start", "ping", "content_block_start", "content_block_delta", "content_block_stop",
		"content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	got := collapseRuns(sseEvents(body), "content_block_delta")
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v\nwant     %v", got, want)
	}
	if !strings.Contains(body, "hmm") {
		t.Fatalf("thinking text was dropped: %s", body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Fatal("Anthropic streams have no [DONE] sentinel")
	}
	if !strings.Contains(body, `event: ping`+"\n"+`data: {"type":"ping"}`) {
		t.Fatalf("ping frame is not conformant: %s", body)
	}
	if !strings.Contains(body, `"type":"thinking_delta","thinking":"`) && !strings.Contains(body, `"thinking":"`) {
		t.Fatalf("thinking was dropped: %s", body)
	}
	if !strings.Contains(body, `"text":"Hello"`) || !strings.Contains(body, `"text":" world"`) {
		t.Fatalf("answer deltas are not incremental: %s", body)
	}
}

// collapseRuns merges consecutive occurrences of name into one element.
func collapseRuns(in []string, name string) []string {
	out := make([]string, 0, len(in))
	for i, v := range in {
		if v == name && i > 0 && in[i-1] == name {
			continue
		}
		out = append(out, v)
	}
	return out
}

func TestResponsesStreamIsIncremental(t *testing.T) {
	e := newTestEnv(t, "", map[string][]string{"basic": {tokA}})
	e.up.postStream = scripted(scriptStep{frames: []string{webTextFrame("Hello"), webTextFrame(" world")}})
	w := e.post("/v1/responses", `{"model":"grok-4.20-fast","input":"hi","stream":true}`, nil)
	got := sseEvents(w.Body.String())
	want := []string{"response.created", "response.output_item.added", "response.content_part.added",
		"response.output_text.delta", "response.output_text.delta", "response.output_text.done",
		"response.content_part.done", "response.output_item.done", "response.completed"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v\nwant     %v", got, want)
	}
	if !strings.Contains(w.Body.String(), `"text":"Hello world"`) {
		t.Fatalf("final text missing: %s", w.Body.String())
	}
	frames := sseDataFrames(w.Body.String())
	if frames[len(frames)-1] != "[DONE]" {
		t.Fatalf("no [DONE]: %v", frames)
	}
}

// --- B19 ---

func TestConfiguredKeyIsMatchedAfterTrimming(t *testing.T) {
	useConfig(t, "[app]\napi_key = \" good-key , other \"\n")
	e, poolOK := apiEngine(&Server{})
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.Header.Set("Authorization", "Bearer good-key")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !*poolOK {
		t.Fatalf("trimmed key not matched: status=%d pool=%v", w.Code, *poolOK)
	}
}

// --- B20 / B21 ---

func TestParseIntStrIsStrict(t *testing.T) {
	for _, in := range []string{"+5", "99999999999999999999999", "5 ", "0x10", "--1"} {
		if _, err := parseIntStr(in); err == nil {
			t.Fatalf("parseIntStr(%q) accepted", in)
		}
	}
}

func TestVideoSecondsAreValidated(t *testing.T) {
	e := newTestEnv(t, "", map[string][]string{"super": {tokA}, "basic": {tokB}})
	for name, c := range map[string]struct {
		model, seconds string
		want           int
	}{
		"unsupported web length": {"grok-imagine-video", "7", http.StatusBadRequest},
		"garbage":                {"grok-imagine-video", "abc", http.StatusBadRequest},
		"plus sign":              {"grok-imagine-video", "+6", http.StatusBadRequest},
		"console too long":       {"grok-imagine-video-console", "16", http.StatusBadRequest},
		"console zero":           {"grok-imagine-video-console", "0", http.StatusBadRequest},
	} {
		w := e.postForm("/v1/videos", map[string]string{"model": c.model, "prompt": "p", "seconds": c.seconds}, nil, nil)
		if w.Code != c.want {
			t.Fatalf("%s = %d, want %d: %s", name, w.Code, c.want, w.Body.String())
		}
	}
	w := e.postForm("/v1/videos", map[string]string{"model": "grok-imagine-video", "prompt": "p", "seconds": "10"}, nil, nil)
	if w.Code != http.StatusOK || jsonBody(t, w)["seconds"] != "10" {
		t.Fatalf("valid seconds: %d %s", w.Code, w.Body.String())
	}
}

func TestVideoJobsAreOwnedByTheirCreator(t *testing.T) {
	e := newTestEnv(t, "[app]\napi_key = \"key-one,key-two\"\n", map[string][]string{"super": {tokA}})
	e.up.postStream = scripted(scriptStep{frames: []string{webImageFrame(100, "v1", "users/x/v1.mp4")}})
	one := map[string]string{"Authorization": "Bearer key-one"}
	two := map[string]string{"Authorization": "Bearer key-two"}
	byo := map[string]string{"Authorization": "Bearer someone-elses-sso-token"}

	w := e.postForm("/v1/videos", map[string]string{"model": "grok-imagine-video", "prompt": "p"}, nil, one)
	id := jsonBody(t, w)["id"].(string)
	e.waitVideo(id, one)

	for name, hdr := range map[string]map[string]string{"other key": two, "byo": byo} {
		for _, path := range []string{"/v1/videos/" + id, "/v1/videos/" + id + "/content"} {
			c := e.do(http.MethodGet, path, "", hdr)
			if c.Code != http.StatusBadRequest || errorCode(t, c) != "video_not_found" {
				t.Fatalf("%s reading %s = %d %s, want the job to be invisible", name, path, c.Code, c.Body.String())
			}
		}
	}
	if c := e.do(http.MethodGet, "/v1/videos/"+id, "", one); c.Code != http.StatusOK {
		t.Fatalf("owner read = %d", c.Code)
	}
}

// --- B18 ---

func TestModelStoreSaveErrorsAreReported(t *testing.T) {
	h := newAuthHarness(t)
	h.setup()
	const id = "grok-5-persist-test"
	t.Cleanup(func() { h.srv.modelRegistry().Remove(id) })
	if w := h.do(http.MethodPost, "/admin/api/models", map[string]any{"id": id, "kind": "console-chat"}); w.Code != http.StatusOK {
		t.Fatalf("add = %d %s", w.Code, w.Body.String())
	}
	// From here on the store cannot write.
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.srv.custom = newModelStore(filepath.Join(blocker, "custom_models.json"))

	if w := h.do(http.MethodPost, "/admin/api/models/"+id+"/enabled", map[string]any{"enabled": false}); w.Code != http.StatusInternalServerError {
		t.Fatalf("enable toggle = %d, want 500", w.Code)
	}
	if _, ok := h.srv.modelRegistry().Resolve(id); !ok {
		t.Fatal("registry changed although the change could not be persisted")
	}
	if w := h.do(http.MethodDelete, "/admin/api/models/"+id, nil); w.Code != http.StatusInternalServerError {
		t.Fatalf("delete = %d, want 500", w.Code)
	}
	if h.srv.modelRegistry().Get(id) == nil {
		t.Fatal("model removed from the registry although the deletion could not be persisted")
	}
}

// --- owned state, no package globals ---

func TestServersDoNotShareState(t *testing.T) {
	useConfig(t, "")
	a := NewServerFromDeps(Deps{DataDir: t.TempDir(), BcryptCost: 4})
	b := NewServerFromDeps(Deps{DataDir: t.TempDir(), BcryptCost: 4})
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	if _, err := a.keyStore().create("only on a"); err != nil {
		t.Fatal(err)
	}
	if a.keyStore().count() != 1 || b.keyStore().count() != 0 {
		t.Fatalf("key stores are shared: a=%d b=%d", a.keyStore().count(), b.keyStore().count())
	}
}

func TestNewServerDoesNotTouchDataDirEnv(t *testing.T) {
	t.Setenv("DATA_DIR", "")
	s := NewServerFromDeps(Deps{DataDir: t.TempDir()})
	t.Cleanup(func() { _ = s.Close() })
	if got := os.Getenv("DATA_DIR"); got != "" {
		t.Fatalf("DATA_DIR = %q, NewServer must not set it", got)
	}
}

func TestCloseFlushesDebouncedStores(t *testing.T) {
	dir := t.TempDir()
	s := NewServerFromDeps(Deps{DataDir: dir, BcryptCost: 4})
	k, err := s.keyStore().create("k")
	if err != nil {
		t.Fatal(err)
	}
	s.keyStore().touch(s.keyStore().match(k.Key))   // debounced by 15 s
	s.Usage.record("grok-4", 200, time.Millisecond) // debounced by 20 s
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	keys := newAPIKeyStore(filepath.Join(dir, "api_keys.json"))
	if e := keys.match(k.Key); e == nil || e.Requests != 1 {
		t.Fatalf("key usage not flushed on Close: %+v", e)
	}
	usage := newUsageRecorder(filepath.Join(dir, "usage.json"))
	if tot := usage.snapshot(1)["totals"].(gin.H); tot["requests"] != 1 {
		t.Fatalf("usage not flushed on Close: %v", tot)
	}
}

// --- review follow-ups ---

// A panic inside an attempt (recovered by gin.Recovery, recoverJob or the
// background guard) must still release the pooled account's in-flight slot.
func TestPanickingAttemptReleasesTheAccount(t *testing.T) {
	e := newTestEnv(t, "", map[string][]string{"basic": {tokA}})
	e.dir.SetMaxInflight(1)
	spec, ok := e.srv.modelRegistry().Resolve("grok-4.20-fast")
	if !ok {
		t.Fatal("model not found")
	}
	ctx := withCaller(context.Background(), Caller{PoolAllowed: true})

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected the attempt to panic")
			}
		}()
		_, _ = e.srv.acquire(ctx, callerFrom(ctx), spec, acquireOptions{}, func(context.Context, *account.Lease) (bool, error) {
			panic("boom")
		})
	}()

	res, err := e.srv.acquire(ctx, callerFrom(ctx), spec, acquireOptions{}, func(context.Context, *account.Lease) (bool, error) {
		return false, nil
	})
	if err != nil || res.Token != tokA {
		t.Fatalf("account stayed leased after a panic: token=%q err=%v", res.Token, err)
	}
}

// Work started after Close (a handler that outlived http.Shutdown) is dropped
// instead of racing Close's wait.
func TestBackgroundAfterCloseIsDropped(t *testing.T) {
	e := newTestEnv(t, "", nil)
	if err := e.srv.Close(); err != nil {
		t.Fatal(err)
	}
	ran := make(chan struct{}, 1)
	e.srv.background("late", 0, func(context.Context) { ran <- struct{}{} })
	select {
	case <-ran:
		t.Fatal("background task ran after Close")
	case <-time.After(50 * time.Millisecond):
	}
}

// Once the upstream accepted a video generation, a poll failure must not start
// a second (billed) generation on another account.
func TestConsoleVideoPollFailureDoesNotRegenerateOnAnotherAccount(t *testing.T) {
	e := newTestEnv(t, "[retry]\nmax_retries = 3\n", map[string][]string{"basic": {tokA, tokB}})
	var created atomic.Int32
	e.up.consoleVideo = func(token string, p map[string]any) (map[string]any, error) {
		created.Add(1)
		return map[string]any{"request_id": "r1"}, nil
	}
	e.up.consoleStatus = func(token, id string) (map[string]any, error) {
		return nil, platform.UpstreamError("overloaded", 503, "")
	}
	w := e.postForm("/v1/videos", map[string]string{"model": "grok-imagine-video-console", "prompt": "a cat"}, nil, nil)
	id := jsonBody(t, w)["id"].(string)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if jsonBody(t, e.do(http.MethodGet, "/v1/videos/"+id, "", nil))["status"] == "failed" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := created.Load(); n != 1 {
		t.Fatalf("video generations started = %d, want exactly 1", n)
	}
}

// An upstream that ends without one event is a 502 for the client, but it must
// not count against a healthy account.
func TestEmptyUpstreamIsA502ButDoesNotPenaliseTheAccount(t *testing.T) {
	e := newTestEnv(t, "", map[string][]string{"basic": {tokA}})
	e.up.postStream = func(postCall) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("")), nil }
	w := e.post("/v1/chat/completions", chatBody, nil)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	e.pool.mu.Lock()
	defer e.pool.mu.Unlock()
	for _, fb := range e.pool.feedbacks {
		if fb.Kind != account.FbSuccess {
			t.Fatalf("empty answer penalised the account: %+v", e.pool.feedbacks)
		}
	}
	if e.pool.releases == 0 {
		t.Fatal("lease was not released")
	}
}

// A client that disconnects mid-stream gets no error frame and is not counted
// as a failed request.
func TestClientAbortMidStreamWritesNoErrorFrame(t *testing.T) {
	e := newTestEnv(t, "", map[string][]string{"basic": {tokA}})
	pr, pw := io.Pipe()
	e.up.postStream = func(postCall) (io.ReadCloser, error) {
		go func() { _, _ = pw.Write([]byte(webTextFrame("hello") + "\n")) }()
		return pr, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBodyStream)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { e.engine.ServeHTTP(w, req); close(done) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	_ = pw.CloseWithError(context.Canceled)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after the client went away")
	}
	if strings.Contains(w.Body.String(), `"error"`) {
		t.Fatalf("error frame written to a departed client: %s", w.Body.String())
	}
}

// Thinking blocks are opt-in on /v1/messages: a request without a `thinking`
// field must get no thinking block even when features.thinking is on.
func TestAnthropicThinkingIsOptIn(t *testing.T) {
	for _, stream := range []string{"true", "false"} {
		e := newTestEnv(t, "[features]\nthinking = true\n", map[string][]string{"basic": {tokA}})
		e.up.postStream = scripted(scriptStep{frames: []string{webThinkFrame("hmm"), webTextFrame("Hello")}})
		w := e.post("/v1/messages", `{"model":"grok-4.20-fast","max_tokens":8,"stream":`+stream+`,"messages":[{"role":"user","content":"hi"}]}`, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("stream=%s status = %d: %s", stream, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), `"thinking"`) {
			t.Fatalf("stream=%s: thinking returned although not requested: %s", stream, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "Hello") {
			t.Fatalf("stream=%s: answer missing: %s", stream, w.Body.String())
		}
	}
}
