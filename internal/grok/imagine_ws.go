// WebSocket image generation protocol for wss://grok.com/ws/imagine/listen:
// client messages, server frame parsers, event types and model predicates.
// The connection and retry logic lives in imagine_ws_client.go.
package grok

import (
	"fmt"
	"regexp"
	"time"
)

const (
	// wsImagineURL is the WebSocket endpoint for image generation.
	wsImagineURL = "wss://grok.com/ws/imagine/listen"

	// ImageBaseURL is the CDN prefix for generated images (with trailing slash).
	ImageBaseURL = AssetsCDN + "/"
)

var imageURLPattern = regexp.MustCompile(`/images/([a-f0-9\-]+)\.(png|jpg|jpeg)`)

// aspectRatios maps OpenAI size strings to Grok aspect ratios.
var aspectRatios = map[string]string{
	"1280x720":  "16:9",
	"16:9":      "16:9",
	"720x1280":  "9:16",
	"9:16":      "9:16",
	"1792x1024": "3:2",
	"3:2":       "3:2",
	"1024x1792": "2:3",
	"2:3":       "2:3",
	"1024x1024": "1:1",
	"1:1":       "1:1",
}

// defaultAspectRatio is used for sizes not in aspectRatios.
const defaultAspectRatio = "2:3"

// ResolveAspectRatio converts an OpenAI size to a Grok aspect ratio.
func ResolveAspectRatio(size string) string {
	if ar, ok := aspectRatios[size]; ok {
		return ar
	}
	return defaultAspectRatio
}

// --- client messages ---

// imagineMessage wraps content in the conversation.item.create envelope.
func imagineMessage(content map[string]any) map[string]any {
	return map[string]any{
		"type":      "conversation.item.create",
		"timestamp": time.Now().UnixMilli(),
		"item": map[string]any{
			"type":    "message",
			"content": []any{content},
		},
	}
}

// buildImagineResetMessage builds the reset message sent before each prompt.
func buildImagineResetMessage() map[string]any {
	return imagineMessage(map[string]any{"type": "reset"})
}

// buildImagineRequestMessage builds the image generation request message.
func buildImagineRequestMessage(requestID, prompt, aspectRatio string, enableNSFW, enablePro bool) map[string]any {
	return imagineMessage(map[string]any{
		"requestId": requestID,
		"text":      prompt,
		"type":      "input_text",
		"properties": map[string]any{
			"section_count":       0,
			"is_kids_mode":        false,
			"enable_nsfw":         enableNSFW,
			"skip_upsampler":      false,
			"enable_side_by_side": true,
			"is_initial":          false,
			"aspect_ratio":        aspectRatio,
			"enable_pro":          enablePro,
		},
	})
}

// --- server frame parsers ---

// parseImageURL extracts (imageID, ext) from a /images/{id}.{ext} URL. ok is
// false when the URL carries no image id; the caller must not invent one (a
// made-up id can never match a known slot).
func parseImageURL(rawURL string) (id, ext string, ok bool) {
	m := imageURLPattern.FindStringSubmatch(rawURL)
	if len(m) >= 3 {
		return m[1], m[2], true
	}
	return "", "", false
}

// imagineSlotStatus represents the status of an image generation slot.
type imagineSlotStatus struct {
	Status    string
	ImageID   string
	Order     int
	Width     int
	Height    int
	Moderated bool
	RRated    bool
}

// parseImagineJSONFrame parses a {type:"json"} server frame. It returns nil
// for statuses the stream does not act on.
func parseImagineJSONFrame(msg map[string]any) *imagineSlotStatus {
	status, _ := msg["current_status"].(string)
	if status != "start_stage" && status != "completed" {
		return nil
	}
	imageID, _ := msg["image_id"].(string)
	if imageID == "" {
		imageID, _ = msg["job_id"].(string)
	}
	if imageID == "" {
		return nil
	}
	num := func(key string) int {
		v, _ := msg[key].(float64)
		return int(v)
	}
	return &imagineSlotStatus{
		Status:    status,
		ImageID:   imageID,
		Order:     num("order"),
		Width:     num("width"),
		Height:    num("height"),
		Moderated: msg["moderated"] == true,
		RRated:    msg["r_rated"] == true,
	}
}

// ImagineEventKind enumerates the event types from the WS imagine stream.
type ImagineEventKind string

const (
	ImagineEventImage     ImagineEventKind = "image"
	ImagineEventProgress  ImagineEventKind = "progress"
	ImagineEventModerated ImagineEventKind = "moderated"
	ImagineEventError     ImagineEventKind = "error"
	// ImagineEventPartial is the last event of a stream that ended with fewer
	// images than requested (attempt budget exhausted) but at least one image.
	// Error is empty; consumers may ignore it.
	ImagineEventPartial ImagineEventKind = "partial"
)

// ImagineEvent is one event from the WS image generation stream.
type ImagineEvent struct {
	Type      ImagineEventKind
	ImageID   string
	Order     int
	Progress  int
	Blob      string
	URL       string
	Width     int
	Height    int
	IsFinal   bool
	Moderated bool
	Error     string
}

// IsWSImageModel returns true if the model should use WS-based generation.
func IsWSImageModel(modelName string) bool {
	return modelName == "grok-imagine-image" || modelName == "grok-imagine-image-pro"
}

// IsProImageModel returns true if the model uses quality (pro) mode.
func IsProImageModel(modelName string) bool {
	return modelName == "grok-imagine-image-pro"
}

// FormatImageProgress formats a progress string for reasoning_content.
func FormatImageProgress(label string, progress int, completed, total int) string {
	if completed >= 0 && total > 0 {
		return fmt.Sprintf("%s正在生成 %d%% (%d/%d)", label, progress, completed, total)
	}
	return fmt.Sprintf("%s正在生成 %d%%", label, progress)
}
