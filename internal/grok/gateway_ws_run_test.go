package grok

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/websocket"

	"github.com/gentpan/grok2api/internal/platform"
)

const gatewayTestUserID = "11111111-2222-3333-4444-555555555555"

// gatewayTestServer is a scripted stand-in for grok.com's /ws/mgw/ Gateway
// plus the /api/auth/session user lookup.
type gatewayTestServer struct {
	baseURL string

	mu       sync.Mutex
	origin   string
	cookie   string
	uid      string
	received []map[string]any
}

type gatewayScript func(conn *websocket.Conn, srv *gatewayTestServer)

// startGatewayServer serves the Gateway on a local plain-http listener.
// reject, when non-zero, answers the upgrade with that HTTP status instead.
func startGatewayServer(t *testing.T, reject int, script gatewayScript) *gatewayTestServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &gatewayTestServer{baseURL: "http://" + ln.Addr().String()}
	up := websocket.Upgrader{CheckOrigin: func(*fhttp.Request) bool { return true }}
	mux := fhttp.NewServeMux()
	mux.HandleFunc("/api/auth/session", func(w fhttp.ResponseWriter, r *fhttp.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user":{"id":"` + gatewayTestUserID + `"}}`))
	})
	mux.HandleFunc("/ws/mgw/", func(w fhttp.ResponseWriter, r *fhttp.Request) {
		srv.mu.Lock()
		srv.origin = r.Header.Get("Origin")
		srv.cookie = r.Header.Get("Cookie")
		srv.uid = r.URL.Query().Get("uid")
		srv.mu.Unlock()
		if reject != 0 {
			w.WriteHeader(reject)
			_, _ = w.Write([]byte("blocked by test sso=leaky-token"))
			return
		}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		script(conn, srv)
	})
	hs := &fhttp.Server{Handler: mux}
	go func() { _ = hs.Serve(ln) }()
	t.Cleanup(func() { _ = hs.Close() })
	return srv
}

func (s *gatewayTestServer) read(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	var msg map[string]any
	if err := conn.ReadJSON(&msg); err != nil {
		t.Errorf("server read: %v", err)
		return nil
	}
	s.mu.Lock()
	s.received = append(s.received, msg)
	s.mu.Unlock()
	return msg
}

func eventOf(msg map[string]any) map[string]any {
	ev, _ := msg["event"].(map[string]any)
	return ev
}

func gatewayHandshake(t *testing.T, srv *gatewayTestServer, conn *websocket.Conn) {
	t.Helper()
	create := eventOf(srv.read(t, conn))
	if create["type"] != "session.create" {
		t.Errorf("first client event = %#v", create)
	}
	_ = conn.WriteJSON(map[string]any{"session_id": "sess-1", "event": map[string]any{
		"type": "session.created", "client_event_id": create["event_id"],
	}})
	_ = conn.WriteJSON(map[string]any{"session_id": "sess-1", "event": map[string]any{
		"type": "conversation.attached", "conversation": map[string]any{"id": "conv-1"},
	}})
}

func collectGateway(t *testing.T, ch <-chan GatewayEvent) []GatewayEvent {
	t.Helper()
	var out []GatewayEvent
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			t.Fatalf("gateway stream did not finish; events so far: %#v", out)
		}
	}
}

func newGatewayTestTransport(t *testing.T, srv *gatewayTestServer) *Transport {
	t.Helper()
	useConfig(t, "")
	allowLocalCookieHosts(t)
	tr := newTestTransport(t)
	tr.baseOverride = srv.baseURL
	return tr
}

func TestGatewayChatRunLoopHappyPath(t *testing.T) {
	srv := startGatewayServer(t, 0, func(conn *websocket.Conn, srv *gatewayTestServer) {
		gatewayHandshake(t, srv, conn)
		turn := eventOf(srv.read(t, conn))
		if turn["type"] != "response.create" {
			t.Errorf("second client event = %#v", turn)
		}
		for _, ev := range []map[string]any{
			{"type": "response.chunk", "chunk": map[string]any{"text": map[string]any{"channel": "CHANNEL_REASONING", "text": "hmm"}}},
			{"type": "response.chunk", "chunk": map[string]any{"text": map[string]any{"channel": "CHANNEL_ASSISTANT_RESPONSE", "text": "Hel"}}},
			{"type": "response.output_text.delta", "delta": "lo"},
			{"type": "response.done", "response": map[string]any{"id": "resp-1", "status": "completed"}},
		} {
			_ = conn.WriteJSON(map[string]any{"session_id": "sess-1", "event": ev})
		}
	})
	tr := newGatewayTestTransport(t, srv)

	events := collectGateway(t, tr.StreamGatewayChat(context.Background(), "tok", "grok-test", "say hi", []string{"file-1"}))
	for _, ev := range events {
		if ev.Err != nil {
			t.Fatalf("unexpected error event: %v", ev.Err)
		}
	}
	var kinds []FrameEventKind
	var text, reasoning string
	for _, ev := range events {
		kinds = append(kinds, ev.Kind)
		switch ev.Kind {
		case EventText:
			text += ev.Content
		case EventThinking:
			reasoning += ev.Content
		}
	}
	if text != "Hello" || reasoning != "hmm" {
		t.Fatalf("text=%q reasoning=%q events=%#v", text, reasoning, events)
	}
	last := events[len(events)-1]
	if last.Kind != EventSoftStop || last.ConversationID != "conv-1" || last.ResponseID != "resp-1" {
		t.Fatalf("last event = %#v (kinds %v)", last, kinds)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.uid != gatewayTestUserID {
		t.Errorf("uid query = %q", srv.uid)
	}
	if srv.origin != srv.baseURL {
		t.Errorf("Origin = %q, want %q", srv.origin, srv.baseURL)
	}
	if !strings.Contains(srv.cookie, "x-userid="+gatewayTestUserID) || !strings.Contains(srv.cookie, "sso=tok") {
		t.Errorf("Cookie = %q", srv.cookie)
	}
	// session.create carries the model and the session settings.
	create := eventOf(srv.received[0])
	session, _ := create["session"].(map[string]any)
	if session["model"] != "grok-test" {
		t.Errorf("session.model = %#v", session["model"])
	}
	// response.create targets the session and carries the prompt + attachment.
	turn := srv.received[1]
	if turn["session_id"] != "sess-1" {
		t.Errorf("turn session_id = %#v", turn["session_id"])
	}
	raw, _ := json.Marshal(turn)
	for _, want := range []string{`"say hi"`, `"file-1"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("turn lacks %s: %s", want, raw)
		}
	}
}

func TestGatewayChatErrorEventBecomesAppError(t *testing.T) {
	srv := startGatewayServer(t, 0, func(conn *websocket.Conn, srv *gatewayTestServer) {
		gatewayHandshake(t, srv, conn)
		srv.read(t, conn)
		_ = conn.WriteJSON(map[string]any{"event": map[string]any{
			"type": "error", "error": map[string]any{"code": "rate_limit_exceeded", "message": "slow down"},
		}})
	})
	tr := newGatewayTestTransport(t, srv)

	events := collectGateway(t, tr.StreamGatewayChat(context.Background(), "tok", "m", "p", nil))
	if len(events) != 1 || events[0].Err == nil {
		t.Fatalf("events = %#v", events)
	}
	var appErr *platform.AppError
	if !errors.As(events[0].Err, &appErr) || appErr.Status != 429 {
		t.Fatalf("err = %v, want AppError 429", events[0].Err)
	}
}

func TestGatewayChatSessionEndedBeforeDone(t *testing.T) {
	srv := startGatewayServer(t, 0, func(conn *websocket.Conn, srv *gatewayTestServer) {
		gatewayHandshake(t, srv, conn)
		srv.read(t, conn)
		_ = conn.WriteJSON(map[string]any{"event": map[string]any{"type": "session.ended"}})
	})
	tr := newGatewayTestTransport(t, srv)

	events := collectGateway(t, tr.StreamGatewayChat(context.Background(), "tok", "m", "p", nil))
	if len(events) != 1 || events[0].Err == nil || !strings.Contains(events[0].Err.Error(), "session ended") {
		t.Fatalf("events = %#v", events)
	}
}

func TestGatewayChatHandshakeRejectionKeepsStatusAndRedactsMessage(t *testing.T) {
	srv := startGatewayServer(t, 403, nil)
	tr := newGatewayTestTransport(t, srv)

	events := collectGateway(t, tr.StreamGatewayChat(context.Background(), "tok", "m", "p", nil))
	if len(events) != 1 || events[0].Err == nil {
		t.Fatalf("events = %#v", events)
	}
	var appErr *platform.AppError
	if !errors.As(events[0].Err, &appErr) || appErr.Status != 403 {
		t.Fatalf("err = %v, want AppError 403", events[0].Err)
	}
	if !strings.Contains(appErr.Message, "HTTP 403") {
		t.Errorf("message lacks status: %q", appErr.Message)
	}
	if strings.Contains(appErr.Message, "leaky-token") || strings.Contains(appErr.Body, "leaky-token") {
		t.Errorf("error leaks secret: message=%q body=%q", appErr.Message, appErr.Body)
	}
	if !strings.Contains(appErr.Body, "blocked by test") {
		t.Errorf("redacted body lost its content: %q", appErr.Body)
	}
	// A rejected handshake feeds the same reset policy as an HTTP 403.
	if !resetPending(tr.pool) {
		t.Error("handshake 403 did not flag the client for reset")
	}
}

func TestGatewayChatContextCancelStops(t *testing.T) {
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	srv := startGatewayServer(t, 0, func(conn *websocket.Conn, srv *gatewayTestServer) {
		gatewayHandshake(t, srv, conn)
		srv.read(t, conn)
		<-hold // never answers
	})
	tr := newGatewayTestTransport(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	ch := tr.StreamGatewayChat(ctx, "tok", "m", "p", nil)
	time.AfterFunc(300*time.Millisecond, cancel)
	collectGateway(t, ch) // must terminate
}

func TestGatewayEndpoint(t *testing.T) {
	ep, origin, err := gatewayEndpoint("https://grok.com", "u 1")
	if err != nil || ep != "wss://grok.com/ws/mgw/?uid=u+1" || origin != "https://grok.com" {
		t.Fatalf("ep=%q origin=%q err=%v", ep, origin, err)
	}
	ep, _, err = gatewayEndpoint("http://127.0.0.1:9", "x")
	if err != nil || !strings.HasPrefix(ep, "ws://127.0.0.1:9/ws/mgw/") {
		t.Fatalf("ep=%q err=%v", ep, err)
	}
	for _, bad := range []string{"", "ftp://x", "://"} {
		if _, _, err := gatewayEndpoint(bad, "x"); err == nil {
			t.Errorf("gatewayEndpoint(%q) accepted", bad)
		}
	}
}
