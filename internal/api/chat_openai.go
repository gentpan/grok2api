package api

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/gentpan/grok2api/internal/account"
	"github.com/gentpan/grok2api/internal/model"
)

// chatCompletionRequest is the OpenAI-compatible chat request body.
//
// tools / tool_choice / parallel_tool_calls / max_tokens are accepted for
// compatibility but not forwarded: the upstream web and console chats have no
// equivalent.
type chatCompletionRequest struct {
	Model             string           `json:"model"`
	Messages          []map[string]any `json:"messages"`
	Stream            *bool            `json:"stream,omitempty"`
	ReasoningEffort   *string          `json:"reasoning_effort,omitempty"`
	Temperature       *float64         `json:"temperature,omitempty"`
	TopP              *float64         `json:"top_p,omitempty"`
	ImageConfig       *imageConfig     `json:"image_config,omitempty"`
	VideoConfig       *videoConfig     `json:"video_config,omitempty"`
	Tools             []map[string]any `json:"tools,omitempty"`
	ToolChoice        any              `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool            `json:"parallel_tool_calls,omitempty"`
	MaxTokens         *int             `json:"max_tokens,omitempty"`
}

type imageConfig struct {
	N              int    `json:"n,omitempty"`
	Size           string `json:"size,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"`
}

type videoConfig struct {
	Seconds        int    `json:"seconds,omitempty"`
	Size           string `json:"size,omitempty"`
	ResolutionName string `json:"resolution_name,omitempty"`
	Preset         string `json:"preset,omitempty"`
}

// streamDefault resolves the effective stream flag of a request.
func (s *Server) streamDefault(req *bool) bool {
	if req != nil {
		return *req
	}
	return s.config().GetBool("features.stream", true)
}

// newChatCall builds the protocol-independent call from the pieces every
// chat-shaped endpoint parses.
func (s *Server) newChatCall(spec *model.Spec, modelName string, messages []map[string]any, stream bool,
	temperature, topP *float64, reasoningEffort *string) *chatCall {
	call := &chatCall{
		spec:        spec,
		modelName:   modelName,
		messages:    messages,
		stream:      stream,
		emitThink:   s.resolveEmitThink(reasoningEffort),
		temperature: defaultTemperature,
		topP:        defaultTopP,
	}
	if temperature != nil {
		call.temperature = *temperature
	}
	if topP != nil {
		call.topP = *topP
	}
	if reasoningEffort != nil {
		call.effort = *reasoningEffort
	}
	return call
}

// handleChatCompletions serves POST /v1/chat/completions.
func (s *Server) handleChatCompletions(c *gin.Context) {
	s.chatCompletions(c, callerFrom(c.Request.Context()))
}

// chatCompletions is handleChatCompletions for an explicit caller (the panel's
// chat tester acts on behalf of the admin, not of an API key).
func (s *Server) chatCompletions(c *gin.Context, caller Caller) {
	var req chatCompletionRequest
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
	call := s.newChatCall(spec, req.Model, req.Messages, stream, req.Temperature, req.TopP, req.ReasoningEffort)
	if req.ImageConfig != nil {
		call.imageN = req.ImageConfig.N
		call.imageSize = req.ImageConfig.Size
	}
	s.serveChat(c, caller, call, func() chatSink {
		return &openaiSink{
			c: c, stream: stream, id: "chatcmpl-" + uuid.NewString(), created: time.Now().Unix(),
			modelName: req.Model, emitThink: call.emitThink,
		}
	})
}

// serveChat runs call against an account (failing over per the retry policy)
// and renders the result through the sink newSink creates for every attempt.
//
// Pre-response failures are answered with a proper error status in the
// endpoint's own envelope; failures after the response was committed are
// reported by the sink in the protocol's error event.
func (s *Server) serveChat(c *gin.Context, caller Caller, call *chatCall, newSink func() chatSink) {
	if perr := s.prepareChat(call); perr != nil {
		writeAppError(c, perr)
		return
	}
	res, err := s.acquire(c.Request.Context(), caller, call.spec, acquireOptions{}, func(ctx context.Context, lease *account.Lease) (bool, error) {
		return s.chatAttempt(ctx, lease, call, newSink())
	})
	if err != nil && !res.Committed {
		writeAppError(c, err)
	}
}

// chatAttempt performs one attempt of call on lease.
func (s *Server) chatAttempt(ctx context.Context, lease *account.Lease, call *chatCall, sink chatSink) (committed bool, err error) {
	timeout := chatAttemptTimeout
	if call.backend == backendImagine {
		timeout = imageAttemptTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	src, track, err := s.openChat(ctx, lease, call)
	if err != nil {
		return false, err
	}
	defer src.Close()

	if sink.Streaming() {
		sink.Begin()
		committed = true
	}
	events := 0
	for {
		evs, nerr := src.Next()
		for _, ev := range evs {
			sink.Event(ev)
		}
		events += len(evs)
		if nerr == io.EOF {
			if events == 0 {
				// An upstream that ends without a single token is a failed
				// outcome, never an empty 200.
				nerr = errEmptyUpstream()
			} else {
				break
			}
		}
		if nerr != nil {
			// A client that went away needs no error frame, and its abort is
			// not a failed request in the usage statistics.
			if committed && !errors.Is(nerr, context.Canceled) {
				sink.Fail(nerr)
			}
			return committed, nerr
		}
	}
	meta := src.Meta()
	sink.End(meta)
	track.save(meta)
	return true, nil
}

// makeStreamChunk builds a chat.completion.chunk frame.
func makeStreamChunk(id string, created int64, modelName, content, reasoning string, isFinal bool) map[string]any {
	delta := map[string]any{}
	if content != "" {
		delta["content"] = content
	} else if reasoning != "" {
		delta["reasoning_content"] = reasoning
	} else if !isFinal {
		delta["role"] = "assistant"
	}
	choice := map[string]any{
		"index":         0,
		"delta":         delta,
		"finish_reason": nil,
	}
	if isFinal {
		choice["delta"] = map[string]any{}
		choice["finish_reason"] = "stop"
	}
	return map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   modelName,
		"choices": []any{choice},
	}
}

// makeChatResponse builds a non-streaming chat.completion response.
func makeChatResponse(id string, created int64, modelName, text, thinking string, emitThink bool) map[string]any {
	msg := map[string]any{"role": "assistant", "content": text}
	if emitThink && thinking != "" {
		msg["reasoning_content"] = thinking
	}
	return map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   modelName,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		},
	}
}
