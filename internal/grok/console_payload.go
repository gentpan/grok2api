package grok

import "encoding/json"

// BuildConsolePayload builds the JSON payload for POST console.x.ai/v1/responses.
// It converts OpenAI chat messages to the Responses API input format and
// applies the per-model features from console_models.go.
func BuildConsolePayload(messages []map[string]any, model string, temperature, topP float64, reasoningEffort string, stream bool) map[string]any {
	upstream := consoleUpstreamName(model)
	traits := consoleUpstreams[upstream]

	maxTokens := traits.MaxOutputTokens
	if maxTokens == 0 {
		maxTokens = defaultConsoleMaxOutputTokens
	}
	payload := map[string]any{
		"model":             upstream,
		"input":             consoleInput(messages),
		"max_output_tokens": maxTokens,
		"temperature":       temperature,
		"top_p":             topP,
		"store":             false,
		"include":           []string{"reasoning.encrypted_content"},
		"stream":            stream,
	}
	if traits.Reasoning {
		payload["reasoning"] = map[string]any{"effort": consoleEffort(model, reasoningEffort)}
	}
	if traits.SearchTools {
		payload["tools"] = []map[string]any{
			{"type": "web_search", "enable_image_understanding": true},
			{"type": "x_search", "enable_video_understanding": true},
		}
		payload["tool_choice"] = "auto"
	}
	return payload
}

// consoleInput converts OpenAI messages to Responses API input items.
// Messages that yield no content blocks are dropped.
func consoleInput(messages []map[string]any) []map[string]any {
	items := []map[string]any{}
	for _, msg := range messages {
		blocks := consoleBlocks(msg["content"])
		if len(blocks) == 0 {
			continue
		}
		items = append(items, map[string]any{"role": consoleRole(msg), "content": blocks})
	}
	return items
}

// consoleRole maps an OpenAI role to a Responses API role.
func consoleRole(msg map[string]any) string {
	role, _ := msg["role"].(string)
	switch role {
	case "system", "developer":
		return "system"
	case "assistant":
		return "assistant"
	}
	return "user"
}

// consoleBlocks converts one message's content (string or content-part array)
// to input_text / input_image blocks.
func consoleBlocks(content any) []map[string]any {
	switch c := content.(type) {
	case string:
		return []map[string]any{{"type": "input_text", "text": c}}
	case []any:
		var blocks []map[string]any
		for _, part := range c {
			bm, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if block := consoleBlock(bm); block != nil {
				blocks = append(blocks, block)
			}
		}
		return blocks
	}
	return []map[string]any{{"type": "input_text", "text": toString(content)}}
}

func consoleBlock(part map[string]any) map[string]any {
	kind, _ := part["type"].(string)
	switch kind {
	case "text":
		text, _ := part["text"].(string)
		return map[string]any{"type": "input_text", "text": text}
	case "image_url":
		obj, _ := part["image_url"].(map[string]any)
		if url, _ := obj["url"].(string); url != "" {
			return map[string]any{"type": "input_image", "image_url": url}
		}
		return nil
	}
	text, _ := part["text"].(string)
	if text == "" {
		text = toString(part)
	}
	return map[string]any{"type": "input_text", "text": text}
}

func toString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []byte:
		return string(t)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
