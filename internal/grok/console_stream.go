package grok

import (
	"encoding/json"
	"strings"

	"github.com/gentpan/grok2api/internal/platform"
)

// ConsoleStreamAdapter parses console.x.ai SSE events and yields text tokens.
// Only response.output_text.delta is emitted; response.completed extracts
// usage. Once a terminal event (completed / failed / incomplete / error) was
// seen the adapter latches and ignores everything after it.
type ConsoleStreamAdapter struct {
	textBuf []string
	Usage   map[string]any
	done    bool
}

// NewConsoleStreamAdapter returns a fresh adapter.
func NewConsoleStreamAdapter() *ConsoleStreamAdapter { return &ConsoleStreamAdapter{} }

// Feed parses one SSE event and returns the text tokens it emitted (0 or 1) or
// the failure it reported.
func (s *ConsoleStreamAdapter) Feed(eventType, data string) ([]string, *platform.AppError) {
	if s.done {
		return nil, nil
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(data), &obj); err != nil {
		return nil, nil
	}
	switch eventType {
	case "response.output_text.delta":
		delta, _ := obj["delta"].(string)
		if delta == "" {
			return nil, nil
		}
		s.textBuf = append(s.textBuf, delta)
		return []string{delta}, nil
	case "response.completed":
		if resp, _ := obj["response"].(map[string]any); resp != nil {
			s.Usage, _ = resp["usage"].(map[string]any)
		}
		s.done = true
	case "error":
		s.done = true
		msg, _ := obj["message"].(string)
		if msg == "" {
			msg = toString(obj)
		}
		return nil, platform.UpstreamError("Console API error: "+msg, 502, "")
	case "response.failed":
		s.done = true
		resp, _ := obj["response"].(map[string]any)
		errObj, _ := resp["error"].(map[string]any)
		msg, _ := errObj["message"].(string)
		if msg == "" {
			msg = "response failed"
		}
		code, _ := errObj["code"].(string)
		return nil, platform.UpstreamError("Console response failed: "+msg, streamErrorStatus(code, msg), "")
	case "response.incomplete":
		s.done = true
		resp, _ := obj["response"].(map[string]any)
		details, _ := resp["incomplete_details"].(map[string]any)
		reason, _ := details["reason"].(string)
		if reason == "" {
			reason = "unknown"
		}
		return nil, platform.UpstreamError("Console response incomplete: "+reason, 502, "")
	}
	return nil, nil
}

// FullText returns the accumulated text.
func (s *ConsoleStreamAdapter) FullText() string { return strings.Join(s.textBuf, "") }
