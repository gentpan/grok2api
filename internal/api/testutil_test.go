package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/gentpan/grok2api/internal/account"
	"github.com/gentpan/grok2api/internal/grok"
)

// postCall records one PostStream the fake upstream served.
type postCall struct {
	URL     string
	Token   string
	Payload []byte
}

// fakeUpstream is a scriptable Upstream. Only the behaviours a test sets are
// available; every other method fails loudly.
type fakeUpstream struct {
	mu sync.Mutex

	posts []postCall

	// postStream decides the answer to PostStream; default: empty body.
	postStream func(call postCall) (io.ReadCloser, error)
	gateway    func(token, modelName, prompt string) <-chan grok.GatewayEvent
	images     func(token, prompt string, n int) <-chan grok.ImagineEvent

	getJSON       func(url, token string) (map[string]any, error)
	getBytes      func(url, token string) (io.ReadCloser, error)
	consoleImage  func(token string, payload map[string]any) (map[string]any, error)
	consoleEdit   func(token string, payload map[string]any) (map[string]any, error)
	consoleVideo  func(token string, payload map[string]any) (map[string]any, error)
	consoleStatus func(token, requestID string) (map[string]any, error)
	consoleDL     func(url string) (io.ReadCloser, error)
	listAssets    func(token string) (map[string]any, error)
	deleteAsset   func(token, id string) (map[string]any, error)
	usage         account.UsageFetcher

	tokens []string // every token any call was made with, in order
}

func (f *fakeUpstream) note(token string) {
	f.mu.Lock()
	f.tokens = append(f.tokens, token)
	f.mu.Unlock()
}

func (f *fakeUpstream) usedTokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.tokens...)
}

func (f *fakeUpstream) postCalls() []postCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]postCall(nil), f.posts...)
}

func (f *fakeUpstream) PostStream(_ context.Context, u, token string, payload []byte, _ ...grok.RequestOption) (io.ReadCloser, error) {
	c := postCall{URL: u, Token: token, Payload: payload}
	f.mu.Lock()
	f.posts = append(f.posts, c)
	f.tokens = append(f.tokens, token)
	h := f.postStream
	f.mu.Unlock()
	if h == nil {
		return io.NopCloser(strings.NewReader("")), nil
	}
	return h(c)
}

func (f *fakeUpstream) GetJSON(_ context.Context, u, token string, _ ...grok.RequestOption) (map[string]any, error) {
	f.note(token)
	if f.getJSON == nil {
		return nil, errNotScripted("GetJSON")
	}
	return f.getJSON(u, token)
}

func (f *fakeUpstream) GetBytes(_ context.Context, u, token string, _ ...grok.RequestOption) (io.ReadCloser, error) {
	f.note(token)
	if f.getBytes == nil {
		return nil, errNotScripted("GetBytes")
	}
	return f.getBytes(u, token)
}

func (f *fakeUpstream) StreamGatewayChat(_ context.Context, token, modelName, prompt string, _ []string) <-chan grok.GatewayEvent {
	f.note(token)
	if f.gateway == nil {
		ch := make(chan grok.GatewayEvent, 1)
		ch <- grok.GatewayEvent{Err: errNotScripted("StreamGatewayChat")}
		close(ch)
		return ch
	}
	return f.gateway(token, modelName, prompt)
}

func (f *fakeUpstream) StreamImages(_ context.Context, token, prompt, _ string, n int, _, _ bool) <-chan grok.ImagineEvent {
	f.note(token)
	if f.images == nil {
		ch := make(chan grok.ImagineEvent, 1)
		ch <- grok.ImagineEvent{Type: grok.ImagineEventError, Error: "not scripted"}
		close(ch)
		return ch
	}
	return f.images(token, prompt, n)
}

func (f *fakeUpstream) ConsoleImageGeneration(_ context.Context, token string, p map[string]any) (map[string]any, error) {
	f.note(token)
	if f.consoleImage == nil {
		return nil, errNotScripted("ConsoleImageGeneration")
	}
	return f.consoleImage(token, p)
}

func (f *fakeUpstream) ConsoleImageEdit(_ context.Context, token string, p map[string]any) (map[string]any, error) {
	f.note(token)
	if f.consoleEdit == nil {
		return nil, errNotScripted("ConsoleImageEdit")
	}
	return f.consoleEdit(token, p)
}

func (f *fakeUpstream) ConsoleVideoGeneration(_ context.Context, token string, p map[string]any) (map[string]any, error) {
	f.note(token)
	if f.consoleVideo == nil {
		return nil, errNotScripted("ConsoleVideoGeneration")
	}
	return f.consoleVideo(token, p)
}

func (f *fakeUpstream) ConsoleVideoStatus(_ context.Context, token, id string) (map[string]any, error) {
	f.note(token)
	if f.consoleStatus == nil {
		return nil, errNotScripted("ConsoleVideoStatus")
	}
	return f.consoleStatus(token, id)
}

func (f *fakeUpstream) DownloadConsoleVideo(_ context.Context, u string) (io.ReadCloser, error) {
	if f.consoleDL == nil {
		return nil, errNotScripted("DownloadConsoleVideo")
	}
	return f.consoleDL(u)
}

func (f *fakeUpstream) ListAssets(_ context.Context, token string) (map[string]any, error) {
	if f.listAssets == nil {
		return nil, errNotScripted("ListAssets")
	}
	return f.listAssets(token)
}

func (f *fakeUpstream) DeleteAsset(_ context.Context, token, id string) (map[string]any, error) {
	if f.deleteAsset == nil {
		return nil, errNotScripted("DeleteAsset")
	}
	return f.deleteAsset(token, id)
}

func (f *fakeUpstream) NSFWSequence(context.Context, string) error { return errNotScripted("NSFW") }
func (f *fakeUpstream) DisableNSFW(context.Context, string) error  { return errNotScripted("NSFW") }

func (f *fakeUpstream) UsageFetcher() account.UsageFetcher {
	if f.usage != nil {
		return f.usage
	}
	return &fakeUsage{}
}

type errNotScripted string

func (e errNotScripted) Error() string { return "fakeUpstream: " + string(e) + " not scripted" }

// --- web (grok.com) SSE fixtures ---

// webFrame is one grok.com stream frame carrying a final-text token.
func webTextFrame(tok string) string {
	b, _ := json.Marshal(map[string]any{"result": map[string]any{"response": map[string]any{
		"token": tok, "isThinking": false, "messageTag": "final",
	}}})
	return string(b)
}

func webThinkFrame(tok string) string {
	b, _ := json.Marshal(map[string]any{"result": map[string]any{"response": map[string]any{
		"token": tok, "isThinking": true, "messageTag": "thinking", "rolloutId": "Grok",
	}}})
	return string(b)
}

const webSoftStopFrame = `{"result":{"response":{"isSoftStop":true}}}`

// webErrorFrame is an in-band upstream error; code 8 maps to HTTP 429.
func webErrorFrame(msg string, code int) string {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "code": code}})
	return string(b)
}

// sseBody renders data frames as a `data:` SSE body.
func sseBody(frames ...string) io.ReadCloser {
	var b strings.Builder
	for _, f := range frames {
		b.WriteString("data: " + f + "\n\n")
	}
	return io.NopCloser(strings.NewReader(b.String()))
}

// consoleSSE renders console.x.ai events (event + data pairs).
func consoleSSE(events ...[2]string) io.ReadCloser {
	var b strings.Builder
	for _, e := range events {
		b.WriteString("event: " + e[0] + "\ndata: " + e[1] + "\n\n")
	}
	return io.NopCloser(strings.NewReader(b.String()))
}

func consoleDelta(text string) [2]string {
	b, _ := json.Marshal(map[string]any{"delta": text})
	return [2]string{"response.output_text.delta", string(b)}
}

func consoleCompleted() [2]string {
	return [2]string{"response.completed", `{"response":{"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`}
}

// --- environment ---

// testEnv is a full Server over a real account repository + directory and a
// fake upstream.
type testEnv struct {
	t       *testing.T
	repo    *account.TxtRepository
	dir     *account.Directory
	pool    *recPool
	up      *fakeUpstream
	refresh *fakeRefresher
	srv     *Server
	engine  *gin.Engine
}

// newTestEnv seeds accounts as pool -> tokens (tokens must look like real
// ones: >= 16 chars) and wires a Server around them.
func newTestEnv(t *testing.T, toml string, pools map[string][]string) *testEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	useConfig(t, toml)
	ctx := context.Background()
	repo := account.NewTxtRepository(filepath.Join(t.TempDir(), "accounts.jsonl"))
	if err := repo.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	for pool, toks := range pools {
		var ups []account.Upsert
		for _, tok := range toks {
			ups = append(ups, account.Upsert{Token: tok, Pool: pool})
		}
		if _, err := repo.UpsertAccounts(ctx, ups); err != nil {
			t.Fatal(err)
		}
	}
	dir := account.NewDirectory(repo)
	if err := dir.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	up := &fakeUpstream{}
	fr := &fakeRefresher{real: account.NewRefreshService(repo, up.UsageFetcher())}
	pool := &recPool{Directory: dir}
	srv := newEnvServer(t, repo, pool, up, fr)
	t.Cleanup(func() { _ = srv.Close() })
	e := &testEnv{t: t, repo: repo, dir: dir, pool: pool, up: up, refresh: fr, srv: srv}
	e.engine = srv.Router()
	return e
}

// do performs one request against the full router.
func (e *testEnv) do(method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = bytes.NewReader([]byte(body))
	}
	r := httptest.NewRequest(method, path, rd)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.engine.ServeHTTP(w, r)
	return w
}

func (e *testEnv) post(path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.do(http.MethodPost, path, body, hdr)
}

// status returns the persisted status of a token.
func (e *testEnv) status(tok string) account.Status {
	e.t.Helper()
	recs, err := e.repo.GetAccounts(context.Background(), []string{tok})
	if err != nil || len(recs) == 0 || recs[0] == nil {
		e.t.Fatalf("account %s missing: %v", tok, err)
	}
	return recs[0].Status
}

func jsonBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("response is not JSON (%d): %q", w.Code, w.Body.String())
	}
	return m
}

func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	m := jsonBody(t, w)
	obj, _ := m["error"].(map[string]any)
	code, _ := obj["code"].(string)
	return code
}

const (
	tokA = "tok-aaaaaaaaaaaaaaaaaaaa"
	tokB = "tok-bbbbbbbbbbbbbbbbbbbb"
	tokC = "tok-cccccccccccccccccccc"
)

// sseDataFrames returns the payloads of every `data:` frame in an SSE body.
func sseDataFrames(body string) []string {
	var out []string
	for _, block := range strings.Split(body, "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "data: ") {
				out = append(out, strings.TrimPrefix(line, "data: "))
			}
		}
	}
	return out
}

// sseEvents returns the `event:` names in order.
func sseEvents(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "event: ") {
			out = append(out, strings.TrimPrefix(line, "event: "))
		}
	}
	return out
}

// memConversations is an in-memory ConversationStore.
type memConversations struct {
	mu sync.Mutex
	m  map[string]*grok.ConversationState
}

func (c *memConversations) Get(token string) *grok.ConversationState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[token]
}

func (c *memConversations) Set(token, conv, resp string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]*grok.ConversationState{}
	}
	c.m[token] = &grok.ConversationState{ConversationID: conv, LastResponseID: resp}
}

func (c *memConversations) Clear(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, token)
}

// webImageFrame is a grok.com image card frame (progress, optional final URL).
func webImageFrame(progress int, imageID, path string) string {
	chunk := map[string]any{"progress": progress, "imageUuid": imageID}
	if path != "" {
		chunk["imageUrl"] = path
	}
	inner, _ := json.Marshal(map[string]any{"id": "card-" + imageID, "image_chunk": chunk})
	b, _ := json.Marshal(map[string]any{"result": map[string]any{"response": map[string]any{
		"cardAttachment": map[string]any{"jsonData": string(inner)},
	}}})
	return string(b)
}

// fakeRefresher records refresh traffic without touching upstream. Failure
// recording is delegated to a real RefreshService so account state changes
// (expired, ...) stay observable through the repository.
type fakeRefresher struct {
	real *account.RefreshService

	mu        sync.Mutex
	refreshed []string
	onDemand  int
	failures  []string
}

func (f *fakeRefresher) RefreshTokens(_ context.Context, tokens []string) (int, int, error) {
	f.mu.Lock()
	f.refreshed = append(f.refreshed, tokens...)
	f.mu.Unlock()
	return len(tokens), 0, nil
}

func (f *fakeRefresher) RefreshOnDemand(context.Context) error {
	f.mu.Lock()
	f.onDemand++
	f.mu.Unlock()
	return nil
}

func (f *fakeRefresher) RecordFailure(ctx context.Context, token string, modeID int, err error) {
	f.mu.Lock()
	f.failures = append(f.failures, token)
	f.mu.Unlock()
	f.real.RecordFailure(ctx, token, modeID, err)
}

func (f *fakeRefresher) onDemandCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.onDemand
}

// fbCall is one Feedback the pool received.
type fbCall struct {
	Token string
	Kind  account.FeedbackKind
	Mode  int
}

// recPool is the real directory plus a record of how it was used.
type recPool struct {
	*account.Directory
	mu        sync.Mutex
	feedbacks []fbCall
	reserved  []string
	releases  int
}

func (p *recPool) Reserve(pools []int, mode int, exclude, tags []string) (*account.Lease, int) {
	l, m := p.Directory.Reserve(pools, mode, exclude, tags)
	if l != nil {
		p.mu.Lock()
		p.reserved = append(p.reserved, l.Token)
		p.mu.Unlock()
	}
	return l, m
}

func (p *recPool) Release(l *account.Lease) {
	p.mu.Lock()
	p.releases++
	p.mu.Unlock()
	p.Directory.Release(l)
}

func (p *recPool) Feedback(token string, kind account.FeedbackKind, modeID int, remaining *int, resetAtMs *int64) {
	p.mu.Lock()
	p.feedbacks = append(p.feedbacks, fbCall{token, kind, modeID})
	p.mu.Unlock()
	p.Directory.Feedback(token, kind, modeID, remaining, resetAtMs)
}

func (p *recPool) kinds(token string) []account.FeedbackKind {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []account.FeedbackKind
	for _, f := range p.feedbacks {
		if token == "" || f.Token == token {
			out = append(out, f.Kind)
		}
	}
	return out
}

func (p *recPool) reservations() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.reserved...)
}

func newEnvServer(t *testing.T, repo account.Repository, dir Pool, up *fakeUpstream, fr *fakeRefresher) *Server {
	t.Helper()
	return &Server{
		Repo:        repo,
		Directory:   dir,
		Refresh:     fr,
		Transport:   up,
		ConvTracker: &memConversations{},
	}
}
