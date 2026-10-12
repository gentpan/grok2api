package api

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gentpan/grok2api/internal/platform"
)

// normalizeRole maps an OpenAI message role to the role label of the
// flattened prompt.
func normalizeRole(role string) string {
	switch role {
	case "system", "developer":
		return "system"
	case "assistant":
		return "assistant"
	case "tool":
		return "tool"
	}
	return "user"
}

// renderToolCalls renders an assistant message's tool_calls as prompt text.
func renderToolCalls(role string, raw any) string {
	calls, _ := raw.([]any)
	var lines []string
	for _, item := range calls {
		call, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := call["function"].(map[string]any)
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		args, _ := fn["arguments"].(string)
		lines = append(lines, fmt.Sprintf("[%s]: tool_call %s(%s)", role, name, args))
	}
	return strings.Join(lines, "\n")
}

// scalarText renders a non-string, non-list content value.
func scalarText(v any) string {
	switch t := v.(type) {
	case float64, bool, int, int64:
		return fmt.Sprint(t)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// extractMessages flattens OpenAI messages into a single prompt string and a
// list of uploaded file references (image URLs / data URIs).
//
// A message with no content (an assistant turn that only carries tool_calls)
// contributes its tool calls and nothing else.
func extractMessages(messages []map[string]any) (string, []string, *platform.AppError) {
	var parts []string
	fileInputs := []string{}
	for _, msg := range messages {
		role, _ := msg["role"].(string)
		if role == "" {
			role = "user"
		}
		role = normalizeRole(role)

		var b strings.Builder
		switch c := msg["content"].(type) {
		case nil:
			// tool_calls-only turn: handled below.
		case string:
			fmt.Fprintf(&b, "[%s]: %s", role, c)
		case []any:
			for _, item := range c {
				bm, ok := item.(map[string]any)
				if !ok {
					continue
				}
				t, _ := bm["type"].(string)
				switch t {
				case "text":
					text, _ := bm["text"].(string)
					fmt.Fprintf(&b, "[%s]: %s\n", role, text)
				case "image_url":
					urlObj, _ := bm["image_url"].(map[string]any)
					if urlObj != nil {
						if u, _ := urlObj["url"].(string); u != "" {
							fileInputs = append(fileInputs, u)
						}
					}
				}
			}
		default:
			fmt.Fprintf(&b, "[%s]: %s", role, scalarText(c))
		}
		if calls := renderToolCalls(role, msg["tool_calls"]); calls != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(calls)
		}
		if b.Len() > 0 {
			parts = append(parts, b.String())
		}
	}
	text := strings.TrimSpace(strings.Join(parts, "\n\n"))
	if text == "" {
		return "", nil, platform.ValidationError("Empty message after extraction", "messages")
	}
	return text, fileInputs, nil
}

// extractImagePrompt extracts the text prompt from chat messages: the last
// non-system text wins.
func extractImagePrompt(messages []map[string]any) string {
	var prompt string
	for _, msg := range messages {
		role, _ := msg["role"].(string)
		if role == "system" || role == "developer" {
			continue
		}
		switch c := msg["content"].(type) {
		case string:
			if c != "" {
				prompt = c
			}
		case []any:
			for _, item := range c {
				bm, ok := item.(map[string]any)
				if !ok {
					continue
				}
				if t, _ := bm["type"].(string); t == "text" {
					if text, _ := bm["text"].(string); text != "" {
						prompt = text
					}
				}
			}
		}
	}
	return strings.TrimSpace(prompt)
}

// resolveEmitThink decides whether to forward thinking tokens to the client:
// an explicit reasoning effort wins, otherwise features.thinking.
func (s *Server) resolveEmitThink(effort *string) bool {
	if effort != nil {
		e := strings.ToLower(strings.TrimSpace(*effort))
		if e == "none" {
			return false
		}
		return e != ""
	}
	return s.config().GetBool("features.thinking", true)
}
