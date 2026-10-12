package grok

import (
	"reflect"
	"testing"
)

func TestBuildConsolePayloadSnapshot(t *testing.T) {
	msgs := []map[string]any{
		{"role": "system", "content": "be nice"},
		{"role": "developer", "content": "dev note"},
		{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "look"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://i/x.png"}},
			map[string]any{"type": "image_url", "image_url": map[string]any{}}, // dropped: no url
			map[string]any{"type": "weird", "text": "fallback"},
		}},
		{"role": "assistant", "content": "ok"},
		{"content": "no role means user"},
		{"role": "user", "content": []any{}}, // no blocks: message dropped
	}
	got := roundTrip(t, BuildConsolePayload(msgs, "grok-4.5-high", 0.7, 0.9, "low", true))
	want := parseJSON(t, `{
	  "model": "grok-4.5",
	  "input": [
	    {"role": "system", "content": [{"type": "input_text", "text": "be nice"}]},
	    {"role": "system", "content": [{"type": "input_text", "text": "dev note"}]},
	    {"role": "user", "content": [
	      {"type": "input_text", "text": "look"},
	      {"type": "input_image", "image_url": "https://i/x.png"},
	      {"type": "input_text", "text": "fallback"}
	    ]},
	    {"role": "assistant", "content": [{"type": "input_text", "text": "ok"}]},
	    {"role": "user", "content": [{"type": "input_text", "text": "no role means user"}]}
	  ],
	  "max_output_tokens": 2000000,
	  "temperature": 0.7,
	  "top_p": 0.9,
	  "store": false,
	  "include": ["reasoning.encrypted_content"],
	  "stream": true,
	  "reasoning": {"effort": "high"},
	  "tools": [
	    {"type": "web_search", "enable_image_understanding": true},
	    {"type": "x_search", "enable_video_understanding": true}
	  ],
	  "tool_choice": "auto"
	}`)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("payload mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestBuildConsolePayloadModelTable(t *testing.T) {
	cases := []struct {
		model, requestEffort string
		upstream             string
		effort               string // "" = no reasoning field
		maxTokens            int
		tools                bool
	}{
		{"grok-4.3-console", "", "grok-4.3", "medium", 1_000_000, true},
		{"grok-4.3-low", "xhigh", "grok-4.3", "low", 1_000_000, true}, // fixed effort wins
		{"grok-4.5-console", "minimal", "grok-4.5", "low", 2_000_000, true},
		{"grok-4.5-console", "bogus", "grok-4.5", "medium", 2_000_000, true},
		{"grok-4.20-multi-agent-xhigh", "", "grok-4.20-multi-agent-0309", "xhigh", 2_000_000, true},
		{"grok-4.20-0309-console", "high", "grok-4.20-0309", "", 1_000_000, true},
		{"grok-build-console", "", "grok-build-0.1", "", 256_000, true},
		{"grok-imagine-image-console", "", "grok-imagine-image", "", 1_000_000, false},
		{"some-unknown-model", "high", "some-unknown-model", "", 1_000_000, false},
	}
	for _, tc := range cases {
		t.Run(tc.model+"/"+tc.requestEffort, func(t *testing.T) {
			p := roundTrip(t, BuildConsolePayload(nil, tc.model, 1, 1, tc.requestEffort, false))
			if p["model"] != tc.upstream {
				t.Errorf("model = %v, want %s", p["model"], tc.upstream)
			}
			if int(p["max_output_tokens"].(float64)) != tc.maxTokens {
				t.Errorf("max_output_tokens = %v, want %d", p["max_output_tokens"], tc.maxTokens)
			}
			r, hasReasoning := p["reasoning"].(map[string]any)
			if (tc.effort != "") != hasReasoning || (hasReasoning && r["effort"] != tc.effort) {
				t.Errorf("reasoning = %#v, want effort %q", p["reasoning"], tc.effort)
			}
			if _, hasTools := p["tools"]; hasTools != tc.tools {
				t.Errorf("tools present = %v, want %v", hasTools, tc.tools)
			}
		})
	}
}

func TestConsoleModelsPublicSurface(t *testing.T) {
	// api resolves upstream model names through this exported map.
	for pub, up := range map[string]string{
		"grok-imagine-image-console":       "grok-imagine-image",
		"grok-imagine-video-console":       "grok-imagine-video",
		"grok-4.20-multi-agent-console":    "grok-4.20-multi-agent-0309",
		"grok-build-console":               "grok-build-0.1",
		"grok-4.20-0309-reasoning-console": "grok-4.20-0309-reasoning",
	} {
		if ConsoleModels[pub] != up {
			t.Errorf("ConsoleModels[%q] = %q, want %q", pub, ConsoleModels[pub], up)
		}
	}
}

func TestConsoleUpstreamModel(t *testing.T) {
	for in, want := range map[string]string{
		"grok-4.5-high":      "grok-4.5",
		"grok-4.5":           "grok-4.5", // already an upstream name
		"unknown-model-x":    "unknown-model-x",
		"grok-build-console": "grok-build-0.1",
	} {
		if got := ConsoleUpstreamModel(in); got != want {
			t.Errorf("ConsoleUpstreamModel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConsoleStreamAdapterDeltaAndCompleted(t *testing.T) {
	a := NewConsoleStreamAdapter()
	if tok, err := a.Feed("response.output_text.delta", `{"delta":"he"}`); err != nil || len(tok) != 1 || tok[0] != "he" {
		t.Fatalf("delta = %v, %v", tok, err)
	}
	if tok, _ := a.Feed("response.output_text.delta", `{"delta":""}`); tok != nil {
		t.Fatalf("empty delta emitted %v", tok)
	}
	if tok, _ := a.Feed("response.output_text.delta", `not json`); tok != nil {
		t.Fatalf("bad json emitted %v", tok)
	}
	a.Feed("response.output_text.delta", `{"delta":"llo"}`)
	if a.FullText() != "hello" {
		t.Fatalf("FullText = %q", a.FullText())
	}
	if tok, err := a.Feed("response.completed", `{"response":{"usage":{"input_tokens":3,"output_tokens":5}}}`); tok != nil || err != nil {
		t.Fatalf("completed = %v, %v", tok, err)
	}
	if a.Usage["output_tokens"] != float64(5) {
		t.Fatalf("Usage = %#v", a.Usage)
	}
	if tok, err := a.Feed("response.output_text.delta", `{"delta":"late"}`); tok != nil || err != nil {
		t.Fatalf("emitted after completed: %v, %v", tok, err)
	}
}

func TestConsoleStreamAdapterErrorEvent(t *testing.T) {
	a := NewConsoleStreamAdapter()
	_, err := a.Feed("error", `{"message":"upstream exploded"}`)
	if err == nil || err.Status != 502 {
		t.Fatalf("err = %v", err)
	}
	if err.Message != "Console API error: upstream exploded" {
		t.Fatalf("message = %q", err.Message)
	}
}
