package api

import (
	"encoding/json"
	"strings"
)

// anthropicToOpenAIMessages converts Anthropic-style messages (and the
// top-level system prompt) to OpenAI-style chat messages.
func anthropicToOpenAIMessages(system any, messages []map[string]any) []map[string]any {
	out := []map[string]any{}
	if system != nil {
		switch s := system.(type) {
		case string:
			if strings.TrimSpace(s) != "" {
				out = append(out, map[string]any{"role": "system", "content": s})
			}
		case []any:
			var b strings.Builder
			for _, item := range s {
				m, _ := item.(map[string]any)
				if m == nil {
					continue
				}
				if t, _ := m["type"].(string); t == "text" {
					if text, _ := m["text"].(string); text != "" {
						b.WriteString(text)
					}
				}
			}
			if b.Len() > 0 {
				out = append(out, map[string]any{"role": "system", "content": b.String()})
			}
		}
	}
	for _, msg := range messages {
		role, _ := msg["role"].(string)
		if role == "" {
			role = "user"
		}
		content := msg["content"]
		switch c := content.(type) {
		case string:
			out = append(out, map[string]any{"role": role, "content": c})
		case []any:
			blocks := []any{}
			toolCalls := []any{}
			toolResults := []map[string]any{}
			for _, item := range c {
				bm, ok := item.(map[string]any)
				if !ok {
					continue
				}
				t, _ := bm["type"].(string)
				switch t {
				case "text":
					text, _ := bm["text"].(string)
					blocks = append(blocks, map[string]any{"type": "text", "text": text})
				case "image":
					src, _ := bm["source"].(map[string]any)
					if src != nil {
						srcType, _ := src["type"].(string)
						mediaType, _ := src["media_type"].(string)
						data, _ := src["data"].(string)
						if srcType == "base64" && data != "" {
							url := "data:" + mediaType + ";base64," + data
							blocks = append(blocks, map[string]any{
								"type":      "image_url",
								"image_url": map[string]any{"url": url},
							})
						}
						if srcType == "url" {
							if u, _ := src["url"].(string); u != "" {
								blocks = append(blocks, map[string]any{
									"type":      "image_url",
									"image_url": map[string]any{"url": u},
								})
							}
						}
					}
				case "tool_use":
					name, _ := bm["name"].(string)
					args, _ := json.Marshal(bm["input"])
					toolCalls = append(toolCalls, map[string]any{
						"id": bm["id"], "type": "function",
						"function": map[string]any{"name": name, "arguments": string(args)},
					})
				case "tool_result":
					toolResults = append(toolResults, map[string]any{
						"role":         "tool",
						"tool_call_id": bm["tool_use_id"],
						"content":      anthropicContentToString(bm["content"]),
					})
				}
			}
			if len(toolCalls) > 0 {
				out = append(out, map[string]any{"role": "assistant", "tool_calls": toolCalls})
			}
			out = append(out, toolResults...)
			if len(blocks) > 0 {
				out = append(out, map[string]any{"role": role, "content": blocks})
			}
		default:
			out = append(out, map[string]any{"role": role, "content": content})
		}
	}
	return out
}

func anthropicContentToString(content any) string {
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
			if t, _ := m["type"].(string); t == "text" {
				if text, _ := m["text"].(string); text != "" {
					b.WriteString(text)
				}
			}
		}
		return b.String()
	}
	return ""
}
