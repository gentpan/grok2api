package grok

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gentpan/grok2api/internal/platform"
)

// --- dpopStore -------------------------------------------------------------

func freshSession(name string, ttl time.Duration) dpopSession {
	return dpopSession{accessToken: name, expiresAt: time.Now().Add(ttl)}
}

func TestDPoPStoreSharesOneFetchAndItsFailure(t *testing.T) {
	s := newDPoPStore()
	var calls atomic.Int32
	release := make(chan struct{})
	fetch := func(context.Context) (dpopSession, error) {
		calls.Add(1)
		<-release
		return dpopSession{}, upstreamErr("token endpoint down")
	}

	const waiters = 8
	errs := make(chan error, waiters)
	var started sync.WaitGroup
	for i := 0; i < waiters; i++ {
		started.Add(1)
		go func() {
			started.Done()
			_, err := s.get(context.Background(), "k", fetch)
			errs <- err
		}()
	}
	// Let every goroutine reach get before the loader finishes.
	started.Wait()
	time.Sleep(150 * time.Millisecond)
	close(release)
	for i := 0; i < waiters; i++ {
		if err := <-errs; err == nil || !strings.Contains(err.Error(), "token endpoint down") {
			t.Fatalf("waiter error = %v", err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("fetch ran %d times for %d concurrent callers, want 1", got, waiters)
	}
	if len(s.loading) != 0 {
		t.Fatal("loading marker leaked")
	}
}

func TestDPoPStoreLoaderCancelLetsWaitersRetry(t *testing.T) {
	s := newDPoPStore()
	started := make(chan struct{})
	loaderCtx, cancelLoader := context.WithCancel(context.Background())

	loaderDone := make(chan error, 1)
	go func() {
		_, err := s.get(loaderCtx, "k", func(ctx context.Context) (dpopSession, error) {
			close(started)
			<-ctx.Done()
			return dpopSession{}, ctx.Err()
		})
		loaderDone <- err
	}()
	<-started

	waiterDone := make(chan struct {
		sess dpopSession
		err  error
	}, 1)
	go func() {
		sess, err := s.get(context.Background(), "k", func(context.Context) (dpopSession, error) {
			return freshSession("second", time.Minute), nil
		})
		waiterDone <- struct {
			sess dpopSession
			err  error
		}{sess, err}
	}()
	time.Sleep(50 * time.Millisecond)
	cancelLoader()

	if err := <-loaderDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("loader err = %v", err)
	}
	r := <-waiterDone
	if r.err != nil || r.sess.accessToken != "second" {
		t.Fatalf("waiter result = %#v / %v (a cancelled loader must not fail other callers)", r.sess, r.err)
	}
}

func TestDPoPStoreCachesUntilRefreshSkew(t *testing.T) {
	s := newDPoPStore()
	var calls atomic.Int32
	fetch := func(ttl time.Duration) func(context.Context) (dpopSession, error) {
		return func(context.Context) (dpopSession, error) {
			calls.Add(1)
			return freshSession(fmt.Sprint("t", calls.Load()), ttl), nil
		}
	}
	a, _ := s.get(context.Background(), "k", fetch(time.Hour))
	b, _ := s.get(context.Background(), "k", fetch(time.Hour))
	if a.accessToken != b.accessToken || calls.Load() != 1 {
		t.Fatalf("cache miss: %v %v calls=%d", a.accessToken, b.accessToken, calls.Load())
	}
	// A session inside the refresh skew is reloaded.
	s2 := newDPoPStore()
	s2.get(context.Background(), "k", fetch(dpopRefreshSkew/2))
	before := calls.Load()
	s2.get(context.Background(), "k", fetch(time.Hour))
	if calls.Load() != before+1 {
		t.Fatal("near-expiry session was served from cache")
	}
}

func TestDPoPStoreInvalidateOnlyMatchingAccessToken(t *testing.T) {
	s := newDPoPStore()
	s.get(context.Background(), "k", func(context.Context) (dpopSession, error) {
		return freshSession("current", time.Hour), nil
	})
	s.invalidate("k", "stale")
	if s.size() != 1 {
		t.Fatal("a stale access token evicted the fresh session")
	}
	s.invalidate("k", "current")
	if s.size() != 0 {
		t.Fatal("matching invalidate did not evict")
	}
}

func TestDPoPStoreEvictsExpiredAndBoundsSize(t *testing.T) {
	s := newDPoPStore()
	s.mu.Lock()
	s.sessions["expired"] = freshSession("old", -time.Minute)
	for i := 0; i < maxDPoPSessions+10; i++ {
		s.sessions[fmt.Sprint("k", i)] = freshSession("x", time.Hour+time.Duration(i)*time.Second)
	}
	s.storeLocked("new", freshSession("new", 3*time.Hour))
	n := len(s.sessions)
	_, hasExpired := s.sessions["expired"]
	_, hasNew := s.sessions["new"]
	_, hasOldest := s.sessions["k0"]
	_, hasNewestOfOld := s.sessions[fmt.Sprint("k", maxDPoPSessions+9)]
	s.mu.Unlock()
	if n > maxDPoPSessions {
		t.Fatalf("size = %d, want <= %d", n, maxDPoPSessions)
	}
	if hasExpired || !hasNew || hasOldest || !hasNewestOfOld {
		t.Fatalf("eviction picked the wrong entries: expired=%v new=%v oldest=%v newestOld=%v", hasExpired, hasNew, hasOldest, hasNewestOfOld)
	}
}

func TestBuildDPoPSessionRejectsForeignTokensAs502(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	jwk := publicDPoPJWK(&key.PublicKey)
	thumb, _ := dpopJWKThumbprint(jwk)
	now := time.Now().UTC()
	mkToken := func(exp time.Time, jkt string) string {
		claims, _ := json.Marshal(map[string]any{"exp": exp.Unix(), "cnf": map[string]string{"jkt": jkt}})
		return "h." + base64.RawURLEncoding.EncodeToString(claims) + ".s"
	}
	body := func(token, typ string, expiresIn int) []byte {
		b, _ := json.Marshal(map[string]any{"access_token": token, "token_type": typ, "expires_in": expiresIn})
		return b
	}
	good := mkToken(now.Add(5*time.Minute), thumb)

	if s, err := buildDPoPSession(body(good, "DPoP", 300), key, jwk, now); err != nil || s.accessToken != good {
		t.Fatalf("good session: %v", err)
	}
	cases := map[string][]byte{
		"not json":       []byte("nope"),
		"wrong type":     body(good, "Bearer", 300),
		"empty token":    body("", "DPoP", 300),
		"bad lifetime":   body(good, "DPoP", 0),
		"huge lifetime":  body(good, "DPoP", 100000),
		"foreign key":    body(mkToken(now.Add(5*time.Minute), "other"), "DPoP", 300),
		"expired":        body(mkToken(now.Add(-time.Minute), thumb), "DPoP", 300),
		"near expiry":    body(mkToken(now.Add(time.Second), thumb), "DPoP", 300),
		"malformed jwt":  body("only-two.parts", "DPoP", 300),
		"missing claims": body("h."+base64.RawURLEncoding.EncodeToString([]byte("{}"))+".s", "DPoP", 300),
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := buildDPoPSession(b, key, jwk, now)
			appErr, ok := asAppError(err)
			if !ok || appErr.Status != 502 {
				t.Fatalf("err = %v, want 502 AppError", err)
			}
		})
	}
}

// --- error classification --------------------------------------------------

func TestIsRateLimited(t *testing.T) {
	cases := []struct {
		name string
		code any
		msg  string
		want bool
	}{
		{"grpc code 8", float64(8), "", true},
		{"int code 8", 8, "", true},
		{"json.Number 8", json.Number("8"), "", true},
		{"string code 8", "8", "", true},
		{"rate_limit code", "rate_limit_exceeded", "", true},
		{"rate limit message", nil, "You hit the Rate Limit", true},
		{"too many requests", nil, "Too many requests", true},
		{"resource exhausted", "RESOURCE_EXHAUSTED", "", true},
		{"other number", float64(7), "anti-bot", false},
		{"generate is not a rate", "generate_failed", "could not generate", false},
		{"context limit is not a rate limit", "context_limit_exceeded", "", false},
		{"limit alone", "limit", "limit", false},
		{"nil", nil, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRateLimited(tc.code, tc.msg); got != tc.want {
				t.Fatalf("isRateLimited(%v, %q) = %v, want %v", tc.code, tc.msg, got, tc.want)
			}
		})
	}
	if streamErrorStatus("rate_limit", "") != 429 || streamErrorStatus(nil, "") != 502 {
		t.Fatal("streamErrorStatus")
	}
}

func TestGatewayErrorEventOverMatchFixed(t *testing.T) {
	status := func(raw string) int {
		var event map[string]any
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			t.Fatal(err)
		}
		return appErrorOf(t, parseGatewayEvent(event, &gatewayEventResult{})).Status
	}
	for raw, want := range map[string]int{
		`{"type":"error","error":{"code":"rate_limit_exceeded","message":"x"}}`:    429,
		`{"type":"error","error":{"code":"generate_failed","message":"x"}}`:        502,
		`{"type":"error","error":{"code":"context_limit_exceeded","message":"x"}}`: 502,
		`{"type":"error","error":{"message":"Too many requests"}}`:                 429,
		`{"type":"error","error":"plain string"}`:                                  502,
		`{"type":"error"}`: 502,
		`{"type":"response.done","response":{"status":"failed"}}`: 502,
	} {
		if got := status(raw); got != want {
			t.Errorf("%s: status %d, want %d", raw, got, want)
		}
	}
}

func TestPrefixErrorKeepsStatusAndBody(t *testing.T) {
	orig := platform.UpstreamError("Upstream returned 401", 401, "invalid-credentials")
	got := prefixError("resolve user", orig)
	if got.Status != 401 || got.Body != "invalid-credentials" || got.Message != "resolve user: Upstream returned 401" {
		t.Fatalf("got = %#v", got)
	}
	if orig.Message != "Upstream returned 401" {
		t.Fatal("prefixError mutated its input")
	}
	wrapped := prefixError("ctx", fmt.Errorf("wrap: %w", orig))
	if wrapped.Status != 401 {
		t.Fatalf("wrapped AppError lost its status: %#v", wrapped)
	}
	plain := prefixError("ctx", errors.New("boom"))
	if plain.Status != 502 || plain.Message != "ctx: boom" {
		t.Fatalf("plain = %#v", plain)
	}
}

func TestIsInvalidCredentialsUsesErrorsAs(t *testing.T) {
	bad := platform.UpstreamError("x", 401, `{"error":"invalid-credentials"}`)
	if !isInvalidCredentials(bad) {
		t.Fatal("direct AppError not recognised")
	}
	if !isInvalidCredentials(fmt.Errorf("wrapped: %w", bad)) {
		t.Fatal("wrapped AppError not recognised (bare type assertion regression)")
	}
	if isInvalidCredentials(platform.UpstreamError("x", 429, `invalid-credentials`)) {
		t.Fatal("429 must not count as invalid credentials")
	}
	if isInvalidCredentials(errors.New("plain")) || isInvalidCredentials(nil) {
		t.Fatal("non-AppError classified")
	}
}

func TestBirthDateLockedDetectionSurvivesWrapping(t *testing.T) {
	locked := platform.UpstreamError("Upstream returned 429", 429, `{"error":"birth-date-change-limit-reached"}`)
	if !isBirthDateLocked(locked) || !isBirthDateLocked(fmt.Errorf("w: %w", locked)) {
		t.Fatal("locked birth date not detected")
	}
	if isBirthDateLocked(platform.UpstreamError("x", 429, "other")) || isBirthDateLocked(platform.UpstreamError("x", 500, birthDateLocked)) {
		t.Fatal("false positive")
	}
}

// --- statsigSource ---------------------------------------------------------

func TestStatsigSourcePrecedence(t *testing.T) {
	s := newStatsigSource()
	if got := s.id(proxyProfile{StatsigID: "manual"}, "/p", "POST", nil); got != "manual" {
		t.Fatalf("manual override = %q", got)
	}
	// A configured pair is authoritative and the HTML refresh does not run.
	called := atomic.Bool{}
	fetch := func(context.Context, proxyProfile) (string, string, error) {
		called.Store(true)
		return "", "", nil
	}
	seed := "t2ODAFY4ozXd0K2Y8MdI2XfxTDiJoakZPuoaKfcQn8VuasZMcKliyhA1pJ+o1oMf"
	hex := "3bab9506b851eb851eb840e8f5c28f5c28f80e8f5c28f5c28f806b851eb851eb8400"
	p := proxyProfile{StatsigSeed: seed, StatsigHEX: hex, StatsigFromHTML: true}
	v := s.id(p, "/rest/x", "POST", fetch)
	s.wait()
	raw, err := base64.RawStdEncoding.DecodeString(v)
	if err != nil || len(raw) != 70 {
		t.Fatalf("id = %q (%d bytes, %v)", v, len(raw), err)
	}
	if called.Load() {
		t.Fatal("HTML refresh ran although a config pair is set")
	}
	if s.cfgSeed != seed || s.cfgHEX != hex {
		t.Fatal("config pair not recorded")
	}
	// An invalid config pair is ignored, the previous pair keeps working.
	bad := proxyProfile{StatsigSeed: "not-a-seed", StatsigHEX: "abc"}
	if v := s.id(bad, "/rest/x", "POST", nil); len(v) == 0 {
		t.Fatal("no id with invalid config pair")
	}
}

func TestStatsigSourceRefreshNeverBlocksTheRequest(t *testing.T) {
	s := newStatsigSource()
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	fetch := func(ctx context.Context, _ proxyProfile) (string, string, error) {
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return "", "", nil
	}
	p := proxyProfile{StatsigFromHTML: true}

	done := make(chan string, 1)
	go func() { done <- s.id(p, "/rest/x", "POST", fetch) }()
	select {
	case v := <-done:
		if v == "" {
			t.Fatal("empty id")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("id() blocked on the HTML refresh")
	}
	<-started

	// While one refresh is running (and for the interval after), no second
	// fetch starts and the mutex is free for concurrent callers.
	for i := 0; i < 5; i++ {
		s.id(p, "/rest/x", "POST", fetch)
	}
	select {
	case <-started:
		t.Fatal("a second refresh started")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	s.wait()
}

func TestStatsigSourceAppliesRefreshedPair(t *testing.T) {
	s := newStatsigSource()
	seed := "t2ODAFY4ozXd0K2Y8MdI2XfxTDiJoakZPuoaKfcQn8VuasZMcKliyhA1pJ+o1oMf"
	fetch := func(context.Context, proxyProfile) (string, string, error) { return seed, "deadbeef", nil }
	s.id(proxyProfile{StatsigFromHTML: true}, "/p", "POST", fetch)
	s.wait()
	_, hex := s.gen.Pair()
	if hex != "deadbeef" || s.htmlSeed != seed {
		t.Fatalf("refreshed pair not applied: hex=%q", hex)
	}
	// A failing refresh keeps the last pair.
	s.lastAttempt = time.Time{}
	failing := func(context.Context, proxyProfile) (string, string, error) { return "", "", errors.New("down") }
	s.id(proxyProfile{StatsigFromHTML: true}, "/p", "POST", failing)
	s.wait()
	if _, hex := s.gen.Pair(); hex != "deadbeef" {
		t.Fatalf("failed refresh clobbered the pair: %q", hex)
	}
}

// --- SSE -------------------------------------------------------------------

func TestReadSSEChatDialect(t *testing.T) {
	body := strings.Join([]string{
		`event: ignored`,
		`data: {"a":1}`,
		``,
		`{"bare":true}`,
		`: comment`,
		`data: {"b":2}`,
		`data: [DONE]`,
		`data: {"after":"done"}`,
	}, "\n")
	var got []string
	err := ReadSSE(context.Background(), strings.NewReader(body), SSEChat, func(m SSEMessage) error {
		got = append(got, m.Data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`{"a":1}`, `{"bare":true}`, `{"b":2}`}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestReadSSEConsoleDialectCarriesEventNames(t *testing.T) {
	body := "event: response.output_text.delta\ndata: {\"delta\":\"a\"}\n\nevent: response.completed\ndata: {\"x\":1}\n\n{\"bare\":true}\n"
	var got [][2]string
	err := ReadSSE(context.Background(), strings.NewReader(body), SSEConsole, func(m SSEMessage) error {
		got = append(got, [2]string{m.Event, m.Data})
		return nil
	})
	if err != nil || len(got) != 2 {
		t.Fatalf("got %v err %v", got, err)
	}
	if got[0] != [2]string{"response.output_text.delta", `{"delta":"a"}`} || got[1][0] != "response.completed" {
		t.Fatalf("got %v", got)
	}
}

func TestReadSSEStopsAndPropagates(t *testing.T) {
	body := "data: {\"n\":1}\ndata: {\"n\":2}\ndata: {\"n\":3}\n"
	n := 0
	err := ReadSSE(context.Background(), strings.NewReader(body), SSEChat, func(SSEMessage) error {
		n++
		if n == 2 {
			return ErrStopSSE
		}
		return nil
	})
	if err != nil || n != 2 {
		t.Fatalf("stop: n=%d err=%v", n, err)
	}
	boom := errors.New("boom")
	err = ReadSSE(context.Background(), strings.NewReader(body), SSEChat, func(SSEMessage) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestReadSSEHandlesLongLinesAndCancel(t *testing.T) {
	long := strings.Repeat("x", 200*1024)
	body := `data: {"blob":"` + long + `"}` + "\n"
	var size int
	if err := ReadSSE(context.Background(), strings.NewReader(body), SSEChat, func(m SSEMessage) error {
		size = len(m.Data)
		return nil
	}); err != nil || size < len(long) {
		t.Fatalf("size=%d err=%v", size, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := ReadSSE(ctx, strings.NewReader("data: {}\n"), SSEChat, func(SSEMessage) error {
		t.Fatal("callback ran after cancel")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestReadSSEReportsBrokenBody(t *testing.T) {
	r := io.MultiReader(strings.NewReader("data: {\"a\":1}\n"), errReader{})
	var n int
	err := ReadSSE(context.Background(), r, SSEChat, func(SSEMessage) error { n++; return nil })
	if err == nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

// --- gateway session state machine ----------------------------------------

func TestGatewaySessionStateMachine(t *testing.T) {
	s := newGatewaySession("evt_init", "hello", []string{"f1"})
	step := func(raw string) gatewayStep {
		var env map[string]any
		if err := json.Unmarshal([]byte(raw), &env); err != nil {
			t.Fatal(err)
		}
		return s.handle(env)
	}

	// A session.created for somebody else's event does not count.
	if st := step(`{"session_id":"S","event":{"type":"session.created","client_event_id":"other"}}`); st.send != nil {
		t.Fatal("turn sent for a foreign session.created")
	}
	if st := step(`{"event":{"type":"conversation.attached","conversation":{"id":"C"}}}`); st.send != nil {
		t.Fatal("turn sent before session.created matched")
	}
	st := step(`{"event":{"type":"session.created","client_event_id":"evt_init"}}`)
	if st.send == nil {
		t.Fatal("turn not sent once created and attached")
	}
	if st.send["session_id"] != "S" {
		t.Fatalf("turn session_id = %v", st.send["session_id"])
	}
	// The turn is sent exactly once.
	if st := step(`{"event":{"type":"conversation.attached","conversation":{"id":"C"}}}`); st.send != nil {
		t.Fatal("turn sent twice")
	}

	st = step(`{"event":{"type":"response.chunk","chunk":{"text":{"channel":"CHANNEL_REASONING","text":"hm"}}}}`)
	if len(st.events) != 1 || st.events[0].Kind != EventThinking || st.events[0].Content != "hm" || st.events[0].ConversationID != "C" {
		t.Fatalf("chunk step = %#v", st)
	}
	st = step(`{"event":{"type":"response.done","response":{"id":"R","status":"completed"}}}`)
	if !st.done || len(st.events) != 1 || st.events[0].Kind != EventSoftStop || st.events[0].ResponseID != "R" || st.events[0].ConversationID != "C" {
		t.Fatalf("done step = %#v", st)
	}
}

func TestGatewaySessionErrorsEndTheTurn(t *testing.T) {
	for name, raw := range map[string]string{
		"error event":   `{"event":{"type":"error","error":{"code":"rate_limit","message":"slow"}}}`,
		"session ended": `{"event":{"type":"session.ended"}}`,
		"bad status":    `{"event":{"type":"response.done","response":{"status":"incomplete","status_details":{"reason":"max_tokens"}}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			var env map[string]any
			_ = json.Unmarshal([]byte(raw), &env)
			st := newGatewaySession("i", "p", nil).handle(env)
			if appErr, ok := asAppError(st.err); !ok || appErr.Status == 0 {
				t.Fatalf("step = %#v", st)
			}
			if st.done {
				t.Fatal("failed turn reported done")
			}
		})
	}
}

func TestGatewaySessionHonoursFeatureSwitches(t *testing.T) {
	temp := gatewaySessionSettings(chatFeatures{Temporary: true, Memory: false})
	if temp["is_temporary"] != true || temp["disable_memory"] != true {
		t.Fatalf("defaults: %#v", temp)
	}
	kept := gatewaySessionSettings(chatFeatures{Temporary: false, Memory: true})
	if kept["is_temporary"] != false || kept["disable_memory"] != false {
		t.Fatalf("configured: %#v", kept)
	}
	useConfig(t, "[features]\ntemporary = false\nmemory = true\n")
	f := chatFeaturesFromConfig()
	if f.Temporary || !f.Memory {
		t.Fatalf("features = %#v", f)
	}
	useConfig(t, "")
	if f := chatFeaturesFromConfig(); !f.Temporary || f.Memory {
		t.Fatalf("default features = %#v", f)
	}
}

// --- imagine round ---------------------------------------------------------

func TestImagineRoundFlushIsOrderedAndCounted(t *testing.T) {
	ids := map[string]int{
		"cccc0000-0000-0000-0000-000000000000": 2,
		"aaaa0000-0000-0000-0000-000000000000": 0,
		"bbbb0000-0000-0000-0000-000000000000": 1,
	}
	// Go randomises map iteration, so repeating the build exercises many
	// slot-creation orders: the output order must not depend on them.
	for i := 0; i < 30; i++ {
		r := newImagineRound(5)
		for id, order := range ids {
			r.handleFrame(map[string]any{"type": "json", "current_status": "start_stage", "image_id": id, "order": float64(order)})
		}
		for id := range ids {
			r.handleFrame(map[string]any{"type": "image", "url": "/images/" + id + ".png", "blob": "blob-" + id[:1]})
		}
		events := r.flush()
		if len(events) != 3 || events[0].Order != 0 || events[1].Order != 1 || events[2].Order != 2 {
			t.Fatalf("flush order = %#v", events)
		}
		if events[0].Blob != "blob-a" || !events[0].IsFinal || r.collected != 3 {
			t.Fatalf("events[0] = %#v collected=%d", events[0], r.collected)
		}
		// Flushing twice never re-emits.
		if again := r.flush(); len(again) != 0 {
			t.Fatalf("second flush emitted %d", len(again))
		}
	}
}

func TestImagineRoundCompletionAndModeration(t *testing.T) {
	r := newImagineRound(2)
	const a, b = "aaaa0000-0000-0000-0000-000000000000", "bbbb0000-0000-0000-0000-000000000000"
	r.handleFrame(map[string]any{"type": "json", "current_status": "start_stage", "image_id": a})
	r.handleFrame(map[string]any{"type": "json", "current_status": "start_stage", "image_id": b, "order": 1.0})

	ev, outcome := r.handleFrame(map[string]any{"type": "json", "current_status": "completed", "image_id": a, "moderated": true})
	if outcome != frameContinue || len(ev) != 1 || ev[0].Type != ImagineEventModerated || r.moderated != 1 || r.collected != 0 {
		t.Fatalf("moderated: %#v %v", ev, outcome)
	}
	// A duplicate completion is ignored.
	if ev, _ := r.handleFrame(map[string]any{"type": "json", "current_status": "completed", "image_id": a}); len(ev) != 0 {
		t.Fatalf("duplicate completion emitted %#v", ev)
	}
	ev, outcome = r.handleFrame(map[string]any{"type": "json", "current_status": "completed", "image_id": b})
	if outcome != frameRoundComplete || len(ev) != 1 || ev[0].Type != ImagineEventImage || !ev[0].IsFinal || r.collected != 1 {
		t.Fatalf("completed: %#v %v", ev, outcome)
	}
}

func TestImagineRoundProgressIsMonotonicAndClamped(t *testing.T) {
	r := newImagineRound(1)
	const id = "aaaa0000-0000-0000-0000-000000000000"
	r.handleFrame(map[string]any{"type": "json", "current_status": "start_stage", "image_id": id})
	img := func(pct float64) []ImagineEvent {
		return r.onImage(map[string]any{"url": "/images/" + id + ".png", "blob": "x", "percentage_complete": pct})
	}
	if ev := img(5); len(ev) != 0 {
		t.Fatalf("progress below the start emitted %#v", ev) // clamped to 10 == current
	}
	if ev := img(60); len(ev) != 1 || ev[0].Progress != 60 {
		t.Fatalf("60%% = %#v", ev)
	}
	if ev := img(40); len(ev) != 0 {
		t.Fatalf("regressing progress emitted %#v", ev)
	}
	if ev := img(250); len(ev) != 1 || ev[0].Progress != 99 {
		t.Fatalf("clamp = %#v", ev)
	}
	// A frame whose URL has no image id is dropped instead of guessed at.
	if ev := r.onImage(map[string]any{"url": "/not-an-image", "blob": "x"}); len(ev) != 0 {
		t.Fatalf("id-less frame emitted %#v", ev)
	}
}

func TestParseImageURLNoMatchReturnsNotOK(t *testing.T) {
	if id, ext, ok := parseImageURL("https://x/y/z.png"); ok || id != "" || ext != "" {
		t.Fatalf("id=%q ext=%q ok=%v", id, ext, ok)
	}
}

// --- conversation tracker --------------------------------------------------

func TestConversationTrackerGetReturnsCopy(t *testing.T) {
	ct := NewConversationTracker(time.Minute)
	defer ct.Close()
	ct.Set("tok", "c", "r")
	got := ct.Get("tok")
	got.ConversationID = "mutated"
	got.LastResponseID = "mutated"
	if again := ct.Get("tok"); again.ConversationID != "c" || again.LastResponseID != "r" {
		t.Fatalf("caller mutation leaked into the tracker: %#v", again)
	}
}

// The TTL runs from creation: reading an entry must not extend it, or a busy
// account would carry one caller's conversation into every later request.
func TestConversationTrackerTTLIsMeasuredFromCreation(t *testing.T) {
	ct := NewConversationTracker(300 * time.Millisecond)
	defer ct.Close()
	ct.Set("tok", "c", "r")
	for i := 0; i < 2; i++ {
		time.Sleep(80 * time.Millisecond)
		if ct.Get("tok") == nil {
			t.Fatalf("entry expired early after %d reads", i)
		}
	}
	time.Sleep(250 * time.Millisecond) // > ttl since creation although just read
	if ct.Get("tok") != nil {
		t.Fatal("reads extended the entry's life")
	}
}

func TestConversationTrackerCloseStopsSweeper(t *testing.T) {
	var wg sync.WaitGroup
	trackers := make([]*ConversationTracker, 20)
	for i := range trackers {
		trackers[i] = NewConversationTracker(time.Hour)
	}
	for _, ct := range trackers {
		ct := ct
		wg.Add(1)
		go func() { defer wg.Done(); ct.Close(); ct.Close() }() // idempotent
	}
	wg.Wait()
	// The sweeper goroutines must have observed the stop signal.
	deadline := time.After(2 * time.Second)
	for _, ct := range trackers {
		select {
		case <-ct.stop:
		case <-deadline:
			t.Fatal("stop channel not closed")
		}
	}
	// A closed tracker keeps working for reads and writes.
	trackers[0].Set("t", "c", "r")
	if trackers[0].Get("t") == nil {
		t.Fatal("closed tracker lost data")
	}
}

// --- grpc-web --------------------------------------------------------------

func TestGRPCMessageIsPercentDecodedExactlyOnce(t *testing.T) {
	// "%2541" is the encoding of the literal text "%41". Decoding twice would
	// turn it into "A".
	for name, tc := range map[string]struct {
		body    []byte
		headers map[string]string
	}{
		"trailer frame": {body: trailerFrame("grpc-status: 3\r\ngrpc-message: %2541\r\n")},
		"http header":   {headers: map[string]string{"Grpc-Status": "3", "Grpc-Message": "%2541"}},
	} {
		t.Run(name, func(t *testing.T) {
			h := map[string][]string{}
			for k, v := range tc.headers {
				h[k] = []string{v}
			}
			_, trailers := parseGRPCWebResponse(tc.body, "application/grpc-web+proto", h)
			if trailers["grpc-message"] != "%41" {
				t.Fatalf("grpc-message = %q, want %q", trailers["grpc-message"], "%41")
			}
		})
	}
}

// --- console ---------------------------------------------------------------

func TestConsoleAdapterErrorEventLatches(t *testing.T) {
	a := NewConsoleStreamAdapter()
	if _, err := a.Feed("error", `{"message":"boom"}`); err == nil {
		t.Fatal("no error reported")
	}
	if tok, err := a.Feed("response.output_text.delta", `{"delta":"late"}`); tok != nil || err != nil {
		t.Fatalf("adapter kept emitting after an error event: %v %v", tok, err)
	}
	if a.FullText() != "" {
		t.Fatalf("FullText = %q", a.FullText())
	}
}

func TestConsoleModelTablesAreConsistent(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range consoleModelSpecs {
		if seen[s.Public] {
			t.Errorf("duplicate public model %q", s.Public)
		}
		seen[s.Public] = true
		if s.Upstream == "" {
			t.Errorf("%q has no upstream", s.Public)
		}
		if s.Effort != "" {
			if _, ok := consoleUpstreams[s.Upstream]; !ok || !consoleUpstreams[s.Upstream].Reasoning {
				t.Errorf("%q pins an effort but %q takes no reasoning block", s.Public, s.Upstream)
			}
		}
	}
	for up := range consoleUpstreams {
		found := false
		for _, s := range consoleModelSpecs {
			if s.Upstream == up {
				found = true
			}
		}
		if !found {
			t.Errorf("upstream traits for %q have no public model", up)
		}
	}
	if consoleEffort("grok-4.5-xhigh", "low") != "xhigh" || consoleEffort("grok-4.5-console", "") != "medium" {
		t.Fatal("consoleEffort")
	}
}

// --- assets ----------------------------------------------------------------

func TestAssetFetchGuard(t *testing.T) {
	ctx := context.Background()
	bad := []string{
		"ftp://example.com/a.png",
		"file:///etc/passwd",
		"http://127.0.0.1/a.png",
		"http://127.0.0.1:8080/a.png",
		"http://[::1]/a.png",
		"http://localhost/a.png",
		"http://app.localhost/a.png",
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.5/a.png",
		"http://192.168.1.1/a.png",
		"http://172.16.0.1/a.png",
		"http://100.64.0.1/a.png",
		"http://0.0.0.0/a.png",
		"http://[::ffff:127.0.0.1]/a.png",
		"http://printer.local/a.png",
		"http://svc.internal/a.png",
		"https://user:pw@example.com/a.png",
		"https:///nohost",
	}
	for _, raw := range bad {
		if err := defaultAssetFetchGuard(ctx, raw); err == nil {
			t.Errorf("guard accepted %q", raw)
		}
	}
	for _, raw := range []string{"https://8.8.8.8/a.png", "http://[2606:4700::1111]/a.png"} {
		if err := defaultAssetFetchGuard(ctx, raw); err != nil {
			t.Errorf("guard rejected public address %q: %v", raw, err)
		}
	}
}

func TestUploadFromInputRefusesPrivateTargetsBeforeFetching(t *testing.T) {
	tr := newTestTransport(t)
	_, err := UploadFromInput(context.Background(), tr, "tok", "http://169.254.169.254/latest/meta-data/")
	appErr := appErrorOf(t, err)
	if appErr.Status != 400 {
		t.Fatalf("err = %#v", appErr)
	}
	if resetPending(tr.pool) || tr.pool.client != nil {
		t.Fatal("a refused URL still reached the transport")
	}
}

func TestAssetIDIsEscapedInDeleteURL(t *testing.T) {
	if got := assetDeleteURL("../x?y=1/z"); got != AssetsDeleteURL+"/..%2Fx%3Fy=1%2Fz" {
		t.Fatalf("assetDeleteURL = %q", got)
	}
	if got := assetDeleteURL("abc-123"); got != AssetsDeleteURL+"/abc-123" {
		t.Fatalf("assetDeleteURL = %q", got)
	}
}
