package grok

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gentpan/grok2api/internal/platform"
)

// FrameEventKind enumerates the event kinds emitted by StreamAdapter.
type FrameEventKind string

const (
	EventText          FrameEventKind = "text"           // final text token
	EventThinking      FrameEventKind = "thinking"       // reasoning token
	EventImage         FrameEventKind = "image"          // generated image final URL
	EventImageProgress FrameEventKind = "image_progress" // generated image progress percent
	EventSoftStop      FrameEventKind = "soft_stop"      // stream end signal
	EventSkip          FrameEventKind = "skip"           // filtered frame (carries AnnotationData for citations)
)

// FrameEvent is one parsed event from StreamAdapter.Feed.
type FrameEvent struct {
	Kind           FrameEventKind
	Content        string
	ImageID        string
	RolloutID      string
	MessageTag     string
	MessageStepID  *int
	AnnotationData map[string]any
}

// streamErrorFromPayload converts an in-band stream error JSON object
// ({"error":{...}}) to an UpstreamError. Returns nil when no error is present.
func streamErrorFromPayload(obj map[string]any) *platform.AppError {
	errObj, ok := obj["error"].(map[string]any)
	if !ok {
		return nil
	}
	message := "Upstream stream error"
	if v, ok := errObj["message"].(string); ok && v != "" {
		message = v
	} else if v, ok := errObj["error"].(string); ok && v != "" {
		message = v
	}
	encoded, _ := json.Marshal(obj)
	return platform.UpstreamError(
		"Upstream stream error: "+message,
		streamErrorStatus(errObj["code"], message),
		truncBody(encoded, 400),
	)
}

// raiseForStreamError returns an UpstreamError if data (raw JSON) contains an
// upstream stream error payload. Returns nil if no error.
func raiseForStreamError(data []byte) *platform.AppError {
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil
	}
	return streamErrorFromPayload(obj)
}

// StreamAdapter parses upstream SSE frames and emits FrameEvents. One
// instance per HTTP request; it is not safe for concurrent use.
type StreamAdapter struct {
	cards     map[string]map[string]any // card id -> card JSON
	citations citationState
	sources   sourceCollector

	annotations    []map[string]any
	textOffset     int // byte length of text emitted so far
	emittedKeys    map[string]struct{}
	lastRollout    string
	contentStarted bool
	thinkingBuf    []string
	textBuf        []string

	ImageURLs [][2]string // [(url, imageUuid), ...]

	// Conversation tracking — populated from the first frames of conversations/new.
	ConversationID string
	LastResponseID string // userResponse.responseId from the current message
}

// NewStreamAdapter returns a StreamAdapter with empty per-request state.
func NewStreamAdapter() *StreamAdapter {
	return &StreamAdapter{
		cards:       map[string]map[string]any{},
		citations:   newCitationState(),
		sources:     newSourceCollector(),
		emittedKeys: map[string]struct{}{},
	}
}

// Feed parses one JSON data payload and returns 0-N events. If the payload
// contains an in-band stream error, a non-nil *platform.AppError is returned
// instead of events.
func (s *StreamAdapter) Feed(data []byte) ([]FrameEvent, *platform.AppError) {
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, nil
	}
	if errObj := streamErrorFromPayload(obj); errObj != nil {
		return nil, errObj
	}
	result, _ := obj["result"].(map[string]any)
	if result == nil {
		return nil, nil
	}

	// Frame 1: conversation creation (only on conversations/new).
	if conv, _ := result["conversation"].(map[string]any); conv != nil {
		if cid, _ := conv["conversationId"].(string); cid != "" {
			s.ConversationID = cid
		}
		return nil, nil
	}

	resp, _ := result["response"].(map[string]any)
	if resp == nil {
		return nil, nil
	}

	// Frame 2: user response echo — capture responseId for conversation tracking.
	if userResp, _ := resp["userResponse"].(map[string]any); userResp != nil {
		if rid, _ := userResp["responseId"].(string); rid != "" {
			s.LastResponseID = rid
		}
	}

	events := []FrameEvent{}
	if card, ok := resp["cardAttachment"].(map[string]any); ok {
		events = append(events, s.handleCard(card)...)
	}
	s.sources.addWeb(resp["webSearchResults"])
	s.sources.addX(resp["xSearchResults"])
	return s.handleTokenFrame(resp, events), nil
}

// handleTokenFrame turns the token-bearing part of a response frame into
// events, appended to events.
func (s *StreamAdapter) handleTokenFrame(resp map[string]any, events []FrameEvent) []FrameEvent {
	token, _ := resp["token"].(string)
	thinking, _ := resp["isThinking"].(bool)
	tag, _ := resp["messageTag"].(string)
	rollout, _ := resp["rolloutId"].(string)

	switch {
	case tag == "tool_usage_card" || tag == "raw_function_result":
		// Tool usage cards are skipped in this minimal port (no reasoning
		// aggregator).
		return events
	case isToolOnlyFrame(resp):
		return events
	case token != "" && thinking:
		return s.handleThinking(token, rollout, events)
	case token != "" && tag == "final":
		return s.handleText(token, events)
	}

	if soft, _ := resp["isSoftStop"].(bool); soft {
		return append(events, FrameEvent{Kind: EventSoftStop})
	}
	if _, ok := resp["finalMetadata"]; ok {
		return append(events, FrameEvent{Kind: EventSoftStop})
	}
	return events
}

// isToolOnlyFrame reports a frame that belongs to a tool card and carries
// neither search nor code-execution results.
func isToolOnlyFrame(resp map[string]any) bool {
	if _, ok := resp["toolUsageCardId"]; !ok {
		return false
	}
	_, hasSearch := resp["webSearchResults"]
	_, hasCode := resp["codeExecutionResult"]
	return !hasSearch && !hasCode
}

// handleThinking handles a reasoning token. Once the answer has started,
// reasoning is only buffered (FullThinking); before that it is streamed as
// EventThinking events: an agent header whenever the rollout changes, then the
// reasoning text itself.
func (s *StreamAdapter) handleThinking(token, rollout string, events []FrameEvent) []FrameEvent {
	if s.contentStarted {
		if raw := strings.TrimSpace(token); raw != "" {
			s.thinkingBuf = append(s.thinkingBuf, withTrailingNewline(raw))
		}
		return events
	}
	raw := strings.TrimPrefix(token, "- ")
	if raw == "" {
		return events
	}
	if rollout != "" && rollout != s.lastRollout {
		s.lastRollout = rollout
		header := "\n[" + rollout + "]\n"
		s.thinkingBuf = append(s.thinkingBuf, header)
		events = append(events, FrameEvent{Kind: EventThinking, Content: header, RolloutID: rollout})
	}
	if text, fresh := s.addReasoning(raw, rollout); fresh {
		events = append(events, FrameEvent{Kind: EventThinking, Content: text, RolloutID: rollout})
	}
	return events
}

// addReasoning buffers one reasoning line (newline-terminated) unless the same
// rollout already produced it. It returns the buffered text.
func (s *StreamAdapter) addReasoning(line, rollout string) (string, bool) {
	key := rollout + ":" + line
	if _, seen := s.emittedKeys[key]; seen {
		return "", false
	}
	s.emittedKeys[key] = struct{}{}
	line = withTrailingNewline(line)
	s.thinkingBuf = append(s.thinkingBuf, line)
	return line, true
}

func withTrailingNewline(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

// handleText handles a final-answer token: citation tags are rewritten and the
// resulting text (plus any citation annotations) emitted.
func (s *StreamAdapter) handleText(token string, events []FrameEvent) []FrameEvent {
	s.contentStarted = true
	cleaned, spans := s.renderToken(token)
	if cleaned == "" {
		return events
	}
	s.textBuf = append(s.textBuf, cleaned)
	events = append(events, FrameEvent{Kind: EventText, Content: cleaned})
	for _, sp := range spans {
		ann := map[string]any{
			"type":        "url_citation",
			"url":         sp.URL,
			"title":       sp.Title,
			"start_index": s.textOffset + sp.Start,
			"end_index":   s.textOffset + sp.End,
		}
		s.annotations = append(s.annotations, ann)
		events = append(events, FrameEvent{Kind: EventSkip, AnnotationData: ann})
	}
	s.textOffset += len(cleaned)
	return events
}

// handleCard caches card data and emits image events on progress=100.
func (s *StreamAdapter) handleCard(cardRaw map[string]any) []FrameEvent {
	jsonStr, _ := cardRaw["jsonData"].(string)
	if jsonStr == "" {
		return nil
	}
	var jd map[string]any
	if err := json.Unmarshal([]byte(jsonStr), &jd); err != nil {
		return nil
	}
	if cardID, _ := jd["id"].(string); cardID != "" {
		s.cards[cardID] = jd
	}
	chunk, _ := jd["image_chunk"].(map[string]any)
	if chunk == nil {
		return nil
	}
	var events []FrameEvent
	progress, _ := chunk["progress"].(float64)
	imageUUID, _ := chunk["imageUuid"].(string)
	if progress != 0 {
		events = append(events, FrameEvent{
			Kind:    EventImageProgress,
			Content: fmt.Sprintf("%d", int(progress)),
			ImageID: imageUUID,
		})
	}
	if progress == 100 {
		if moderated, _ := chunk["moderated"].(bool); !moderated {
			if imageURL, _ := chunk["imageUrl"].(string); imageURL != "" {
				url := ImageBaseURL + imageURL
				s.ImageURLs = append(s.ImageURLs, [2]string{url, imageUUID})
				events = append(events, FrameEvent{Kind: EventImage, Content: url, ImageID: imageUUID})
			}
		}
	}
	return events
}

// Annotations returns the citation annotations collected so far.
func (s *StreamAdapter) Annotations() []map[string]any {
	out := make([]map[string]any, len(s.annotations))
	copy(out, s.annotations)
	return out
}

// SearchSources returns the structured search sources collected so far.
func (s *StreamAdapter) SearchSources() []map[string]any { return s.sources.list() }

// FullText returns the accumulated text content.
func (s *StreamAdapter) FullText() string { return strings.Join(s.textBuf, "") }

// FullThinking returns the accumulated reasoning text.
func (s *StreamAdapter) FullThinking() string { return strings.Join(s.thinkingBuf, "") }
