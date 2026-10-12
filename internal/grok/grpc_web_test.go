package grok

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"reflect"
	"testing"
)

func trailerFrame(text string) []byte {
	payload := []byte(text)
	out := []byte{0x80, 0, 0, 0, byte(len(payload))}
	return append(out, payload...)
}

func TestEncodeGRPCWebPayload(t *testing.T) {
	got := encodeGRPCWebPayload([]byte{0x10, 0x01})
	want := []byte{0x00, 0x00, 0x00, 0x00, 0x02, 0x10, 0x01}
	if !bytes.Equal(got, want) {
		t.Fatalf("frame = %x, want %x", got, want)
	}
	if !bytes.Equal(buildAcceptTOSPayload(), want) {
		t.Fatalf("accept tos = %x", buildAcceptTOSPayload())
	}
}

func TestBuildNSFWMgmtPayloadWireLayout(t *testing.T) {
	name := "always_show_nsfw_content"
	on := buildNSFWMgmtPayload(true)
	off := buildNSFWMgmtPayload(false)
	if on[0] != 0 || len(on) != len(off) {
		t.Fatalf("frames: %x / %x", on, off)
	}
	// protobuf = 0a 02 10 <bool> 12 <len> 0a <len(name)> name
	body := on[5:]
	wantHead := []byte{0x0a, 0x02, 0x10, 0x01, 0x12, byte(2 + len(name)), 0x0a, byte(len(name))}
	if !bytes.HasPrefix(body, wantHead) || string(body[len(wantHead):]) != name {
		t.Fatalf("body = %x", body)
	}
	if off[5+3] != 0x00 {
		t.Fatalf("disable payload bool byte = %x", off[5+3])
	}
}

func TestParseGRPCWebResponseMessagesAndTrailers(t *testing.T) {
	var body []byte
	body = append(body, encodeGRPCWebPayload([]byte("one"))...)
	body = append(body, encodeGRPCWebPayload([]byte("two"))...)
	body = append(body, trailerFrame("grpc-status: 0\r\ngrpc-message: all%20good\r\n")...)

	msgs, trailers := parseGRPCWebResponse(body, "application/grpc-web+proto", nil)
	if len(msgs) != 2 || string(msgs[0]) != "one" || string(msgs[1]) != "two" {
		t.Fatalf("messages = %q", msgs)
	}
	if trailers["grpc-status"] != "0" || trailers["grpc-message"] != "all good" {
		t.Fatalf("trailers = %#v", trailers)
	}
	st := getGRPCStatus(trailers)
	if !st.OK() || st.Message != "all good" {
		t.Fatalf("status = %#v", st)
	}
}

func TestParseGRPCWebResponseBase64TextBody(t *testing.T) {
	var raw []byte
	raw = append(raw, encodeGRPCWebPayload([]byte("msg"))...)
	raw = append(raw, trailerFrame("grpc-status: 7\r\ngrpc-message: denied\r\n")...)
	enc := base64.StdEncoding.EncodeToString(raw)

	for _, ct := range []string{"application/grpc-web-text", "application/grpc-web+proto"} {
		msgs, trailers := parseGRPCWebResponse([]byte(enc), ct, nil)
		if len(msgs) != 1 || string(msgs[0]) != "msg" || trailers["grpc-status"] != "7" {
			t.Fatalf("ct=%s msgs=%q trailers=%#v", ct, msgs, trailers)
		}
	}
}

func TestParseGRPCWebResponseHeaderFallbackAndTruncation(t *testing.T) {
	h := http.Header{}
	h.Set("Grpc-Status", "8")
	h.Set("Grpc-Message", "quota")
	msgs, trailers := parseGRPCWebResponse(nil, "application/grpc-web+proto", h)
	if len(msgs) != 0 || trailers["grpc-status"] != "8" || trailers["grpc-message"] != "quota" {
		t.Fatalf("msgs=%q trailers=%#v", msgs, trailers)
	}
	// A truncated frame is ignored rather than panicking.
	bad := []byte{0x00, 0x00, 0x00, 0x00, 0x09, 'a', 'b'}
	msgs, _ = parseGRPCWebResponse(bad, "application/grpc-web+proto", nil)
	if len(msgs) != 0 {
		t.Fatalf("truncated frame yielded %q", msgs)
	}
}

func TestGetGRPCStatusAndHTTPEquivalents(t *testing.T) {
	if st := getGRPCStatus(map[string]string{}); st.Code != -1 {
		t.Fatalf("missing status = %#v", st)
	}
	cases := map[int]int{0: 200, 4: 504, 7: 403, 8: 429, 14: 503, 16: 401, 2: 502, 99: 502}
	for code, want := range cases {
		if got := (GRPCStatus{Code: code}).HTTPEquiv(); got != want {
			t.Errorf("HTTPEquiv(%d) = %d, want %d", code, got, want)
		}
	}
}

func TestUnquoteGRPCMessage(t *testing.T) {
	cases := map[string]string{
		"":              "",
		"plain":         "plain",
		"a%20b":         "a b",
		"%E4%B8%AD":     "中",
		"bad%zzescape":  "bad%zzescape",
		"trailing%2":    "trailing%2",
		"100%25 sure":   "100% sure",
		"lower%2fcase":  "lower/case",
		"UPPER%2FCASE%": "UPPER/CASE%",
	}
	for in, want := range cases {
		if got := unquoteGRPCMessage(in); got != want {
			t.Errorf("unquote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseDataURI(t *testing.T) {
	name, b64, mime, err := parseDataURI("data:image/png;base64,QUJD\nREVG")
	if err != nil || name != "file.png" || b64 != "QUJDREVG" || mime != "image/png" {
		t.Fatalf("got %q %q %q %v", name, b64, mime, err)
	}
	for _, bad := range []string{"https://x/y", "data:image/png;base64", "data:image/png,QUJD", "data:image/png;base64,  "} {
		if _, _, _, err := parseDataURI(bad); err == nil {
			t.Errorf("parseDataURI(%q) accepted", bad)
		}
	}
	if got := inferContentType("https://h/a/b.JPG?x=1"); got != "image/jpeg" {
		t.Fatalf("inferContentType = %q", got)
	}
	if got := inferContentType("https://h/a/b.exe"); got != "" {
		t.Fatalf("inferContentType exe = %q", got)
	}
}

func TestParseRateLimits(t *testing.T) {
	q := parseRateLimits(map[string]any{"remainingQueries": 5.0, "totalQueries": 20.0, "windowSizeSeconds": 3600.0}, 7200)
	if q == nil || q.Remaining != 5 || q.Total != 20 || q.WindowSec != 3600 || q.ResetAtMs == nil {
		t.Fatalf("quota = %#v", q)
	}
	q = parseRateLimits(map[string]any{"remainingQueries": 3.0}, 7200)
	if q == nil || q.Total != 3 || q.WindowSec != 7200 {
		t.Fatalf("defaults = %#v", q)
	}
	if parseRateLimits(map[string]any{"totalQueries": 3.0}, 7200) != nil {
		t.Fatal("missing remainingQueries must yield nil")
	}
	_ = reflect.TypeOf(q)
}
