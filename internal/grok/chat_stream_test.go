package grok

import (
	"encoding/json"
	"reflect"
	"testing"
)

// frame wraps a response object in the {"result":{"response":...}} envelope.
func respFrame(t *testing.T, resp map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"result": map[string]any{"response": resp}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func cardFrame(t *testing.T, card map[string]any) []byte {
	t.Helper()
	jd, err := json.Marshal(card)
	if err != nil {
		t.Fatal(err)
	}
	return respFrame(t, map[string]any{"cardAttachment": map[string]any{"jsonData": string(jd)}})
}

func mustFeed(t *testing.T, a *StreamAdapter, data []byte) []FrameEvent {
	t.Helper()
	events, err := a.Feed(data)
	if err != nil {
		t.Fatalf("unexpected stream error: %v", err)
	}
	return events
}

func TestStreamAdapterConversationAndUserResponseFrames(t *testing.T) {
	a := NewStreamAdapter()
	conv, _ := json.Marshal(map[string]any{"result": map[string]any{"conversation": map[string]any{"conversationId": "conv-1"}}})
	if ev := mustFeed(t, a, conv); len(ev) != 0 {
		t.Fatalf("conversation frame emitted %#v", ev)
	}
	if a.ConversationID != "conv-1" {
		t.Fatalf("ConversationID = %q", a.ConversationID)
	}
	ev := mustFeed(t, a, respFrame(t, map[string]any{"userResponse": map[string]any{"responseId": "resp-9"}}))
	if len(ev) != 0 {
		t.Fatalf("user response frame emitted %#v", ev)
	}
	if a.LastResponseID != "resp-9" {
		t.Fatalf("LastResponseID = %q", a.LastResponseID)
	}
}

func TestStreamAdapterIgnoresMalformedAndEmptyFrames(t *testing.T) {
	a := NewStreamAdapter()
	for _, raw := range []string{`not json`, `{}`, `{"result":{}}`, `{"result":{"response":{}}}`, `[]`} {
		ev, err := a.Feed([]byte(raw))
		if err != nil || len(ev) != 0 {
			t.Fatalf("Feed(%q) = %#v, %v", raw, ev, err)
		}
	}
}

func TestStreamAdapterFinalTextAndSoftStop(t *testing.T) {
	a := NewStreamAdapter()
	got := mustFeed(t, a, respFrame(t, map[string]any{"token": "Hello", "messageTag": "final"}))
	want := []FrameEvent{{Kind: EventText, Content: "Hello"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %#v, want %#v", got, want)
	}
	got = mustFeed(t, a, respFrame(t, map[string]any{"token": " world", "messageTag": "final"}))
	if len(got) != 1 || got[0].Content != " world" {
		t.Fatalf("events = %#v", got)
	}
	if a.FullText() != "Hello world" {
		t.Fatalf("FullText = %q", a.FullText())
	}
	got = mustFeed(t, a, respFrame(t, map[string]any{"isSoftStop": true}))
	if len(got) != 1 || got[0].Kind != EventSoftStop {
		t.Fatalf("soft stop events = %#v", got)
	}
	got = mustFeed(t, a, respFrame(t, map[string]any{"finalMetadata": map[string]any{"x": 1}}))
	if len(got) != 1 || got[0].Kind != EventSoftStop {
		t.Fatalf("finalMetadata events = %#v", got)
	}
}

func TestStreamAdapterNonFinalTokenIsNotText(t *testing.T) {
	a := NewStreamAdapter()
	// A token without tag "final" and not thinking is dropped.
	if ev := mustFeed(t, a, respFrame(t, map[string]any{"token": "x", "messageTag": "summary"})); len(ev) != 0 {
		t.Fatalf("events = %#v", ev)
	}
}

func TestStreamAdapterToolCardsAndFunctionResultsAreSkipped(t *testing.T) {
	a := NewStreamAdapter()
	for _, resp := range []map[string]any{
		{"token": "tool", "messageTag": "tool_usage_card"},
		{"token": "raw", "messageTag": "raw_function_result"},
		{"token": "t", "toolUsageCardId": "c1", "messageTag": "final"},
	} {
		if ev := mustFeed(t, a, respFrame(t, resp)); len(ev) != 0 {
			t.Fatalf("frame %#v emitted %#v", resp, ev)
		}
	}
	if a.FullText() != "" {
		t.Fatalf("FullText = %q", a.FullText())
	}
}

func TestStreamAdapterThinkingTokens(t *testing.T) {
	a := NewStreamAdapter()
	got := mustFeed(t, a, respFrame(t, map[string]any{"token": "- step one", "isThinking": true, "rolloutId": "agentA", "messageTag": "assistant"}))
	var thinking string
	for _, ev := range got {
		if ev.Kind != EventThinking {
			t.Fatalf("unexpected event kind %q in %#v", ev.Kind, got)
		}
		thinking += ev.Content
	}
	// The agent header and the reasoning text both reach the caller as
	// thinking events, and together equal the buffered reasoning.
	if thinking != "\n[agentA]\nstep one\n" || a.FullThinking() != thinking {
		t.Fatalf("thinking events = %q, FullThinking = %q", thinking, a.FullThinking())
	}
	// The same line from the same rollout is deduped.
	again := mustFeed(t, a, respFrame(t, map[string]any{"token": "- step one", "isThinking": true, "rolloutId": "agentA"}))
	if len(again) != 0 {
		t.Fatalf("duplicate thinking emitted %#v", again)
	}
	// A different rollout gets its own header.
	other := mustFeed(t, a, respFrame(t, map[string]any{"token": "- step two", "isThinking": true, "rolloutId": "agentB"}))
	if len(other) != 2 || other[0].Content != "\n[agentB]\n" || other[0].RolloutID != "agentB" ||
		other[1].Content != "step two\n" || other[1].RolloutID != "agentB" {
		t.Fatalf("agentB events = %#v", other)
	}
}

func TestStreamAdapterThinkingAfterContentGoesToBufferOnly(t *testing.T) {
	a := NewStreamAdapter()
	mustFeed(t, a, respFrame(t, map[string]any{"token": "answer", "messageTag": "final"}))
	ev := mustFeed(t, a, respFrame(t, map[string]any{"token": "late thought", "isThinking": true}))
	if len(ev) != 0 {
		t.Fatalf("late thinking emitted %#v", ev)
	}
	if a.FullThinking() != "late thought\n" {
		t.Fatalf("FullThinking = %q", a.FullThinking())
	}
}

func TestStreamAdapterImageCardProgressAndFinal(t *testing.T) {
	a := NewStreamAdapter()
	progress := mustFeed(t, a, cardFrame(t, map[string]any{
		"id":          "card-1",
		"image_chunk": map[string]any{"progress": 40, "imageUuid": "img-1"},
	}))
	if len(progress) != 1 || progress[0].Kind != EventImageProgress || progress[0].Content != "40" || progress[0].ImageID != "img-1" {
		t.Fatalf("progress = %#v", progress)
	}
	final := mustFeed(t, a, cardFrame(t, map[string]any{
		"id":          "card-1",
		"image_chunk": map[string]any{"progress": 100, "imageUuid": "img-1", "imageUrl": "users/u/img-1/content"},
	}))
	if len(final) != 2 || final[0].Kind != EventImageProgress || final[1].Kind != EventImage {
		t.Fatalf("final = %#v", final)
	}
	wantURL := "https://assets.grok.com/users/u/img-1/content"
	if final[1].Content != wantURL || final[1].ImageID != "img-1" {
		t.Fatalf("image event = %#v", final[1])
	}
	if len(a.ImageURLs) != 1 || a.ImageURLs[0] != [2]string{wantURL, "img-1"} {
		t.Fatalf("ImageURLs = %#v", a.ImageURLs)
	}
}

func TestStreamAdapterModeratedImageEmitsNoURL(t *testing.T) {
	a := NewStreamAdapter()
	ev := mustFeed(t, a, cardFrame(t, map[string]any{
		"id":          "card-2",
		"image_chunk": map[string]any{"progress": 100, "imageUuid": "img-2", "imageUrl": "x", "moderated": true},
	}))
	for _, e := range ev {
		if e.Kind == EventImage {
			t.Fatalf("moderated image produced %#v", e)
		}
	}
	if len(a.ImageURLs) != 0 {
		t.Fatalf("ImageURLs = %#v", a.ImageURLs)
	}
}

func TestStreamAdapterInlineCitationAndAnnotations(t *testing.T) {
	a := NewStreamAdapter()
	mustFeed(t, a, cardFrame(t, map[string]any{"id": "c1", "url": "https://example.com/a", "title": "Example A"}))
	token := `See<grok:render card_id="c1" card_type="citation_card" type="render_inline_citation"></grok:render>.`
	ev := mustFeed(t, a, respFrame(t, map[string]any{"token": token, "messageTag": "final"}))
	if len(ev) != 2 {
		t.Fatalf("events = %#v", ev)
	}
	if ev[0].Kind != EventText || ev[0].Content != "See [[1]](https://example.com/a)." {
		t.Fatalf("text event = %#v", ev[0])
	}
	if ev[1].Kind != EventSkip || ev[1].AnnotationData["type"] != "url_citation" ||
		ev[1].AnnotationData["url"] != "https://example.com/a" || ev[1].AnnotationData["title"] != "Example A" {
		t.Fatalf("annotation event = %#v", ev[1])
	}
	needle := " [[1]](https://example.com/a)"
	if ev[1].AnnotationData["start_index"] != 3 || ev[1].AnnotationData["end_index"] != 3+len(needle) {
		t.Fatalf("annotation span = %#v", ev[1].AnnotationData)
	}
	if got := a.Annotations(); len(got) != 1 {
		t.Fatalf("Annotations = %#v", got)
	}

	// The same source cited again immediately after itself is collapsed.
	ev = mustFeed(t, a, respFrame(t, map[string]any{"token": `<grok:render card_id="c1" card_type="citation_card" type="render_inline_citation"></grok:render>tail`, "messageTag": "final"}))
	if len(ev) != 1 || ev[0].Content != "tail" {
		t.Fatalf("repeat citation events = %#v", ev)
	}
}

func TestStreamAdapterSearchedImageAndGeneratedImageRender(t *testing.T) {
	a := NewStreamAdapter()
	mustFeed(t, a, cardFrame(t, map[string]any{
		"id":    "img-card",
		"image": map[string]any{"title": "Cat", "thumbnail": "https://t/1.jpg", "link": "https://src/1"},
	}))
	mustFeed(t, a, cardFrame(t, map[string]any{"id": "gen-card"}))
	token := `A<grok:render card_id="img-card" card_type="x" type="render_searched_image"></grok:render>` +
		`B<grok:render card_id="gen-card" card_type="x" type="render_generated_image"></grok:render>` +
		`C<grok:render card_id="unknown" card_type="x" type="render_inline_citation"></grok:render>`
	ev := mustFeed(t, a, respFrame(t, map[string]any{"token": token, "messageTag": "final"}))
	if len(ev) != 1 || ev[0].Content != "A[![Cat](https://t/1.jpg)](https://src/1)BC" {
		t.Fatalf("events = %#v", ev)
	}
}

func TestStreamAdapterCollectsWebAndXSearchSources(t *testing.T) {
	a := NewStreamAdapter()
	frame := respFrame(t, map[string]any{
		"webSearchResults": map[string]any{"results": []any{
			map[string]any{"url": "https://a.test/1", "title": "One"},
			map[string]any{"url": "https://a.test/1", "title": "Dup"},
			map[string]any{"title": "no url"},
		}},
		"xSearchResults": map[string]any{"results": []any{
			map[string]any{"postId": "42", "username": "alice", "text": "hello   there"},
			map[string]any{"postId": "", "username": "bob"},
		}},
	})
	mustFeed(t, a, frame)
	mustFeed(t, a, frame) // second delivery must not duplicate
	got := a.SearchSources()
	want := []map[string]any{
		{"url": "https://a.test/1", "title": "One", "type": "web"},
		{"url": "https://x.com/alice/status/42", "title": "𝕏/@alice: hello there", "type": "x_post"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sources = %#v, want %#v", got, want)
	}
}

func TestStreamAdapterInBandErrors(t *testing.T) {
	cases := []struct {
		name   string
		frame  string
		status int
	}{
		{"code 8", `{"error":{"code":8,"message":"quota"}}`, 429},
		{"too many requests", `{"error":{"code":1,"message":"Too Many Requests"}}`, 429},
		{"rate limit text", `{"error":{"message":"hit the rate limit"}}`, 429},
		{"other", `{"error":{"code":7,"message":"Request rejected by anti-bot rules."}}`, 502},
		{"no message", `{"error":{"code":3}}`, 502},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := NewStreamAdapter()
			ev, err := a.Feed([]byte(tc.frame))
			if err == nil || err.Status != tc.status || ev != nil {
				t.Fatalf("Feed = %#v, %v; want status %d", ev, err, tc.status)
			}
			if err.Body == "" {
				t.Fatal("error carries no body for account-level classification")
			}
		})
	}
	// An error with no usable message still yields a message.
	_, err := NewStreamAdapter().Feed([]byte(`{"error":{"code":3}}`))
	if err == nil || err.Message != "Upstream stream error: Upstream stream error" {
		t.Fatalf("message = %v", err)
	}
}

func TestStreamErrorFromPayloadAndRaise(t *testing.T) {
	if streamErrorFromPayload(map[string]any{"result": 1}) != nil {
		t.Fatal("non-error payload produced an error")
	}
	if raiseForStreamError([]byte(`garbage`)) != nil {
		t.Fatal("garbage produced an error")
	}
	if e := raiseForStreamError([]byte(`{"error":{"error":"inner msg","code":8}}`)); e == nil || e.Status != 429 {
		t.Fatalf("raiseForStreamError = %v", e)
	}
}
