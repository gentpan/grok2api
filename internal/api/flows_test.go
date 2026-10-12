package api

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gentpan/grok2api/internal/grok"
)

// Characterization of the success paths of every chat-shaped endpoint.

func TestResponsesNonStreamSuccess(t *testing.T) {
	e := newTestEnv(t, "", map[string][]string{"basic": {tokA}})
	e.up.postStream = scripted(scriptStep{frames: []string{webTextFrame("Hello"), webTextFrame(" world")}})

	w := e.post("/v1/responses", `{"model":"grok-4.20-fast","input":"hi","stream":false}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	m := jsonBody(t, w)
	if m["object"] != "response" || m["status"] != "completed" {
		t.Fatalf("envelope = %v", m)
	}
	out := m["output"].([]any)[0].(map[string]any)
	part := out["content"].([]any)[0].(map[string]any)
	if part["type"] != "output_text" || part["text"] != "Hello world" {
		t.Fatalf("output = %v", out)
	}
}

func TestAnthropicNonStreamSuccess(t *testing.T) {
	e := newTestEnv(t, "", map[string][]string{"basic": {tokA}})
	e.up.postStream = scripted(scriptStep{frames: []string{webTextFrame("Hello"), webTextFrame(" world")}})

	w := e.post("/v1/messages", `{"model":"grok-4.20-fast","max_tokens":64,"stream":false,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	m := jsonBody(t, w)
	if m["type"] != "message" || m["role"] != "assistant" || m["stop_reason"] != "end_turn" {
		t.Fatalf("envelope = %v", m)
	}
	block := m["content"].([]any)[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "Hello world" {
		t.Fatalf("content = %v", m["content"])
	}
}

func TestConsoleChatNonStreamAndStream(t *testing.T) {
	e := newTestEnv(t, "", map[string][]string{"basic": {tokA}})
	e.up.postStream = func(c postCall) (io.ReadCloser, error) {
		return consoleSSE(consoleDelta("Hel"), consoleDelta("lo"), consoleCompleted()), nil
	}
	body := `{"model":"grok-4.5-console","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	w := e.post("/v1/chat/completions", body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	m := jsonBody(t, w)
	msg := m["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "Hello" {
		t.Fatalf("message = %v", msg)
	}
	if usage := m["usage"].(map[string]any); usage["total_tokens"] != float64(5) {
		t.Fatalf("console usage not passed through: %v", usage)
	}
	if calls := e.up.postCalls(); len(calls) != 1 || calls[0].URL != grok.ConsoleResponses {
		t.Fatalf("calls = %+v", calls)
	}

	w = e.post("/v1/chat/completions", strings.Replace(body, `"stream":false`, `"stream":true`, 1), nil)
	frames := sseDataFrames(w.Body.String())
	if len(frames) != 4 || !strings.Contains(frames[0], `"content":"Hel"`) || frames[3] != "[DONE]" {
		t.Fatalf("stream frames = %v", frames)
	}
}

func TestGatewayChatNonStreamSuccess(t *testing.T) {
	e := newTestEnv(t, "", map[string][]string{"super": {tokA}})
	e.up.gateway = func(token, modelName, prompt string) <-chan grok.GatewayEvent {
		ch := make(chan grok.GatewayEvent, 4)
		ch <- grok.GatewayEvent{Kind: grok.EventThinking, Content: "hmm", ConversationID: "c", ResponseID: "r"}
		ch <- grok.GatewayEvent{Kind: grok.EventText, Content: "4", ConversationID: "c", ResponseID: "r"}
		ch <- grok.GatewayEvent{Kind: grok.EventSoftStop, ConversationID: "c", ResponseID: "r"}
		close(ch)
		return ch
	}
	w := e.post("/v1/chat/completions", `{"model":"grok-4.6","stream":false,"messages":[{"role":"user","content":"2+2"}]}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	msg := jsonBody(t, w)["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "4" || msg["reasoning_content"] != "hmm" {
		t.Fatalf("message = %v", msg)
	}
}

func TestWSImageChatNonStream(t *testing.T) {
	e := newTestEnv(t, "", map[string][]string{"super": {tokA}})
	e.up.images = func(token, prompt string, n int) <-chan grok.ImagineEvent {
		ch := make(chan grok.ImagineEvent, 4)
		ch <- grok.ImagineEvent{Type: grok.ImagineEventProgress, ImageID: "i1", Progress: 50}
		ch <- grok.ImagineEvent{Type: grok.ImagineEventImage, ImageID: "i1", URL: "/images/i1.png", Progress: 100}
		close(ch)
		return ch
	}
	w := e.post("/v1/chat/completions", `{"model":"grok-imagine-image","stream":false,"messages":[{"role":"user","content":"a cat"}]}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	msg := jsonBody(t, w)["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "![image](https://assets.grok.com/images/i1.png)" {
		t.Fatalf("content = %q", msg["content"])
	}
}

func TestLiteImageGenerationReturnsURLs(t *testing.T) {
	e := newTestEnv(t, "", map[string][]string{"basic": {tokA}})
	e.up.postStream = scripted(scriptStep{frames: []string{
		webImageFrame(50, "u1", ""),
		webImageFrame(100, "u1", "users/x/u1.png"),
	}})
	w := e.post("/v1/images/generations", `{"model":"grok-imagine-image-lite","prompt":"a cat","n":1}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	data := jsonBody(t, w)["data"].([]any)
	if len(data) != 1 || data[0].(map[string]any)["url"] != "https://assets.grok.com/users/x/u1.png" {
		t.Fatalf("data = %v", data)
	}
}
