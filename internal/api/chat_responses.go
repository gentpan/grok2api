package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// handleResponses serves the OpenAI Responses API (POST /v1/responses).
func (s *Server) handleResponses(c *gin.Context) {
	var req struct {
		Model        string           `json:"model"`
		Input        any              `json:"input"`
		Instructions string           `json:"instructions,omitempty"`
		Stream       *bool            `json:"stream,omitempty"`
		Reasoning    map[string]any   `json:"reasoning,omitempty"`
		Temperature  *float64         `json:"temperature,omitempty"`
		TopP         *float64         `json:"top_p,omitempty"`
		Tools        []map[string]any `json:"tools,omitempty"`
		ToolChoice   any              `json:"tool_choice,omitempty"`
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
	var effort *string
	if e, ok := req.Reasoning["effort"].(string); ok {
		effort = &e
	}
	messages := responsesInputToMessages(req.Input, req.Instructions)
	call := s.newChatCall(spec, req.Model, messages, stream, req.Temperature, req.TopP, effort)
	s.serveChat(c, callerFrom(c.Request.Context()), call, func() chatSink {
		return &responsesSink{
			c: c, stream: stream, modelName: req.Model,
			responseID: "resp_" + uuid.NewString(), itemID: "msg_" + uuid.NewString(),
			created: time.Now().Unix(),
		}
	})
}

// responsesSink renders a chat attempt as an OpenAI Responses API response.
type responsesSink struct {
	c          *gin.Context
	stream     bool
	modelName  string
	responseID string
	itemID     string
	created    int64

	sw      *sseWriter
	emitted string // text sent so far (streaming)
	col     collector
}

func (k *responsesSink) Streaming() bool { return k.stream }

func (k *responsesSink) responseObject(status string, output []any) map[string]any {
	return map[string]any{
		"id": k.responseID, "object": "response", "created_at": k.created,
		"status": status, "model": k.modelName, "output": output,
	}
}

func (k *responsesSink) messageItem(text, status string) map[string]any {
	content := []any{}
	if status == "completed" {
		content = []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}
	}
	return map[string]any{
		"id": k.itemID, "type": "message", "role": "assistant",
		"status": status, "content": content,
	}
}

func (k *responsesSink) Begin() {
	if !k.stream {
		return
	}
	k.sw = newSSEWriter(k.c.Writer)
	inProgress := map[string]any{
		"id": k.responseID, "object": "response", "created_at": k.created,
		"status": "in_progress", "model": k.modelName,
	}
	k.sw.writeEventJSON("response.created", map[string]any{"type": "response.created", "response": inProgress})
	k.sw.writeComment("heartbeat")
	k.sw.writeEventJSON("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": 0,
		"item": k.messageItem("", "in_progress"),
	})
	k.sw.writeEventJSON("response.content_part.added", map[string]any{
		"type": "response.content_part.added", "item_id": k.itemID, "output_index": 0,
		"content_index": 0,
		"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})
}

func (k *responsesSink) Event(ev chatEvent) {
	if !k.stream {
		k.col.add(ev)
		return
	}
	var delta string
	switch ev.Kind {
	case evText:
		delta = ev.Content
	case evImage:
		delta = markdownImage(ev.Content)
		if k.emitted != "" {
			delta = "\n\n" + delta
		}
	default:
		return // reasoning has no place in a plain message item
	}
	if delta == "" {
		return
	}
	k.emitted += delta
	k.sw.writeEventJSON("response.output_text.delta", map[string]any{
		"type": "response.output_text.delta", "item_id": k.itemID,
		"output_index": 0, "content_index": 0, "delta": delta,
	})
}

func usageTriple(meta chatMeta) map[string]any {
	in, out := usageNumber(meta.Usage, "input_tokens"), usageNumber(meta.Usage, "output_tokens")
	total := usageNumber(meta.Usage, "total_tokens")
	if total == 0 {
		total = in + out
	}
	return map[string]any{"input_tokens": in, "output_tokens": out, "total_tokens": total}
}

// usageNumber reads an integer-valued counter from an upstream usage object.
func usageNumber(usage map[string]any, key string) int {
	n, _ := numberAsInt(usage[key])
	return n
}

func (k *responsesSink) End(meta chatMeta) {
	if !k.stream {
		text := k.col.Text()
		resp := k.responseObject("completed", []any{k.messageItem(text, "completed")})
		resp["usage"] = usageTriple(meta)
		k.c.JSON(http.StatusOK, resp)
		return
	}
	text := k.emitted
	part := map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
	k.sw.writeEventJSON("response.output_text.done", map[string]any{
		"type": "response.output_text.done", "item_id": k.itemID,
		"output_index": 0, "content_index": 0, "text": text,
	})
	k.sw.writeEventJSON("response.content_part.done", map[string]any{
		"type": "response.content_part.done", "item_id": k.itemID, "output_index": 0,
		"content_index": 0, "part": part,
	})
	done := k.messageItem(text, "completed")
	k.sw.writeEventJSON("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": 0, "item": done,
	})
	resp := k.responseObject("completed", []any{done})
	resp["usage"] = usageTriple(meta)
	k.sw.writeEventJSON("response.completed", map[string]any{"type": "response.completed", "response": resp})
	k.sw.writeDone()
}

func (k *responsesSink) Fail(err error) {
	markUsageFailed(k.c)
	if k.sw == nil {
		return
	}
	appErr := asAppError(err)
	k.sw.writeEventJSON("error", map[string]any{
		"type": "error", "code": appErr.Code, "message": appErr.Message, "param": nil,
	})
	resp := k.responseObject("failed", []any{})
	resp["error"] = map[string]any{"code": appErr.Code, "message": appErr.Message}
	k.sw.writeEventJSON("response.failed", map[string]any{"type": "response.failed", "response": resp})
	k.sw.writeDone()
}
