package grok

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/gentpan/grok2api/internal/platform"
)

// This file is the wire protocol of grok.com's WebSocket Gateway
// (wss://grok.com/ws/mgw/): the client messages, the server event parser and
// the error mapping. The dial/run loop lives in gateway_ws.go and the
// per-turn state machine in gateway_session.go.

const gatewayDiagnosticBytes = 768

var gatewaySensitiveValueRE = regexp.MustCompile(`(?i)(sso(?:-rw)?|authorization|cookie)\s*[:=]\s*(?:bearer\s+)?[^;\s<]+|eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)

// GatewayEvent is one normalized event from the Grok WebSocket Gateway.
type GatewayEvent struct {
	Kind           FrameEventKind
	Content        string
	ConversationID string
	ResponseID     string
	Err            error
}

// gatewayEventResult is the state accumulated while parsing a Gateway turn.
type gatewayEventResult struct {
	SessionID      string
	ConversationID string
	ResponseID     string
	Text           string
	Reasoning      string
	Done           bool

	// lastKind / lastContent describe the delta produced by the most recent
	// event (empty when it produced none).
	lastKind    FrameEventKind
	lastContent string
}

// chatFeatures are the account-independent chat switches shared by the REST
// payload and the Gateway session.
type chatFeatures struct {
	Temporary         bool
	Memory            bool
	CustomInstruction string
}

// --- client messages -------------------------------------------------------

// gatewaySessionSettings is the x_grok block of session.create.
func gatewaySessionSettings(f chatFeatures) map[string]any {
	return map[string]any{
		"protocol_capabilities":   []string{"conversation_attached", "custom_methods_v1", "workspace_servers_v1"},
		"use_chunk":               true,
		"enable_side_by_side":     true,
		"force_side_by_side":      false,
		"enable_image_generation": true,
		"image_generation_count":  2,
		"disable_text_follow_ups": false,
		"disable_artifact":        true,
		"force_concise":           false,
		"keep_context":            false,
		"is_temporary":            f.Temporary,
		"disable_memory":          !f.Memory,
	}
}

// buildGatewaySessionCreate builds the first client event of a connection.
func buildGatewaySessionCreate(modelName, eventID string, f chatFeatures) map[string]any {
	return map[string]any{
		"event": map[string]any{
			"type":     "session.create",
			"event_id": eventID,
			"session": map[string]any{
				"model":  modelName,
				"x_grok": gatewaySessionSettings(f),
			},
		},
	}
}

// buildGatewayTurnMessages builds the response.create event used by Grok Web.
func buildGatewayTurnMessages(sessionID, prompt string, attachments []string) map[string]any {
	chunks := make([]any, 0, len(attachments)+1)
	for _, attachment := range attachments {
		chunks = append(chunks, map[string]any{
			"mention": map[string]any{
				"target": map[string]any{
					"file_mention": map[string]any{"file_id": attachment},
				},
			},
		})
	}
	chunks = append(chunks, map[string]any{"text": map[string]any{"text": prompt}})
	item := map[string]any{
		"type": "message",
		"role": "user",
		"x_grok": map[string]any{
			"client_message_id": uuid.NewString(),
			"input_chunks":      chunks,
		},
	}
	event := map[string]any{
		"type":     "response.create",
		"event_id": fmt.Sprintf("evt_resp_%d", time.Now().UnixMilli()),
		"item":     item,
	}
	if len(attachments) > 0 {
		item["file_attachment_ids"] = attachments
		event["file_attachment_ids"] = attachments
	}
	return map[string]any{"session_id": sessionID, "event": event}
}

// buildGatewayPing is the application-level keepalive.
func buildGatewayPing(now time.Time) map[string]any {
	return map[string]any{"event": map[string]any{
		"type": "ping", "event_id": fmt.Sprintf("evt_hb_%d", now.UnixMilli()),
	}}
}

// --- server events ---------------------------------------------------------

// parseGatewayEvent normalizes one decoded Gateway event into parsed. An
// in-band error event or stream_error is returned as *platform.AppError.
func parseGatewayEvent(event map[string]any, parsed *gatewayEventResult) error {
	if parsed == nil {
		return fmt.Errorf("nil Gateway parse result")
	}
	parsed.lastKind = ""
	parsed.lastContent = ""
	typeName, _ := event["type"].(string)
	switch typeName {
	case "conversation.attached":
		conversation, _ := event["conversation"].(map[string]any)
		parsed.ConversationID, _ = conversation["id"].(string)
	case "response.chunk":
		chunk, _ := event["chunk"].(map[string]any)
		text, _ := chunk["text"].(map[string]any)
		if text == nil {
			return nil
		}
		delta, _ := text["text"].(string)
		channel, _ := text["channel"].(string)
		appendGatewayText(parsed, channel, delta)
	case "response.output_text.delta":
		delta, _ := event["delta"].(string)
		appendGatewayText(parsed, "CHANNEL_ASSISTANT_RESPONSE", delta)
	case "response.output_text.done":
		text, _ := event["text"].(string)
		if parsed.Text == "" && text != "" {
			appendGatewayText(parsed, "CHANNEL_ASSISTANT_RESPONSE", text)
		}
	case "response.grok.output":
		output, _ := event["output"].(map[string]any)
		if streamError, _ := output["stream_error"].(map[string]any); streamError != nil {
			message, _ := streamError["message"].(string)
			if message == "" {
				message = "Grok Gateway stream error"
			}
			return platform.UpstreamError(message, streamErrorStatus(streamError["code"], message), compactGatewayJSON(event))
		}
	case "response.done":
		response, _ := event["response"].(map[string]any)
		parsed.ResponseID, _ = response["id"].(string)
		status, _ := response["status"].(string)
		if status != "" && status != "completed" {
			if details, _ := response["status_details"].(map[string]any); details != nil {
				if reason, _ := details["reason"].(string); reason != "" {
					return upstreamErr("Grok Gateway response status is %s (%s)", status, reason)
				}
			}
			return upstreamErr("Grok Gateway response status is %s", status)
		}
		parsed.Done = true
	case "error":
		return gatewayEventError(event)
	}
	return nil
}

func appendGatewayText(parsed *gatewayEventResult, channel, delta string) {
	if delta == "" {
		return
	}
	ch := strings.ToLower(channel)
	if strings.Contains(ch, "reason") || strings.Contains(ch, "analysis") || strings.Contains(ch, "thinking") {
		parsed.Reasoning += delta
		parsed.lastKind = EventThinking
	} else {
		parsed.Text += delta
		parsed.lastKind = EventText
	}
	parsed.lastContent = delta
}

func gatewayEventError(event map[string]any) error {
	switch value := event["error"].(type) {
	case map[string]any:
		message, _ := value["message"].(string)
		if message == "" {
			message, _ = value["error"].(string)
		}
		if message == "" {
			message = "Grok Gateway stream error"
		}
		return platform.UpstreamError(message, streamErrorStatus(value["code"], message), compactGatewayJSON(event))
	case string:
		if value != "" {
			return platform.UpstreamError(value, 502, compactGatewayJSON(event))
		}
	}
	return platform.UpstreamError("Grok Gateway stream error", 502, compactGatewayJSON(event))
}

func unwrapGatewayEvent(envelope map[string]any) map[string]any {
	if event, ok := envelope["event"].(map[string]any); ok {
		return event
	}
	return envelope
}

func compactGatewayJSON(value any) string {
	data, _ := json.Marshal(value)
	return truncUTF8(string(data), 4096)
}

// --- diagnostics -----------------------------------------------------------

// redactSensitive masks cookies, authorization values and JWTs in text that
// came from (or may echo) a request.
func redactSensitive(s string) string {
	return gatewaySensitiveValueRE.ReplaceAllString(s, "[redacted]")
}

func gatewayHandshakeDiagnostic(errText string, status int, statusText, body string) string {
	parts := []string{errText}
	if status > 0 {
		label := strings.TrimSpace(statusText)
		prefix := fmt.Sprintf("%d ", status)
		if strings.HasPrefix(label, prefix) {
			label = strings.TrimSpace(strings.TrimPrefix(label, prefix))
		}
		if label == "" {
			label = http.StatusText(status)
		}
		parts = append(parts, strings.TrimSpace(fmt.Sprintf("HTTP %d %s", status, label)))
	}
	if body = strings.TrimSpace(body); body != "" {
		body = redactSensitive(body)
		if len(body) > gatewayDiagnosticBytes {
			body = truncUTF8(body, gatewayDiagnosticBytes) + "..."
		}
		parts = append(parts, "body="+body)
	}
	return strings.Join(parts, "; ")
}

func firstMapString(value map[string]any, paths ...[]string) string {
	for _, path := range paths {
		current := any(value)
		for _, key := range path {
			obj, ok := current.(map[string]any)
			if !ok {
				current = nil
				break
			}
			current = obj[key]
		}
		if result, ok := current.(string); ok && strings.TrimSpace(result) != "" {
			return strings.TrimSpace(result)
		}
	}
	return ""
}
