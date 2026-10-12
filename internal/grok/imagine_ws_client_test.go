package grok

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/websocket"
)

const testImageID = "abcdef12-0000-0000-0000-000000000001"

// imagineTestServer is a scripted stand-in for wss://grok.com/ws/imagine/listen.
type imagineTestServer struct {
	url         string
	connections atomic.Int32
	prompts     atomic.Int32
	closed      chan struct{}
}

// startImagineServer serves script once per received prompt. script returns
// true to keep the connection open for another prompt.
func startImagineServer(t *testing.T, script func(conn *websocket.Conn, prompt int) bool) *imagineTestServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &imagineTestServer{url: "ws://" + ln.Addr().String() + "/ws/imagine/listen", closed: make(chan struct{}, 16)}
	up := websocket.Upgrader{CheckOrigin: func(*fhttp.Request) bool { return true }}
	hs := &fhttp.Server{Handler: fhttp.HandlerFunc(func(w fhttp.ResponseWriter, r *fhttp.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() {
			conn.Close()
			srv.closed <- struct{}{}
		}()
		srv.connections.Add(1)
		for {
			var msg map[string]any
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			item, _ := msg["item"].(map[string]any)
			content, _ := item["content"].([]any)
			if len(content) == 0 {
				continue
			}
			first, _ := content[0].(map[string]any)
			if first["type"] != "input_text" {
				continue // reset message
			}
			n := int(srv.prompts.Add(1))
			if !script(conn, n) {
				return
			}
		}
	})}
	go func() { _ = hs.Serve(ln) }()
	t.Cleanup(func() { _ = hs.Close() })
	return srv
}

func newTestImagineStream(url string) *ImagineStream {
	s := NewImagineStream("test-token")
	s.proxyURL = ""
	s.url = url
	s.roundTimeout = 300 * time.Millisecond
	s.backoffBase = 10 * time.Millisecond
	return s
}

func collectImagine(t *testing.T, ch <-chan ImagineEvent, wait time.Duration) []ImagineEvent {
	t.Helper()
	var out []ImagineEvent
	deadline := time.After(wait)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			t.Fatalf("stream did not finish within %s; events so far: %#v", wait, out)
		}
	}
}

func TestImagineModeratedPromptIsNotResent(t *testing.T) {
	srv := startImagineServer(t, func(conn *websocket.Conn, _ int) bool {
		_ = conn.WriteJSON(map[string]any{"type": "json", "current_status": "start_stage", "image_id": testImageID, "order": 0})
		_ = conn.WriteJSON(map[string]any{"type": "json", "current_status": "completed", "image_id": testImageID, "moderated": true})
		return true
	})
	events := collectImagine(t, newTestImagineStream(srv.url).StreamImagesContext(context.Background(), "p", "1:1", 1, false, false), 5*time.Second)

	last := events[len(events)-1]
	if last.Type != ImagineEventError {
		t.Fatalf("last event = %#v, want moderation error", last)
	}
	if got := srv.prompts.Load(); got != 1 {
		t.Fatalf("prompt submitted %d times, want 1", got)
	}
}

func TestImagineUpstreamErrorFrameIsNotResent(t *testing.T) {
	srv := startImagineServer(t, func(conn *websocket.Conn, _ int) bool {
		_ = conn.WriteJSON(map[string]any{"type": "error", "err_msg": "boom"})
		return true
	})
	events := collectImagine(t, newTestImagineStream(srv.url).StreamImagesContext(context.Background(), "p", "1:1", 2, false, false), 5*time.Second)

	if len(events) != 1 || events[0].Type != ImagineEventError || events[0].Error != "boom" {
		t.Fatalf("events = %#v", events)
	}
	if srv.prompts.Load() != 1 || srv.connections.Load() != 1 {
		t.Fatalf("prompts=%d connections=%d, want 1/1", srv.prompts.Load(), srv.connections.Load())
	}
}

func TestImagineSilentUpstreamIsBounded(t *testing.T) {
	srv := startImagineServer(t, func(conn *websocket.Conn, _ int) bool {
		return true // accept the prompt, never answer
	})
	events := collectImagine(t, newTestImagineStream(srv.url).StreamImagesContext(context.Background(), "p", "1:1", 1, false, false), 10*time.Second)

	if len(events) != 1 || events[0].Type != ImagineEventError {
		t.Fatalf("events = %#v", events)
	}
	if got := srv.prompts.Load(); got > imagineMaxIdleRounds {
		t.Fatalf("prompt submitted %d times, want <= %d", got, imagineMaxIdleRounds)
	}
}

func TestImagineConnectFailuresAreBounded(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // nothing listens here any more

	events := collectImagine(t, newTestImagineStream("ws://"+addr+"/x").StreamImagesContext(context.Background(), "p", "1:1", 1, false, false), 10*time.Second)
	if len(events) != 1 || events[0].Type != ImagineEventError {
		t.Fatalf("events = %#v", events)
	}
}

func TestImagineSuccess(t *testing.T) {
	srv := startImagineServer(t, func(conn *websocket.Conn, _ int) bool {
		_ = conn.WriteJSON(map[string]any{"type": "json", "current_status": "start_stage", "image_id": testImageID, "order": 0})
		_ = conn.WriteJSON(map[string]any{"type": "image", "url": "/images/" + testImageID + ".png", "blob": "QUJD", "percentage_complete": 80})
		_ = conn.WriteJSON(map[string]any{"type": "json", "current_status": "completed", "image_id": testImageID})
		return true
	})
	events := collectImagine(t, newTestImagineStream(srv.url).StreamImagesContext(context.Background(), "p", "1:1", 1, false, false), 5*time.Second)

	images := 0
	for _, ev := range events {
		switch ev.Type {
		case ImagineEventImage:
			images++
			if ev.Blob != "QUJD" || !ev.IsFinal {
				t.Fatalf("image event = %#v", ev)
			}
		case ImagineEventError:
			t.Fatalf("unexpected error event %#v", ev)
		}
	}
	if images != 1 || srv.prompts.Load() != 1 {
		t.Fatalf("images=%d prompts=%d", images, srv.prompts.Load())
	}
}

func TestImagineContextCancelClosesConnectionAndStops(t *testing.T) {
	srv := startImagineServer(t, func(conn *websocket.Conn, _ int) bool {
		_ = conn.WriteJSON(map[string]any{"type": "json", "current_status": "start_stage", "image_id": testImageID, "order": 0})
		return true
	})
	s := newTestImagineStream(srv.url)
	s.roundTimeout = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	ch := s.StreamImagesContext(ctx, "p", "1:1", 1, false, false)

	select {
	case ev := <-ch:
		if ev.Type != ImagineEventProgress {
			t.Fatalf("first event = %#v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no progress event")
	}
	cancel()

	deadline := time.After(5 * time.Second)
	for open := true; open; {
		select {
		case _, open = <-ch:
		case <-deadline:
			t.Fatal("producer did not stop after ctx cancel")
		}
	}
	select {
	case <-srv.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("server connection was not closed after ctx cancel")
	}
}

func TestImagineStalledConsumerDoesNotLeakProducer(t *testing.T) {
	// Many progress events, nobody reads: the producer must stop on ctx cancel
	// instead of blocking forever on the channel.
	srv := startImagineServer(t, func(conn *websocket.Conn, _ int) bool {
		_ = conn.WriteJSON(map[string]any{"type": "json", "current_status": "start_stage", "image_id": testImageID, "order": 0})
		for i := 11; i < 100; i++ {
			_ = conn.WriteJSON(map[string]any{"type": "image", "url": "/images/" + testImageID + ".png", "blob": "QQ==", "percentage_complete": i})
		}
		return true
	})
	s := newTestImagineStream(srv.url)
	s.roundTimeout = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	ch := s.StreamImagesContext(ctx, "p", "1:1", 1, false, false)
	time.Sleep(300 * time.Millisecond) // let the 64-slot buffer fill
	cancel()

	deadline := time.After(5 * time.Second)
	for open := true; open; {
		select {
		case _, open = <-ch:
		case <-deadline:
			t.Fatal("producer stayed blocked after ctx cancel")
		}
	}
}

func TestImaginePartialResultIsMarked(t *testing.T) {
	// The server delivers one image for the first prompt and stays silent
	// afterwards: the stream asked for two, so it must end with the image and
	// an explicit partial marker (not an error, which would discard the image).
	srv := startImagineServer(t, func(conn *websocket.Conn, prompt int) bool {
		if prompt == 1 {
			_ = conn.WriteJSON(map[string]any{"type": "json", "current_status": "start_stage", "image_id": testImageID, "order": 0})
			_ = conn.WriteJSON(map[string]any{"type": "image", "url": "/images/" + testImageID + ".png", "blob": "QUJD", "percentage_complete": 80})
			_ = conn.WriteJSON(map[string]any{"type": "json", "current_status": "completed", "image_id": testImageID})
		}
		return true
	})
	events := collectImagine(t, newTestImagineStream(srv.url).StreamImagesContext(context.Background(), "p", "1:1", 2, false, false), 15*time.Second)

	var images, partials, errs int
	for _, ev := range events {
		switch ev.Type {
		case ImagineEventImage:
			images++
		case ImagineEventPartial:
			partials++
		case ImagineEventError:
			errs++
		}
	}
	if images != 1 || partials != 1 || errs != 0 {
		t.Fatalf("images=%d partials=%d errors=%d events=%#v", images, partials, errs, events)
	}
	if last := events[len(events)-1]; last.Type != ImagineEventPartial {
		t.Fatalf("partial marker is not last: %#v", last)
	}
}

func TestImagineCompleteResultHasNoPartialMarker(t *testing.T) {
	srv := startImagineServer(t, func(conn *websocket.Conn, _ int) bool {
		_ = conn.WriteJSON(map[string]any{"type": "json", "current_status": "start_stage", "image_id": testImageID, "order": 0})
		_ = conn.WriteJSON(map[string]any{"type": "image", "url": "/images/" + testImageID + ".png", "blob": "QUJD", "percentage_complete": 80})
		_ = conn.WriteJSON(map[string]any{"type": "json", "current_status": "completed", "image_id": testImageID})
		return true
	})
	for _, ev := range collectImagine(t, newTestImagineStream(srv.url).StreamImagesContext(context.Background(), "p", "1:1", 1, false, false), 5*time.Second) {
		if ev.Type == ImagineEventPartial || ev.Type == ImagineEventError {
			t.Fatalf("unexpected %#v", ev)
		}
	}
}

func TestImagineFlushesPreviewsOnConnectionDropInOrder(t *testing.T) {
	ids := []string{
		"cccc0000-0000-0000-0000-000000000000",
		"aaaa0000-0000-0000-0000-000000000000",
		"bbbb0000-0000-0000-0000-000000000000",
	}
	orders := map[string]int{ids[0]: 2, ids[1]: 0, ids[2]: 1}
	srv := startImagineServer(t, func(conn *websocket.Conn, _ int) bool {
		for _, id := range ids {
			_ = conn.WriteJSON(map[string]any{"type": "json", "current_status": "start_stage", "image_id": id, "order": orders[id]})
		}
		for _, id := range ids {
			_ = conn.WriteJSON(map[string]any{"type": "image", "url": "/images/" + id + ".png", "blob": "blob-" + id[:1], "percentage_complete": 60})
		}
		return false // drop the connection with previews pending
	})
	events := collectImagine(t, newTestImagineStream(srv.url).StreamImagesContext(context.Background(), "p", "1:1", 3, false, false), 15*time.Second)
	var finals []ImagineEvent
	for _, ev := range events {
		if ev.Type == ImagineEventImage {
			finals = append(finals, ev)
		}
	}
	if len(finals) < 3 || finals[0].Order != 0 || finals[1].Order != 1 || finals[2].Order != 2 {
		t.Fatalf("flushed images out of order: %#v", finals)
	}
}
