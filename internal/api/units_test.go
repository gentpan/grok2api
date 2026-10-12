package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/gentpan/grok2api/internal/account"
	"github.com/gentpan/grok2api/internal/grok"
	"github.com/gentpan/grok2api/internal/model"
	"github.com/gentpan/grok2api/internal/platform"
)

func TestSSEReaderWebAndConsoleFraming(t *testing.T) {
	web := newSSEReader(strings.NewReader(": heartbeat\n\nevent: x\ndata: {\"a\":1}\n\n{\"b\":2}\ndata: [DONE]\ndata: {\"never\":1}\n"), false)
	var got []string
	for {
		f, ok := web.next()
		if !ok {
			break
		}
		got = append(got, f.Data)
	}
	if strings.Join(got, "|") != `{"a":1}|{"b":2}` {
		t.Fatalf("web frames = %v", got)
	}

	console := newSSEReader(strings.NewReader("event: response.output_text.delta\ndata: {\"delta\":\"a\"}\n\nevent: response.completed\ndata: {}\n\n"), true)
	f, ok := console.next()
	if !ok || f.Event != "response.output_text.delta" || f.Data != `{"delta":"a"}` {
		t.Fatalf("first console frame = %+v ok=%v", f, ok)
	}
	f, ok = console.next()
	if !ok || f.Event != "response.completed" {
		t.Fatalf("second console frame = %+v ok=%v", f, ok)
	}
	if _, ok := console.next(); ok || console.err() != nil {
		t.Fatalf("expected a clean end, err=%v", console.err())
	}
}

func TestSSEReaderHandlesLongLines(t *testing.T) {
	long := strings.Repeat("x", 3*1024*1024)
	r := newSSEReader(strings.NewReader("data: "+long+"\n\n"), false)
	f, ok := r.next()
	if !ok || len(f.Data) != len(long) {
		t.Fatalf("long frame: ok=%v len=%d err=%v", ok, len(f.Data), r.err())
	}
}

func TestCollectorTextAndThinking(t *testing.T) {
	var a collector
	for _, ev := range []chatEvent{
		{Kind: evThinking, Content: "[a]\n"},
		{Kind: evImageProgress, Content: "40"},
		{Kind: evText, Content: "hello "},
		{Kind: evText, Content: "world"},
		{Kind: evImage, Content: "https://x/1.png"},
		{Kind: evImage, Content: "https://x/2.png"},
	} {
		a.add(ev)
	}
	if got := a.Text(); got != "hello world\n\n![image](https://x/1.png)\n\n![image](https://x/2.png)" {
		t.Fatalf("text = %q", got)
	}
	if got := a.Thinking(false); got != "[a]\nimage generating 40%\n" {
		t.Fatalf("thinking = %q", got)
	}
	if got := a.Thinking(true); got != "[a]\nimage generating 40%" {
		t.Fatalf("trimmed thinking = %q", got)
	}
	var onlyImages collector
	onlyImages.add(chatEvent{Kind: evImage, Content: "u"})
	if got := onlyImages.Text(); got != "![image](u)" {
		t.Fatalf("image-only text = %q", got)
	}
}

func TestCallerOwnerID(t *testing.T) {
	if (Caller{}).OwnerID() != "" || (Caller{PoolAllowed: true}).OwnerID() != "" {
		t.Fatal("anonymous callers share the empty owner")
	}
	a := Caller{KeyID: "abc"}.OwnerID()
	b := Caller{SSOToken: "tok-1"}.OwnerID()
	c := Caller{SSOToken: "tok-2"}.OwnerID()
	if a == "" || b == "" || c == "" || a == b || b == c {
		t.Fatalf("owners must be distinct and non-empty: %q %q %q", a, b, c)
	}
	if strings.Contains(b, "tok-1") {
		t.Fatal("owner id leaks the SSO token")
	}
}

func TestAnthropicErrorTypes(t *testing.T) {
	cases := []struct {
		err  *platform.AppError
		want string
	}{
		{platform.AuthError("x"), "authentication_error"},
		{platform.RateLimitError("x"), "rate_limit_error"},
		{platform.ValidationError("x", "p"), "invalid_request_error"},
		{platform.UpstreamError("x", 502, ""), "api_error"},
		{platform.UpstreamError("x", 503, ""), "overloaded_error"},
		{platform.UpstreamError("x", 403, ""), "permission_error"},
		{platform.NewAppError("x", platform.ErrValidation, "nf", 404), "not_found_error"},
		{platform.NewAppError("x", platform.ErrServer, "boom", 500), "api_error"},
	}
	for _, tc := range cases {
		if got := anthropicErrorType(tc.err); got != tc.want {
			t.Fatalf("status %d kind %s: got %q want %q", tc.err.Status, tc.err.Kind, got, tc.want)
		}
	}
}

func TestFailLimiterBlocksAndSweepsStalePeers(t *testing.T) {
	l := newFailLimiter(2, 50*time.Millisecond)
	l.fail("1.1.1.1")
	if l.blocked("1.1.1.1") {
		t.Fatal("blocked after one failure")
	}
	l.fail("1.1.1.1")
	if !l.blocked("1.1.1.1") {
		t.Fatal("not blocked after reaching the limit")
	}
	time.Sleep(70 * time.Millisecond)
	if l.blocked("1.1.1.1") {
		t.Fatal("still blocked after the window")
	}
	// Many one-off peers must not accumulate forever.
	for i := 0; i < limiterSweepSize+10; i++ {
		l.fail("10.0." + string(rune('a'+i%26)) + "." + strings.Repeat("x", i%7) + string(rune('0'+i%10)) + string(rune('A'+i/26%26)))
	}
	time.Sleep(70 * time.Millisecond)
	l.fail("9.9.9.9")
	l.mu.Lock()
	n := len(l.hits)
	l.mu.Unlock()
	if n > 3 {
		t.Fatalf("stale peers kept: %d entries", n)
	}
}

func TestVideoJobsOwnershipAndEviction(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	jobs := newVideoJobs(time.Hour)
	jobs.now = func() time.Time { return now }

	old := &videoJob{ID: "video_old", Owner: "key:a"}
	old.complete("u", "", now.Add(-2*time.Hour))
	jobs.add(old)
	fresh := &videoJob{ID: "video_new", Owner: "key:a"}
	jobs.add(fresh)

	if jobs.get("video_old", "key:a") != nil {
		t.Fatal("finished job past retention was not evicted")
	}
	if jobs.get("video_new", "key:a") == nil {
		t.Fatal("owner cannot read its job")
	}
	if jobs.get("video_new", "key:b") != nil || jobs.get("video_new", "") != nil {
		t.Fatal("another owner can read the job")
	}
	fresh.fail("first", now)
	fresh.fail("second", now)
	if fresh.Error == nil || fresh.Error.Message != "first" {
		t.Fatalf("a failed job must keep its first error: %+v", fresh.Error)
	}
}

func TestRetryConfigParsing(t *testing.T) {
	cfg := mapSettings{"retry.max_retries": -4, "retry.on_codes": []string{"429", " 500 ", "x"}}
	rc := newRetryConfig(cfg, account.StrategyQuota)
	if rc.MaxRetries != 0 {
		t.Fatalf("negative max_retries must clamp to 0, got %d", rc.MaxRetries)
	}
	if _, ok := rc.OnCodes[500]; !ok || len(rc.OnCodes) != 2 {
		t.Fatalf("codes = %v", rc.OnCodes)
	}
	if !rc.AutoModeFallback {
		t.Fatal("auto mode fallback defaults to on")
	}
	empty := newRetryConfig(mapSettings{}, "")
	if empty.MaxRetries != 1 || len(empty.OnCodes) != 3 {
		t.Fatalf("defaults = %+v", empty)
	}
}

type mapSettings map[string]any

func (m mapSettings) GetStr(key, def string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return def
}
func (m mapSettings) GetInt(key string, def int) int {
	if v, ok := m[key].(int); ok {
		return v
	}
	return def
}
func (m mapSettings) GetBool(key string, def bool) bool {
	if v, ok := m[key].(bool); ok {
		return v
	}
	return def
}
func (m mapSettings) GetList(key string, def []string) []string {
	if v, ok := m[key].([]string); ok {
		return v
	}
	return def
}

// --- acquire, driven directly ---

func TestAcquireSettlesEveryAttempt(t *testing.T) {
	e := newTestEnv(t, "[retry]\nmax_retries = 2\n", map[string][]string{"basic": {tokA, tokB, tokC}})
	spec, _ := e.srv.modelRegistry().Resolve("grok-4.20-fast")
	caller := Caller{PoolAllowed: true}

	var used []string
	res, err := e.srv.acquire(context.Background(), caller, spec, acquireOptions{}, func(_ context.Context, l *account.Lease) (bool, error) {
		used = append(used, l.Token)
		if len(used) < 3 {
			return false, platform.UpstreamError("slow", 429, "")
		}
		return false, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(used) != 3 || used[0] == used[1] || used[1] == used[2] || used[0] == used[2] {
		t.Fatalf("attempts = %v, want three distinct accounts", used)
	}
	if res.Token != used[2] || res.BYO {
		t.Fatalf("result = %+v", res)
	}
	if e.pool.releases != 3 {
		t.Fatalf("releases = %d, want one per lease", e.pool.releases)
	}
	kinds := e.pool.kinds("")
	if len(kinds) != 3 || kinds[0] != account.FbRateLimited || kinds[2] != account.FbSuccess {
		t.Fatalf("feedback = %v", kinds)
	}
}

func TestAcquireDoesNotRetryAfterCommit(t *testing.T) {
	e := newTestEnv(t, "[retry]\nmax_retries = 3\n", map[string][]string{"basic": {tokA, tokB}})
	spec, _ := e.srv.modelRegistry().Resolve("grok-4.20-fast")
	calls := 0
	res, err := e.srv.acquire(context.Background(), Caller{PoolAllowed: true}, spec, acquireOptions{},
		func(context.Context, *account.Lease) (bool, error) {
			calls++
			return true, platform.UpstreamError("broke mid-stream", 429, "")
		})
	if err == nil || calls != 1 || !res.Committed {
		t.Fatalf("calls=%d committed=%v err=%v", calls, res.Committed, err)
	}
}

func TestAcquireHonoursExcludeAndCustomMessage(t *testing.T) {
	e := newTestEnv(t, "", map[string][]string{"basic": {tokA}})
	spec, _ := e.srv.modelRegistry().Resolve("grok-4.20-fast")
	_, err := e.srv.acquire(context.Background(), Caller{PoolAllowed: true}, spec,
		acquireOptions{Exclude: []string{tokA}, NoAccountsMessage: "nobody home"},
		func(context.Context, *account.Lease) (bool, error) {
			t.Fatal("excluded account was leased")
			return false, nil
		})
	appErr := platformAppError(err)
	if appErr == nil || appErr.Status != http.StatusTooManyRequests || appErr.Message != "nobody home" {
		t.Fatalf("err = %v", err)
	}
}

// --- chat sources ---

func TestGatewaySourcePrimesOnFirstEvent(t *testing.T) {
	ch := make(chan grok.GatewayEvent, 2)
	ch <- grok.GatewayEvent{Err: errors.New("handshake failed")}
	close(ch)
	if _, err := openGatewaySource(context.Background(), ch); err == nil || !strings.Contains(err.Error(), "handshake") {
		t.Fatalf("connect error not surfaced before commit: %v", err)
	}

	ch = make(chan grok.GatewayEvent, 3)
	ch <- grok.GatewayEvent{Kind: grok.EventThinking, Content: "t", ConversationID: "c", ResponseID: "r"}
	ch <- grok.GatewayEvent{Kind: grok.EventText, Content: "x"}
	ch <- grok.GatewayEvent{Kind: grok.EventSoftStop}
	close(ch)
	src, err := openGatewaySource(context.Background(), ch)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []chatEventKind
	for {
		evs, err := src.Next()
		for _, ev := range evs {
			kinds = append(kinds, ev.Kind)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(kinds) != 2 || kinds[0] != evThinking || kinds[1] != evText {
		t.Fatalf("kinds = %v", kinds)
	}
	if m := src.Meta(); m.ConversationID != "c" || m.ResponseID != "r" {
		t.Fatalf("meta = %+v", m)
	}
}

func TestGatewaySourceStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan grok.GatewayEvent) // never delivers
	cancel()
	if _, err := openGatewaySource(ctx, ch); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestImagineSourceAggregatesProgressMonotonically(t *testing.T) {
	ch := make(chan grok.ImagineEvent, 8)
	ch <- grok.ImagineEvent{Type: grok.ImagineEventProgress, ImageID: "a", Progress: 20}
	ch <- grok.ImagineEvent{Type: grok.ImagineEventProgress, ImageID: "a", Progress: 10} // not an increase
	ch <- grok.ImagineEvent{Type: grok.ImagineEventProgress, ImageID: "a", Progress: 60}
	ch <- grok.ImagineEvent{Type: grok.ImagineEventImage, ImageID: "a", URL: "/images/a.png"}
	close(ch)
	src, err := openImagineSource(context.Background(), ch, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	var thinking []string
	var images []string
	for {
		evs, err := src.Next()
		for _, ev := range evs {
			switch ev.Kind {
			case evThinking:
				thinking = append(thinking, ev.Content)
			case evImage:
				images = append(images, ev.Content)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(thinking) != 2 || !strings.Contains(thinking[0], "20%") || !strings.Contains(thinking[1], "60%") {
		t.Fatalf("thinking = %q", thinking)
	}
	if len(images) != 1 || images[0] != "https://assets.grok.com/images/a.png" {
		t.Fatalf("images = %v", images)
	}
}

// --- conversation continuation (B13) ---

func TestConversationContinuationIsScopedToAccountAndModel(t *testing.T) {
	e := newTestEnv(t, "[features]\nconversation_continuation = true\n", map[string][]string{"basic": {tokA}})
	convFrames := []string{
		`{"result":{"conversation":{"conversationId":"conv-1"}}}`,
		`{"result":{"response":{"userResponse":{"responseId":"resp-1"}}}}`,
		webTextFrame("ok"),
	}
	e.up.postStream = scripted(scriptStep{frames: convFrames})
	body := func(modelName string) string {
		return `{"model":"` + modelName + `","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	}
	if w := e.post("/v1/chat/completions", body("grok-4.20-fast"), nil); w.Code != http.StatusOK {
		t.Fatalf("first = %d: %s", w.Code, w.Body.String())
	}
	e.up.postStream = scripted(scriptStep{frames: []string{webTextFrame("again")}})
	if w := e.post("/v1/chat/completions", body("grok-4.20-fast"), nil); w.Code != http.StatusOK {
		t.Fatalf("second = %d: %s", w.Code, w.Body.String())
	}
	calls := e.up.postCalls()
	if len(calls) != 2 || calls[0].URL != grok.Chat || !strings.Contains(calls[1].URL, "/conversations/conv-1/responses") {
		t.Fatalf("same model must continue the conversation: %+v", calls)
	}

	// A different model on the same account must start its own conversation.
	e.up.postStream = scripted(scriptStep{frames: []string{webTextFrame("other")}})
	if w := e.post("/v1/chat/completions", body("grok-4.3-fast"), nil); w.Code != http.StatusOK {
		t.Fatalf("third = %d: %s", w.Code, w.Body.String())
	}
	calls = e.up.postCalls()
	if last := calls[len(calls)-1]; last.URL != grok.Chat {
		t.Fatalf("a different model reused another model's conversation: %s", last.URL)
	}
}

// --- B17 ---

func TestUsageCountsStreamFailuresAfterHeaders(t *testing.T) {
	e := newTestEnv(t, "[retry]\nmax_retries = 0\n", map[string][]string{"basic": {tokA}})
	e.srv.Usage = newUsageRecorder("")
	e.up.postStream = scripted(scriptStep{frames: []string{webTextFrame("partial"), webErrorFrame("boom", 3)}})
	w := e.post("/v1/chat/completions", chatBodyStream, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	tot := e.srv.Usage.snapshot(1)["totals"].(gin.H)
	if tot["requests"] != 1 || tot["errors"] != 1 {
		t.Fatalf("totals = %v, the broken stream must count as an error", tot)
	}
}

// --- router / envelopes ---

func TestModelGetUsesEnvelopeAnd404(t *testing.T) {
	e := newTestEnv(t, "", nil)
	w := e.do(http.MethodGet, "/v1/models/nope", "", nil)
	if w.Code != http.StatusNotFound || errorCode(t, w) != "model_not_found" {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	e = newTestEnv(t, "", map[string][]string{"basic": {tokA}})
	w = e.do(http.MethodGet, "/v1/models", "", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "grok-4.20-fast") {
		t.Fatalf("models = %d %s", w.Code, w.Body.String())
	}
}

func TestOptionsAndHealth(t *testing.T) {
	e := newTestEnv(t, "[app]\napi_key = \"k\"\n", nil)
	if w := e.do(http.MethodGet, "/health", "", nil); w.Code != http.StatusOK {
		t.Fatalf("health = %d", w.Code)
	}
	if w := e.do(http.MethodOptions, "/v1/chat/completions", "", nil); w.Code != http.StatusNoContent {
		t.Fatalf("preflight = %d", w.Code)
	}
	if w := e.do(http.MethodGet, "/meta", "", nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "version") {
		t.Fatalf("meta = %d %s", w.Code, w.Body.String())
	}
}

func TestSpecModelsAreResolvedThroughServerRegistry(t *testing.T) {
	// A Server given its own registry never consults the process-wide one.
	e := newTestEnv(t, "", map[string][]string{"basic": {tokA}})
	e.srv.Models = onlyModel{name: "private-model", spec: &model.Spec{
		ModelName: "private-model", ModeId: model.ModeFast, Tier: model.TierBasic, Capability: model.CapChat, Enabled: true,
	}}
	e.up.postStream = scripted(scriptStep{frames: []string{webTextFrame("ok")}})
	if w := e.post("/v1/chat/completions", `{"model":"private-model","stream":false,"messages":[{"role":"user","content":"hi"}]}`, nil); w.Code != http.StatusOK {
		t.Fatalf("private model = %d: %s", w.Code, w.Body.String())
	}
	if w := e.post("/v1/chat/completions", `{"model":"grok-4.20-fast","stream":false,"messages":[{"role":"user","content":"hi"}]}`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("global model leaked into the private registry: %d", w.Code)
	}
}

// onlyModel is a single-entry ModelRegistry.
type onlyModel struct {
	name string
	spec *model.Spec
}

func (o onlyModel) Resolve(n string) (*model.Spec, bool) {
	if n == o.name {
		return o.spec, true
	}
	return nil, false
}
func (o onlyModel) Get(n string) *model.Spec {
	if n == o.name {
		return o.spec
	}
	return nil
}
func (o onlyModel) ListEnabled() []*model.Spec   { return []*model.Spec{o.spec} }
func (o onlyModel) All() []*model.Spec           { return []*model.Spec{o.spec} }
func (o onlyModel) Register(model.Spec) error    { return errors.New("read only") }
func (o onlyModel) Remove(string) bool           { return false }
func (o onlyModel) SetEnabled(string, bool) bool { return false }

func TestChallengeTableIsBoundedPerPeer(t *testing.T) {
	a := newAdminAuth("", 4)
	for i := 0; i < maxChallengesPerIP; i++ {
		if _, err := a.putChallenge("login", "203.0.113.1", &webauthn.SessionData{}); err != nil {
			t.Fatalf("challenge %d: %v", i, err)
		}
	}
	if _, err := a.putChallenge("login", "203.0.113.1", &webauthn.SessionData{}); err == nil {
		t.Fatal("one peer filled more than its share of the table")
	}
	if _, err := a.putChallenge("login", "198.51.100.2", &webauthn.SessionData{}); err != nil {
		t.Fatalf("another peer was locked out: %v", err)
	}
}

func TestNewServerWithNilDepsServes(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	useConfig(t, "")
	s := NewServer(nil, nil, nil, nil, nil)
	t.Cleanup(func() { _ = s.Close() })
	e := s.Router()
	for path, want := range map[string]int{"/health": 200, "/admin/auth/status": 200, "/v1/models": 200, "/admin/api/status": 401} {
		w := httptest.NewRecorder()
		e.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != want {
			t.Fatalf("%s = %d, want %d", path, w.Code, want)
		}
	}
	// A chat request with nothing wired is a clean 429, never a panic.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody))
	r.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(w, r)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("chat on an empty server = %d: %s", w.Code, w.Body.String())
	}
}
