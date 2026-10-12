package grok

import (
	"github.com/gentpan/grok2api/internal/model"
)

// defaultToolOverrides disables the account connectors the web app would
// otherwise search.
func defaultToolOverrides() map[string]any {
	return map[string]any{
		"gmailSearch":           false,
		"googleCalendarSearch":  false,
		"outlookSearch":         false,
		"outlookCalendarSearch": false,
		"googleDriveSearch":     false,
	}
}

// BuildChatPayload constructs the JSON payload for POST
// /rest/app-chat/conversations/new. fileAttachments are pre-uploaded
// file_metadata IDs. The temporary / memory / custom-instruction switches come
// from config (features.*).
func BuildChatPayload(message string, modeID model.ModeId, fileAttachments []string, toolOverrides map[string]any, modelConfigOverride map[string]any, requestOverrides map[string]any) map[string]any {
	return buildChatPayload(chatFeaturesFromConfig(), message, modeID, fileAttachments, toolOverrides, modelConfigOverride, requestOverrides)
}

// buildChatPayload is BuildChatPayload with the config-derived features
// injected, so it is a pure function.
func buildChatPayload(f chatFeatures, message string, modeID model.ModeId, fileAttachments []string, toolOverrides map[string]any, modelConfigOverride map[string]any, requestOverrides map[string]any) map[string]any {
	tools := toolOverrides
	if tools == nil {
		tools = defaultToolOverrides()
	}
	if fileAttachments == nil {
		fileAttachments = []string{}
	}

	payload := map[string]any{
		"collectionIds":        []any{},
		"disabledConnectorIds": []any{},
		"deviceEnvInfo": map[string]any{
			"darkModeEnabled":  false,
			"devicePixelRatio": 1,
			"screenHeight":     1080,
			"screenWidth":      1920,
			"viewportHeight":   945,
			"viewportWidth":    1920,
		},
		"disableMemory":               !f.Memory,
		"disableSearch":               false,
		"disableSelfHarmShortCircuit": false,
		"disableTextFollowUps":        false,
		"enableImageGeneration":       true,
		"enableImageStreaming":        true,
		"enableSideBySide":            true,
		"fileAttachments":             fileAttachments,
		"forceConcise":                false,
		"forceSideBySide":             false,
		"imageAttachments":            []any{},
		"imageGenerationCount":        2,
		"isAsyncChat":                 false,
		"message":                     message,
		"modeId":                      modeID.ApiStr(),
		"responseMetadata":            map[string]any{},
		"returnImageBytes":            false,
		"returnRawGrokInXaiRequest":   false,
		"searchAllConnectors":         false,
		"sendFinalMetadata":           true,
		"temporary":                   f.Temporary,
		"toolOverrides":               tools,
	}
	if f.CustomInstruction != "" {
		payload["customPersonality"] = f.CustomInstruction
	}
	if modelConfigOverride != nil {
		payload["responseMetadata"] = map[string]any{"modelConfigOverride": modelConfigOverride}
	}
	for k, v := range requestOverrides {
		if v == nil {
			continue
		}
		payload[k] = v
	}
	return payload
}

// BuildResponsePayload constructs the JSON payload for POST
// /rest/app-chat/conversations/{id}/responses. Unlike BuildChatPayload, this
// sends a follow-up message in an existing conversation and requires
// responseId pointing to the parent message to reply to.
func BuildResponsePayload(message string, responseID string, modeID model.ModeId, fileAttachments []string, toolOverrides map[string]any, requestOverrides map[string]any) map[string]any {
	base := BuildChatPayload(message, modeID, fileAttachments, toolOverrides, nil, requestOverrides)
	base["responseId"] = responseID
	return base
}
