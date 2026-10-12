package grok

import (
	"context"
	"testing"
	"time"
)

func TestConversationTrackerSetGetClear(t *testing.T) {
	ct := NewConversationTracker(time.Minute)
	if ct.Get("tok") != nil {
		t.Fatal("empty tracker returned a state")
	}
	ct.Set("tok", "conv", "resp")
	st := ct.Get("tok")
	if st == nil || st.ConversationID != "conv" || st.LastResponseID != "resp" {
		t.Fatalf("state = %#v", st)
	}
	ct.Set("tok", "conv", "resp2")
	if got := ct.Get("tok"); got == nil || got.LastResponseID != "resp2" {
		t.Fatalf("state after update = %#v", got)
	}
	ct.Clear("tok")
	if ct.Get("tok") != nil {
		t.Fatal("state survived Clear")
	}
}

func TestConversationTrackerExpiry(t *testing.T) {
	ct := NewConversationTracker(40 * time.Millisecond)
	ct.Set("tok", "c", "r")
	time.Sleep(80 * time.Millisecond)
	if ct.Get("tok") != nil {
		t.Fatal("stale entry returned")
	}
	ct.evictStale()
	ct.mu.Lock()
	n := len(ct.conv)
	ct.mu.Unlock()
	if n != 0 {
		t.Fatalf("evictStale left %d entries", n)
	}
}

func TestParseImagineJSONFrame(t *testing.T) {
	if parseImagineJSONFrame(map[string]any{"current_status": "progress", "image_id": "a"}) != nil {
		t.Fatal("unknown status parsed")
	}
	if parseImagineJSONFrame(map[string]any{"current_status": "completed"}) != nil {
		t.Fatal("frame without id parsed")
	}
	got := parseImagineJSONFrame(map[string]any{
		"current_status": "completed", "job_id": "job-1", "order": 2.0, "width": 640.0, "height": 480.0,
		"moderated": true, "r_rated": true,
	})
	if got == nil || got.ImageID != "job-1" || got.Order != 2 || got.Width != 640 || got.Height != 480 || !got.Moderated || !got.RRated {
		t.Fatalf("parsed = %#v", got)
	}
}

func TestParseImageURLMatches(t *testing.T) {
	id, ext, ok := parseImageURL("https://assets.grok.com/images/abcdef12-0000-4000-8000-000000000001.png?x=1")
	if !ok || id != "abcdef12-0000-4000-8000-000000000001" || ext != "png" {
		t.Fatalf("id=%q ext=%q", id, ext)
	}
}

func TestResolveAspectRatioAndModelPredicates(t *testing.T) {
	for in, want := range map[string]string{"1280x720": "16:9", "9:16": "9:16", "1024x1024": "1:1", "weird": "2:3", "": "2:3"} {
		if got := ResolveAspectRatio(in); got != want {
			t.Errorf("ResolveAspectRatio(%q) = %q, want %q", in, got, want)
		}
	}
	if !IsWSImageModel("grok-imagine-image") || !IsWSImageModel("grok-imagine-image-pro") || IsWSImageModel("grok-4") {
		t.Fatal("IsWSImageModel")
	}
	if !IsProImageModel("grok-imagine-image-pro") || IsProImageModel("grok-imagine-image") {
		t.Fatal("IsProImageModel")
	}
	if got := FormatImageProgress("A", 50, 1, 2); got != "A正在生成 50% (1/2)" {
		t.Fatalf("FormatImageProgress = %q", got)
	}
	if got := FormatImageProgress("", 50, -1, 0); got != "正在生成 50%" {
		t.Fatalf("FormatImageProgress = %q", got)
	}
}

func TestDownloadConsoleVideoRejectsUntrustedURLs(t *testing.T) {
	tr := newTestTransport(t)
	for _, raw := range []string{
		"http://vidgen.x.ai/v.mp4",            // not https
		"https://evil.example/v.mp4",          // wrong host
		"https://vidgen.x.ai.evil.test/v.mp4", // suffix trick
		"https://user:pw@vidgen.x.ai/v.mp4",   // userinfo
		"://bad",
	} {
		if rc, err := tr.DownloadConsoleVideo(context.Background(), raw); err == nil {
			rc.Close()
			t.Errorf("DownloadConsoleVideo(%q) accepted", raw)
		}
	}
}

func TestTrustedConsoleVideoHost(t *testing.T) {
	for host, want := range map[string]bool{
		"vidgen.x.ai": true, "a.vidgen.x.ai": true, "VIDGEN.X.AI": true,
		"x.ai": false, "evilvidgen.x.ai": false, "vidgen.x.ai.evil": false,
	} {
		if got := trustedConsoleVideoHost(host); got != want {
			t.Errorf("trustedConsoleVideoHost(%q) = %v, want %v", host, got, want)
		}
	}
}
