package api

import (
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/gentpan/grok2api/internal/platform"
)

// chatSink renders the events of one chat attempt in a wire protocol.
//
// A streaming sink commits the HTTP response in Begin (after the upstream was
// opened successfully) and writes incrementally. An aggregating sink writes
// nothing until End, so a failure before End can still be retried on another
// account and answered with a proper error status.
type chatSink interface {
	// Streaming reports whether the sink writes incrementally.
	Streaming() bool
	// Begin commits the response (streaming sinks only).
	Begin()
	Event(ev chatEvent)
	// End finishes a successful response.
	End(meta chatMeta)
	// Fail reports a failure after Begin in the protocol's error event.
	Fail(err error)
}

// markdownImage renders an image URL the way chat clients display it.
func markdownImage(url string) string { return "![image](" + url + ")" }

// imageProgressText is the reasoning text emitted for web image progress.
func imageProgressText(percent string) string { return "image generating " + percent + "%" }

// collector accumulates the events of a non-streaming response.
type collector struct {
	text     []string
	thinking []string
	images   []string
}

func (a *collector) add(ev chatEvent) {
	switch ev.Kind {
	case evText:
		a.text = append(a.text, ev.Content)
	case evThinking:
		a.thinking = append(a.thinking, ev.Content)
	case evImageProgress:
		a.thinking = append(a.thinking, imageProgressText(ev.Content)+"\n")
	case evImage:
		a.images = append(a.images, ev.Content)
	}
}

// Text is the answer text; finished images follow it as markdown.
func (a *collector) Text() string {
	text := strings.Join(a.text, "")
	if len(a.images) > 0 {
		mds := make([]string, len(a.images))
		for i, u := range a.images {
			mds[i] = markdownImage(u)
		}
		if text != "" {
			text += "\n\n"
		}
		text += strings.Join(mds, "\n\n")
	}
	return text
}

func (a *collector) Thinking(trim bool) string {
	t := strings.Join(a.thinking, "")
	if trim {
		t = strings.TrimRight(t, "\n")
	}
	return t
}

// streamErrorFields flattens err for an OpenAI-style SSE error frame.
func streamErrorFields(err error) (message, kind, code, param string) {
	if appErr := platformAppError(err); appErr != nil {
		return appErr.Message, string(appErr.Kind), appErr.Code, appErr.Param
	}
	return err.Error(), string(platform.ErrUpstream), "", ""
}

// --- OpenAI chat.completion ---

type openaiSink struct {
	c         *gin.Context
	stream    bool
	id        string
	created   int64
	modelName string
	emitThink bool

	sw  *sseWriter
	col collector
}

func (k *openaiSink) Streaming() bool { return k.stream }

func (k *openaiSink) Begin() {
	if !k.stream {
		return
	}
	k.sw = newSSEWriter(k.c.Writer)
	k.sw.writeComment("heartbeat")
}

func (k *openaiSink) Event(ev chatEvent) {
	if !k.stream {
		k.col.add(ev)
		return
	}
	switch ev.Kind {
	case evText:
		k.sw.writeJSONData(makeStreamChunk(k.id, k.created, k.modelName, ev.Content, "", false))
	case evThinking:
		if k.emitThink {
			k.sw.writeJSONData(makeReasoningChunk(k.id, k.created, k.modelName, ev.Content))
		}
	case evImageProgress:
		k.sw.writeJSONData(makeReasoningChunk(k.id, k.created, k.modelName, imageProgressText(ev.Content)))
	case evImage:
		k.sw.writeJSONData(makeStreamChunk(k.id, k.created, k.modelName, markdownImage(ev.Content), "", false))
	}
}

func (k *openaiSink) End(meta chatMeta) {
	if k.stream {
		k.sw.writeJSONData(makeStreamChunk(k.id, k.created, k.modelName, "", "", true))
		k.sw.writeDone()
		return
	}
	resp := makeChatResponse(k.id, k.created, k.modelName, k.col.Text(), k.col.Thinking(meta.TrimThinking), k.emitThink)
	if meta.Usage != nil {
		resp["usage"] = meta.Usage
	}
	k.c.JSON(200, resp)
}

func (k *openaiSink) Fail(err error) {
	markUsageFailed(k.c)
	if k.sw == nil {
		return
	}
	k.sw.writeOpenAIError(streamErrorFields(err))
}

// makeReasoningChunk is a chat.completion.chunk carrying reasoning text.
func makeReasoningChunk(id string, created int64, modelName, reasoning string) map[string]any {
	return map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   modelName,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{"reasoning_content": reasoning},
			"finish_reason": nil,
		}},
	}
}
