package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// handleMessages serves the Anthropic-compatible POST /v1/messages.
func (s *Server) handleMessages(c *gin.Context) {
	var req struct {
		Model       string           `json:"model"`
		Messages    []map[string]any `json:"messages"`
		System      any              `json:"system,omitempty"`
		MaxTokens   *int             `json:"max_tokens,omitempty"`
		Stream      *bool            `json:"stream,omitempty"`
		Temperature *float64         `json:"temperature,omitempty"`
		TopP        *float64         `json:"top_p,omitempty"`
		Tools       []map[string]any `json:"tools,omitempty"`
		ToolChoice  any              `json:"tool_choice,omitempty"`
		Thinking    map[string]any   `json:"thinking,omitempty"`
	}
	if err := readJSON(c, &req); err != nil {
		writeAppError(c, err)
		return
	}
	noteUsageModel(c, req.Model)
	spec, ok := s.modelRegistry().Resolve(req.Model)
	if !ok {
		writeAppError(c, errModelNotFound(req.Model))
		return
	}
	stream := s.streamDefault(req.Stream)
	// Anthropic clients opt in to thinking blocks explicitly; they are never
	// added to a response that did not ask for them (the blocks carry an empty
	// signature, which strict clients reject).
	emitThink := false
	if req.Thinking != nil {
		t, _ := req.Thinking["type"].(string)
		emitThink = t == "enabled"
	}

	messages := anthropicToOpenAIMessages(req.System, req.Messages)
	call := s.newChatCall(spec, req.Model, messages, stream, req.Temperature, req.TopP, nil)
	call.emitThink = emitThink
	s.serveChat(c, callerFrom(c.Request.Context()), call, func() chatSink {
		return &anthropicSink{
			c: c, stream: stream, modelName: req.Model,
			messageID: "msg_" + uuid.NewString(), emitThink: emitThink,
		}
	})
}

// anthropicSink renders a chat attempt as an Anthropic message.
type anthropicSink struct {
	c         *gin.Context
	stream    bool
	modelName string
	messageID string
	emitThink bool

	sw      *sseWriter
	index   int    // index of the next content block
	open    string // kind of the open content block: "", "text" or "thinking"
	emitted bool   // any text written (for image separators)
	col     collector
}

func (k *anthropicSink) Streaming() bool { return k.stream }

func (k *anthropicSink) Begin() {
	if !k.stream {
		return
	}
	k.sw = newSSEWriter(k.c.Writer)
	k.sw.writeEventJSON("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": k.messageID, "type": "message", "role": "assistant",
			"model": k.modelName, "content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
	k.sw.writeEventJSON("ping", map[string]any{"type": "ping"})
}

// ensureBlock opens a content block of the given kind, closing the previous
// one when the kind changes (reasoning arrives before the answer).
func (k *anthropicSink) ensureBlock(kind string) {
	if k.open == kind {
		return
	}
	k.closeBlock()
	block := map[string]any{"type": "text", "text": ""}
	if kind == "thinking" {
		block = map[string]any{"type": "thinking", "thinking": "", "signature": ""}
	}
	k.sw.writeEventJSON("content_block_start", map[string]any{
		"type": "content_block_start", "index": k.index, "content_block": block,
	})
	k.open = kind
}

func (k *anthropicSink) closeBlock() {
	if k.open == "" {
		return
	}
	k.sw.writeEventJSON("content_block_stop", map[string]any{"type": "content_block_stop", "index": k.index})
	k.index++
	k.open = ""
}

func (k *anthropicSink) delta(kind, text string) {
	deltaType, field := "text_delta", "text"
	if kind == "thinking" {
		deltaType, field = "thinking_delta", "thinking"
	}
	k.ensureBlock(kind)
	k.sw.writeEventJSON("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": k.index,
		"delta": map[string]any{"type": deltaType, field: text},
	})
}

func (k *anthropicSink) Event(ev chatEvent) {
	if !k.stream {
		k.col.add(ev)
		return
	}
	switch ev.Kind {
	case evText:
		if ev.Content != "" {
			k.delta("text", ev.Content)
			k.emitted = true
		}
	case evThinking:
		if k.emitThink && ev.Content != "" {
			k.delta("thinking", ev.Content)
		}
	case evImage:
		md := markdownImage(ev.Content)
		if k.emitted {
			md = "\n\n" + md
		}
		k.delta("text", md)
		k.emitted = true
	}
}

func anthropicUsage(meta chatMeta) map[string]any {
	return map[string]any{
		"input_tokens":  usageNumber(meta.Usage, "input_tokens"),
		"output_tokens": usageNumber(meta.Usage, "output_tokens"),
	}
}

func (k *anthropicSink) End(meta chatMeta) {
	if !k.stream {
		blocks := []any{}
		if thinking := k.col.Thinking(meta.TrimThinking); k.emitThink && thinking != "" {
			blocks = append(blocks, map[string]any{"type": "thinking", "thinking": thinking, "signature": ""})
		}
		blocks = append(blocks, map[string]any{"type": "text", "text": k.col.Text()})
		k.c.JSON(http.StatusOK, map[string]any{
			"id": k.messageID, "type": "message", "role": "assistant", "model": k.modelName,
			"content": blocks, "stop_reason": "end_turn", "stop_sequence": nil,
			"usage": anthropicUsage(meta),
		})
		return
	}
	if k.index == 0 && k.open == "" {
		k.ensureBlock("text") // an empty answer still has one (empty) text block
	}
	k.closeBlock()
	k.sw.writeEventJSON("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": usageNumber(meta.Usage, "output_tokens")},
	})
	k.sw.writeEventJSON("message_stop", map[string]any{"type": "message_stop"})
}

func (k *anthropicSink) Fail(err error) {
	markUsageFailed(k.c)
	if k.sw == nil {
		return
	}
	appErr := asAppError(err)
	k.sw.writeAnthropicError(anthropicErrorType(appErr), appErr.Message)
}
