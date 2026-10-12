package grok

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/gentpan/grok2api/internal/platform"
)

func TestGatewayStreamErrorStatus(t *testing.T) {
	cases := []struct {
		name  string
		event string
		want  int
	}{
		{"generic", `{"type":"response.grok.output","output":{"stream_error":{"message":"internal failure"}}}`, 502},
		{"numeric code 8", `{"type":"response.grok.output","output":{"stream_error":{"message":"x","code":8}}}`, 429},
		{"string rate code", `{"type":"response.grok.output","output":{"stream_error":{"message":"x","code":"rate_limited"}}}`, 429},
		{"message", `{"type":"response.grok.output","output":{"stream_error":{"message":"Too many requests, slow down"}}}`, 429},
		{"empty message", `{"type":"response.grok.output","output":{"stream_error":{}}}`, 502},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var event map[string]any
			if err := json.Unmarshal([]byte(tc.event), &event); err != nil {
				t.Fatal(err)
			}
			err := parseGatewayEvent(event, &gatewayEventResult{})
			var appErr *platform.AppError
			if !errors.As(err, &appErr) || appErr.Status != tc.want {
				t.Fatalf("err = %v (status %v), want status %d", err, appErr, tc.want)
			}
		})
	}
}

func TestSendGatewayEventDoesNotBlockAfterCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan GatewayEvent, 1)
	if !sendGatewayEvent(ctx, out, GatewayEvent{}) {
		t.Fatal("send into free buffer failed")
	}
	cancel()
	done := make(chan bool, 1)
	go func() { done <- sendGatewayEvent(ctx, out, GatewayEvent{}) }() // buffer is full, nobody reads
	select {
	case ok := <-done:
		if ok {
			t.Fatal("send reported success on a full channel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("send blocked after ctx cancel")
	}
}

func TestConsoleStreamAdapterFailureEvents(t *testing.T) {
	cases := []struct {
		name, event, data string
		wantStatus        int
	}{
		{"failed", "response.failed", `{"response":{"status":"failed","error":{"code":"server_error","message":"boom"}}}`, 502},
		{"failed rate limit", "response.failed", `{"response":{"error":{"code":"rate_limit_exceeded","message":"slow down"}}}`, 429},
		{"incomplete", "response.incomplete", `{"response":{"incomplete_details":{"reason":"max_output_tokens"}}}`, 502},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := NewConsoleStreamAdapter()
			if _, err := a.Feed("response.output_text.delta", `{"delta":"hi"}`); err != nil {
				t.Fatal(err)
			}
			_, err := a.Feed(tc.event, tc.data)
			if err == nil || err.Status != tc.wantStatus {
				t.Fatalf("err = %v, want status %d", err, tc.wantStatus)
			}
			if tokens, again := a.Feed("response.output_text.delta", `{"delta":"late"}`); tokens != nil || again != nil {
				t.Fatal("adapter kept emitting after a terminal event")
			}
		})
	}
}

func TestXSearchTitleTruncatesOnRuneBoundary(t *testing.T) {
	a := NewStreamAdapter()
	text := ""
	for i := 0; i < 60; i++ {
		text += "中"
	}
	frame, _ := json.Marshal(map[string]any{"result": map[string]any{"response": map[string]any{
		"xSearchResults": map[string]any{"results": []any{
			map[string]any{"postId": "1", "username": "u", "text": text},
		}},
	}}})
	if _, err := a.Feed(frame); err != nil {
		t.Fatal(err)
	}
	src := a.SearchSources()
	if len(src) != 1 {
		t.Fatalf("sources = %#v", src)
	}
	title := src[0]["title"].(string)
	want := "𝕏/@u: "
	for i := 0; i < 50; i++ {
		want += "中"
	}
	want += "..."
	if title != want {
		t.Fatalf("title = %q", title)
	}
}
