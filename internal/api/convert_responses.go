package api

import "strings"

// responsesInputToMessages converts the Responses API `input` (and
// `instructions`) to chat messages.
func responsesInputToMessages(input any, instructions string) []map[string]any {
	out := []map[string]any{}
	if strings.TrimSpace(instructions) != "" {
		out = append(out, map[string]any{"role": "system", "content": instructions})
	}
	switch v := input.(type) {
	case string:
		out = append(out, map[string]any{"role": "user", "content": v})
	case []any:
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			t, _ := m["type"].(string)
			switch t {
			case "message":
				role, _ := m["role"].(string)
				if role == "" {
					role = "user"
				}
				out = append(out, map[string]any{"role": role, "content": responsesContentToText(m["content"])})
			case "function_call":
				name, _ := m["name"].(string)
				args, _ := m["arguments"].(string)
				out = append(out, map[string]any{
					"role": "assistant",
					"tool_calls": []any{map[string]any{
						"id": m["call_id"], "type": "function",
						"function": map[string]any{"name": name, "arguments": args},
					}},
				})
			case "function_call_output":
				out = append(out, map[string]any{
					"role":         "tool",
					"tool_call_id": m["call_id"],
					"content":      m["output"],
				})
			}
		}
	}
	return out
}

// responsesContentToText flattens a Responses message content: plain text for
// text-only content, a chat content array when an image is present.
func responsesContentToText(content any) any {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			t, _ := m["type"].(string)
			if t == "input_text" || t == "output_text" || t == "text" {
				if text, ok := m["text"].(string); ok {
					b.WriteString(text)
				}
			}
			if t == "input_image" || t == "image" {
				if u, ok := m["image_url"].(string); ok {
					return []any{map[string]any{"type": "text", "text": ""}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}}}
				}
			}
		}
		return b.String()
	}
	return content
}
