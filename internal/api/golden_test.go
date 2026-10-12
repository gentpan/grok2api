package api

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/gentpan/grok2api/internal/model"
	"github.com/gentpan/grok2api/internal/platform"
)

// mustJSON renders v the way the wire does (sorted map keys).
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestMakeStreamChunkGolden(t *testing.T) {
	cases := []struct {
		name               string
		content, reasoning string
		final              bool
		want               string
	}{
		{"role opener", "", "", false,
			`{"choices":[{"delta":{"role":"assistant"},"finish_reason":null,"index":0}],"created":7,"id":"cid","model":"m","object":"chat.completion.chunk"}`},
		{"content", "hi", "", false,
			`{"choices":[{"delta":{"content":"hi"},"finish_reason":null,"index":0}],"created":7,"id":"cid","model":"m","object":"chat.completion.chunk"}`},
		{"reasoning", "", "hmm", false,
			`{"choices":[{"delta":{"reasoning_content":"hmm"},"finish_reason":null,"index":0}],"created":7,"id":"cid","model":"m","object":"chat.completion.chunk"}`},
		{"final", "", "", true,
			`{"choices":[{"delta":{},"finish_reason":"stop","index":0}],"created":7,"id":"cid","model":"m","object":"chat.completion.chunk"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mustJSON(t, makeStreamChunk("cid", 7, "m", tc.content, tc.reasoning, tc.final))
			if got != tc.want {
				t.Fatalf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}

func TestMakeChatResponseGolden(t *testing.T) {
	got := mustJSON(t, makeChatResponse("cid", 7, "m", "answer", "because", true))
	want := `{"choices":[{"finish_reason":"stop","index":0,"message":{"content":"answer","reasoning_content":"because","role":"assistant"}}],` +
		`"created":7,"id":"cid","model":"m","object":"chat.completion","usage":{"completion_tokens":0,"prompt_tokens":0,"total_tokens":0}}`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	// Thinking is dropped when the client did not ask for it.
	got = mustJSON(t, makeChatResponse("cid", 7, "m", "answer", "because", false))
	if want := `"message":{"content":"answer","role":"assistant"}`; !strings.Contains(got, want) {
		t.Fatalf("thinking leaked: %s", got)
	}
}

func TestExtractMessagesGolden(t *testing.T) {
	msgs := []map[string]any{
		{"role": "system", "content": "be brief"},
		{"role": "developer", "content": "dev note"},
		{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "look"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://x/y.png"}},
		}},
		{"content": "no role means user"},
		{"role": "tool", "content": "tool out"},
		{"role": "weird", "content": "mapped to user"},
	}
	text, files, perr := extractMessages(msgs)
	if perr != nil {
		t.Fatal(perr)
	}
	want := "[system]: be brief\n\n[system]: dev note\n\n[user]: look\n\n\n[user]: no role means user\n\n[tool]: tool out\n\n[user]: mapped to user"
	if text != want {
		t.Fatalf("text = %q\nwant   %q", text, want)
	}
	if !reflect.DeepEqual(files, []string{"https://x/y.png"}) {
		t.Fatalf("files = %v", files)
	}

	if _, _, perr := extractMessages(nil); perr == nil || perr.Status != 400 {
		t.Fatalf("empty messages must be a 400, got %v", perr)
	}
}

func TestAnthropicToOpenAIMessagesGolden(t *testing.T) {
	system := []any{map[string]any{"type": "text", "text": "sys A "}, map[string]any{"type": "text", "text": "sys B"}}
	in := []map[string]any{
		{"role": "user", "content": "hello"},
		{"role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": "let me check"},
			map[string]any{"type": "tool_use", "id": "tu_1", "name": "lookup", "input": map[string]any{"q": "x"}},
		}},
		{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "tu_1", "content": []any{map[string]any{"type": "text", "text": "found"}}},
			map[string]any{"type": "text", "text": "thanks"},
			map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "QUJD"}},
		}},
	}
	got := anthropicToOpenAIMessages(system, in)
	want := `[` +
		`{"content":"sys A sys B","role":"system"},` +
		`{"content":"hello","role":"user"},` +
		`{"role":"assistant","tool_calls":[{"function":{"arguments":"{\"q\":\"x\"}","name":"lookup"},"id":"tu_1","type":"function"}]},` +
		`{"content":[{"text":"let me check","type":"text"}],"role":"assistant"},` +
		`{"content":"found","role":"tool","tool_call_id":"tu_1"},` +
		`{"content":[{"text":"thanks","type":"text"},{"image_url":{"url":"data:image/png;base64,QUJD"},"type":"image_url"}],"role":"user"}` +
		`]`
	// An assistant turn with text + tool_use becomes the tool_calls message
	// followed by a separate text message (pins the existing shape).
	if s := mustJSON(t, got); s != want {
		t.Fatalf("got  %s\nwant %s", s, want)
	}
}

func TestResponsesInputToMessagesGolden(t *testing.T) {
	got := responsesInputToMessages([]any{
		map[string]any{"type": "message", "role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": "hi "},
			map[string]any{"type": "input_text", "text": "there"},
		}},
		map[string]any{"type": "function_call", "call_id": "c1", "name": "f", "arguments": `{"a":1}`},
		map[string]any{"type": "function_call_output", "call_id": "c1", "output": "42"},
	}, "be nice")
	want := `[{"content":"be nice","role":"system"},{"content":"hi there","role":"user"},` +
		`{"role":"assistant","tool_calls":[{"function":{"arguments":"{\"a\":1}","name":"f"},"id":"c1","type":"function"}]},` +
		`{"content":"42","role":"tool","tool_call_id":"c1"}]`
	if s := mustJSON(t, got); s != want {
		t.Fatalf("got  %s\nwant %s", s, want)
	}
	if s := mustJSON(t, responsesInputToMessages("plain", "")); s != `[{"content":"plain","role":"user"}]` {
		t.Fatalf("string input: %s", s)
	}
}

func TestShouldRetryUpstream(t *testing.T) {
	useConfig(t, "[retry]\non_codes = \"429, 503\"\n")
	rc := newRetryConfig(globalConfig{}, "")
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain error", errors.New("boom"), false},
		{"429 configured", platform.UpstreamError("x", 429, ""), true},
		{"503 configured", platform.UpstreamError("x", 503, ""), true},
		{"401 not configured", platform.UpstreamError("x", 401, ""), false},
		{"502 not configured", platform.UpstreamError("x", 502, ""), false},
		{"invalid credentials body always retries", platform.UpstreamError("x", 403, "Blocked-User"), true},
		{"validation error", platform.ValidationError("bad", "p"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rc.shouldRetry(tc.err); got != tc.want {
				t.Fatalf("shouldRetry = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestModeCandidates(t *testing.T) {
	auto := &model.Spec{ModelName: "a", ModeId: model.ModeAuto}
	fast := &model.Spec{ModelName: "f", ModeId: model.ModeFast}

	useConfig(t, "[features]\nauto_chat_mode_fallback = true\n")
	rc := newRetryConfig(globalConfig{}, "")
	if got := rc.modeCandidates(auto); !reflect.DeepEqual(got, []int{0, 1, 2}) {
		t.Fatalf("auto with fallback = %v", got)
	}
	if got := rc.modeCandidates(fast); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("fast = %v", got)
	}
	useConfig(t, "[features]\nauto_chat_mode_fallback = false\n")
	rc = newRetryConfig(globalConfig{}, "")
	if got := rc.modeCandidates(auto); !reflect.DeepEqual(got, []int{0}) {
		t.Fatalf("auto without fallback = %v", got)
	}
}

func TestComputeAggregateProgress(t *testing.T) {
	cases := []struct {
		name  string
		m     map[string]int
		total int
		want  int
	}{
		{"no total", map[string]int{"a": 10}, 0, 100},
		{"empty", map[string]int{}, 2, 0},
		{"half", map[string]int{"a": 100, "b": 0}, 2, 50},
		{"clamped", map[string]int{"a": 500, "b": -5}, 2, 50},
		{"all done", map[string]int{"a": 100, "b": 100}, 2, 100},
		{"fewer reported than expected", map[string]int{"a": 100}, 4, 25},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := computeAggregateProgress(tc.m, tc.total); got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestParseIntStr(t *testing.T) {
	ok := map[string]int{"0": 0, "7": 7, "12": 12, "-3": -3, "000": 0}
	for in, want := range ok {
		got, err := parseIntStr(in)
		if err != nil || got != want {
			t.Fatalf("parseIntStr(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "-", "1x", "x1", " 1", "1.5"} {
		if _, err := parseIntStr(in); err == nil {
			t.Fatalf("parseIntStr(%q) accepted", in)
		}
	}
}
