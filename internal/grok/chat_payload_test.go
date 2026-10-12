package grok

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/gentpan/grok2api/internal/model"
)

// roundTrip normalises a payload through JSON so comparisons ignore Go types
// (int vs float64, []string vs []any) exactly like the wire would.
func roundTrip(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func parseJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

const chatPayloadGolden = `{
  "collectionIds": [],
  "disabledConnectorIds": [],
  "deviceEnvInfo": {"darkModeEnabled": false, "devicePixelRatio": 1, "screenHeight": 1080, "screenWidth": 1920, "viewportHeight": 945, "viewportWidth": 1920},
  "disableMemory": true,
  "disableSearch": false,
  "disableSelfHarmShortCircuit": false,
  "disableTextFollowUps": false,
  "enableImageGeneration": true,
  "enableImageStreaming": true,
  "enableSideBySide": true,
  "fileAttachments": [],
  "forceConcise": false,
  "forceSideBySide": false,
  "imageAttachments": [],
  "imageGenerationCount": 2,
  "isAsyncChat": false,
  "message": "hello",
  "modeId": "fast",
  "responseMetadata": {},
  "returnImageBytes": false,
  "returnRawGrokInXaiRequest": false,
  "searchAllConnectors": false,
  "sendFinalMetadata": true,
  "temporary": true,
  "toolOverrides": {"gmailSearch": false, "googleCalendarSearch": false, "outlookSearch": false, "outlookCalendarSearch": false, "googleDriveSearch": false}
}`

func TestBuildChatPayloadDefaultsSnapshot(t *testing.T) {
	useConfig(t, "")
	got := roundTrip(t, BuildChatPayload("hello", model.ModeFast, nil, nil, nil, nil))
	want := parseJSON(t, chatPayloadGolden)
	if !reflect.DeepEqual(got, want) {
		gb, _ := json.MarshalIndent(got, "", " ")
		t.Fatalf("payload mismatch:\n%s", gb)
	}
}

func TestBuildChatPayloadFeatureConfigAndOverrides(t *testing.T) {
	useConfig(t, "[features]\ntemporary = false\nmemory = true\ncustom_instruction = \"  be terse \"\n")
	got := roundTrip(t, BuildChatPayload("m", model.ModeExpert, []string{"f1", "f2"},
		map[string]any{"webSearch": true},
		map[string]any{"modelOverride": "x"},
		map[string]any{"forceConcise": true, "dropped": nil}))

	want := parseJSON(t, chatPayloadGolden)
	want["message"] = "m"
	want["modeId"] = "expert"
	want["temporary"] = false
	want["disableMemory"] = false
	want["customPersonality"] = "be terse"
	want["fileAttachments"] = []any{"f1", "f2"}
	want["toolOverrides"] = map[string]any{"webSearch": true}
	want["responseMetadata"] = map[string]any{"modelConfigOverride": map[string]any{"modelOverride": "x"}}
	want["forceConcise"] = true
	if !reflect.DeepEqual(got, want) {
		gb, _ := json.MarshalIndent(got, "", " ")
		t.Fatalf("payload mismatch:\n%s", gb)
	}
	if _, ok := got["dropped"]; ok {
		t.Fatal("nil override leaked into payload")
	}
}

func TestBuildResponsePayloadAddsParentResponseID(t *testing.T) {
	useConfig(t, "")
	got := roundTrip(t, BuildResponsePayload("again", "resp-7", model.ModeAuto, nil, nil, nil))
	if got["responseId"] != "resp-7" || got["message"] != "again" || got["modeId"] != "auto" {
		t.Fatalf("payload = %#v", got)
	}
	// A follow-up never carries a model config override.
	if !reflect.DeepEqual(got["responseMetadata"], map[string]any{}) {
		t.Fatalf("responseMetadata = %#v", got["responseMetadata"])
	}
}

func TestClassifyLine(t *testing.T) {
	cases := []struct {
		line, kind, data string
	}{
		{"", "skip", ""},
		{"   ", "skip", ""},
		{`data: {"a":1}`, "data", `{"a":1}`},
		{`data:{"a":1}`, "data", `{"a":1}`},
		{"data: [DONE]", "done", ""},
		{"event: message", "skip", ""},
		{`{"a":1}`, "data", `{"a":1}`},
		{": heartbeat", "skip", ""},
		{"garbage", "skip", ""},
	}
	for _, tc := range cases {
		kind, data := ClassifyLine(tc.line)
		if kind != tc.kind || data != tc.data {
			t.Errorf("ClassifyLine(%q) = %q,%q want %q,%q", tc.line, kind, data, tc.kind, tc.data)
		}
	}
}

func TestClassifyConsoleLine(t *testing.T) {
	cases := []struct {
		line, kind, value string
	}{
		{"", "skip", ""},
		{"event: response.output_text.delta", "event", "response.output_text.delta"},
		{"event:error", "event", "error"},
		{`data: {"delta":"x"}`, "data", `{"delta":"x"}`},
		{"data: [DONE]", "done", ""},
		{`{"raw":"json"}`, "skip", ""}, // unlike ClassifyLine, bare JSON is not data
		{": ping", "skip", ""},
	}
	for _, tc := range cases {
		kind, value := ClassifyConsoleLine(tc.line)
		if kind != tc.kind || value != tc.value {
			t.Errorf("ClassifyConsoleLine(%q) = %q,%q want %q,%q", tc.line, kind, value, tc.kind, tc.value)
		}
	}
}
