package grok

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bogdanfinn/websocket"
	"github.com/google/uuid"

	"github.com/gentpan/grok2api/internal/config"
	"github.com/gentpan/grok2api/internal/platform"
)

const (
	gatewayHandshakeTimeout = 20 * time.Second
	gatewayHeartbeatPeriod  = 25 * time.Second
	gatewayWriteTimeout     = 15 * time.Second
	gatewayMaxFrameBytes    = 16 << 20
	gatewayEventBuffer      = 32
)

// chatFeaturesFromConfig reads the chat switches shared by the REST payload
// and the Gateway session.
func chatFeaturesFromConfig() chatFeatures {
	cfg := config.Global()
	return chatFeatures{
		Temporary:         cfg.GetBool("features.temporary", true),
		Memory:            cfg.GetBool("features.memory", false),
		CustomInstruction: strings.TrimSpace(cfg.GetStr("features.custom_instruction", "")),
	}
}

// StreamGatewayChat connects to the current grok.com text-chat Gateway and
// streams one turn. The channel is closed when the turn ends; a failure is
// delivered as a final event with Err set.
func (t *Transport) StreamGatewayChat(ctx context.Context, token, modelName, prompt string, attachments []string) <-chan GatewayEvent {
	out := make(chan GatewayEvent, gatewayEventBuffer)
	go func() {
		defer close(out)
		if err := t.runGatewayChat(ctx, token, modelName, prompt, attachments, out); err != nil {
			sendGatewayEvent(ctx, out, GatewayEvent{Kind: EventSkip, Err: err})
		}
	}()
	return out
}

// runGatewayChat dials the Gateway and drives a gatewaySession over it.
func (t *Transport) runGatewayChat(ctx context.Context, token, modelName, prompt string, attachments []string, out chan<- GatewayEvent) error {
	profile := resolveProxyProfile()
	userID, err := t.resolveGatewayUserID(ctx, token, profile)
	if err != nil {
		return err
	}
	conn, err := t.dialGateway(ctx, token, userID, profile)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetReadLimit(gatewayMaxFrameBytes)

	var writeMu sync.Mutex
	write := func(value any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(gatewayWriteTimeout))
		return conn.WriteJSON(value)
	}

	sess := newGatewaySession("evt_init_"+uuid.NewString(), prompt, attachments)
	if err := write(buildGatewaySessionCreate(modelName, sess.initialID, chatFeaturesFromConfig())); err != nil {
		return upstreamErr("send Gateway session.create: %v", err)
	}
	stopHeartbeat := startGatewayHeartbeat(ctx, conn, write)
	defer stopHeartbeat()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		messageType, data, err := conn.ReadMessage()
		if err != nil {
			return upstreamErr("read Grok Gateway: %v", err)
		}
		if messageType != websocket.TextMessage || len(data) > gatewayMaxFrameBytes {
			continue
		}
		var envelope map[string]any
		if err := json.Unmarshal(data, &envelope); err != nil {
			continue
		}
		step := sess.handle(envelope)
		if step.err != nil {
			return step.err
		}
		for _, ev := range step.events {
			if !sendGatewayEvent(ctx, out, ev) {
				return ctx.Err()
			}
		}
		if step.send != nil {
			if err := write(step.send); err != nil {
				return upstreamErr("send Gateway response.create: %v", err)
			}
		}
		if step.done {
			return nil
		}
	}
}

// dialGateway opens the WebSocket through the shared client's browser-profiled
// dialers (same proxy and TLS fingerprint as the HTTP traffic). A handshake
// the server rejected keeps its HTTP status and feeds the client reset policy
// like any other upstream response.
func (t *Transport) dialGateway(ctx context.Context, token, userID string, profile proxyProfile) (*websocket.Conn, error) {
	endpoint, origin, err := gatewayEndpoint(t.baseURL(), userID)
	if err != nil {
		return nil, err
	}
	client, err := t.pool.get()
	if err != nil {
		return nil, platform.UpstreamError("transport init failed: "+err.Error(), 502, "")
	}
	dialer := &websocket.Dialer{
		HandshakeTimeout:  gatewayHandshakeTimeout,
		NetDialContext:    client.GetDialer().DialContext,
		NetDialTLSContext: client.GetTLSDialer(),
	}
	conn, handshake, err := dialer.DialContext(ctx, endpoint, profile.gatewayHeaders(origin, userID, token))
	if err == nil {
		return conn, nil
	}

	status, statusText, body := 502, "", ""
	if handshake != nil {
		status, statusText = handshake.StatusCode, handshake.Status
		if handshake.Body != nil {
			data, _ := io.ReadAll(io.LimitReader(handshake.Body, 4096))
			_ = handshake.Body.Close()
			body = strings.TrimSpace(string(data))
		}
		t.pool.markResetIfStatus(status)
	} else if ctx.Err() == nil {
		t.pool.markReset()
	}
	msg := gatewayHandshakeDiagnostic("Grok Gateway connect failed: "+err.Error(), status, statusText, body)
	// Body feeds account-level classification; it gets the same secret
	// redaction as the message.
	return nil, platform.UpstreamError(msg, status, redactSensitive(body))
}

// startGatewayHeartbeat sends an application-level ping every
// gatewayHeartbeatPeriod and closes the connection when ctx ends or a write
// fails (which also unblocks the reader). The returned func stops it.
func startGatewayHeartbeat(ctx context.Context, conn *websocket.Conn, write func(any) error) (stop func()) {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(gatewayHeartbeatPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				_ = conn.Close()
				return
			case now := <-ticker.C:
				if write(buildGatewayPing(now)) != nil {
					_ = conn.Close()
					return
				}
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

// sendGatewayEvent delivers ev to out without blocking forever on a consumer
// that stopped reading: it takes free buffer space first (so a final error is
// still delivered after ctx ended) and otherwise waits only until ctx is done.
func sendGatewayEvent(ctx context.Context, out chan<- GatewayEvent, ev GatewayEvent) bool {
	select {
	case out <- ev:
		return true
	default:
	}
	select {
	case out <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

// resolveGatewayUserID returns the account's user id, from the cookie
// configuration when available and from /api/auth/session otherwise.
func (t *Transport) resolveGatewayUserID(ctx context.Context, token string, profile proxyProfile) (string, error) {
	if userID := profile.userID(); userID != "" {
		if _, err := uuid.Parse(userID); err == nil {
			return userID, nil
		}
	}
	value, err := t.GetJSON(ctx, t.baseURL()+"/api/auth/session", token, WithTimeout(15*time.Second))
	if err != nil {
		return "", prefixError("resolve Grok Gateway user id", err)
	}
	userID := firstMapString(value,
		[]string{"session", "userId"}, []string{"user", "id"}, []string{"user", "userId"},
		[]string{"user", "sub"}, []string{"id"}, []string{"userId"}, []string{"sub"})
	if _, err := uuid.Parse(userID); err != nil {
		return "", upstreamErr("Grok Session returned invalid user id")
	}
	return userID, nil
}

// gatewayEndpoint derives the WebSocket endpoint and the Origin from the
// grok.com base URL.
func gatewayEndpoint(baseURL, userID string) (endpoint, origin string, err error) {
	parsed, perr := url.Parse(strings.TrimSpace(baseURL))
	if perr != nil || parsed.Host == "" {
		return "", "", fmt.Errorf("invalid Grok Gateway base URL")
	}
	origin = (&url.URL{Scheme: parsed.Scheme, Host: parsed.Host}).String()
	switch parsed.Scheme {
	case "https":
		parsed.Scheme = "wss"
	case "http":
		parsed.Scheme = "ws"
	default:
		return "", "", fmt.Errorf("invalid Grok Gateway base URL scheme")
	}
	parsed.Path = "/ws/mgw/"
	parsed.RawPath = ""
	parsed.RawQuery = url.Values{"uid": []string{userID}}.Encode()
	parsed.Fragment = ""
	return parsed.String(), origin, nil
}
