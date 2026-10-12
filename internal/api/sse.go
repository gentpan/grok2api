package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/gentpan/grok2api/internal/grok"
)

// sseWriter writes Server-Sent Events frames to an HTTP response.
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

// newSSEWriter commits the response as an event stream (status 200 and the
// SSE headers). Call it only once the upstream is known to be usable: after
// this point the HTTP status can no longer change.
func newSSEWriter(w http.ResponseWriter) *sseWriter {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	return &sseWriter{w: w, flusher: flusher}
}

func (s *sseWriter) flush() {
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// writeData emits "data: <payload>\n\n".
func (s *sseWriter) writeData(payload string) {
	fmt.Fprintf(s.w, "data: %s\n\n", payload)
	s.flush()
}

// writeJSONData emits "data: <json>\n\n" with the JSON-encoded payload.
func (s *sseWriter) writeJSONData(v any) {
	b, _ := json.Marshal(v)
	s.writeData(string(b))
}

// writeEvent emits "event: <type>\ndata: <payload>\n\n".
func (s *sseWriter) writeEvent(eventType, payload string) {
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", eventType, payload)
	s.flush()
}

// writeEventJSON emits "event: <type>\ndata: <json>\n\n".
func (s *sseWriter) writeEventJSON(eventType string, v any) {
	b, _ := json.Marshal(v)
	s.writeEvent(eventType, string(b))
}

// writeComment emits ": <comment>\n\n" (a comment/heartbeat line).
func (s *sseWriter) writeComment(comment string) {
	fmt.Fprintf(s.w, ": %s\n\n", comment)
	s.flush()
}

// writeDone emits the terminal "data: [DONE]\n\n".
func (s *sseWriter) writeDone() {
	s.writeData("[DONE]")
}

// writeOpenAIError emits the OpenAI-style SSE error frame, then [DONE].
func (s *sseWriter) writeOpenAIError(message string, kind string, code string, param string) {
	errObj := map[string]any{"message": message, "type": kind, "code": code}
	if param != "" {
		errObj["param"] = param
	}
	// OpenAI streams errors as a plain data frame carrying {"error": {...}}.
	s.writeJSONData(map[string]any{"error": errObj})
	s.writeDone()
}

// writeAnthropicError emits the Anthropic SSE error event (no [DONE]).
func (s *sseWriter) writeAnthropicError(errType, message string) {
	s.writeEventJSON("error", map[string]any{
		"type":  "error",
		"error": map[string]any{"type": errType, "message": message},
	})
}

// --- reading ---

const (
	sseInitialBuffer = 64 * 1024
	sseMaxLine       = 4 * 1024 * 1024
)

// sseFrame is one data frame of an upstream event stream.
type sseFrame struct {
	// Event is the most recent "event:" name (console.x.ai streams); empty for
	// grok.com streams.
	Event string
	Data  string
}

// sseReader reads the data frames of an upstream SSE body. It is the single
// place that knows how to split and classify upstream lines; grok.com style
// (bare "data:" / JSON lines) and console style (event + data pairs) differ
// only in the classifier.
type sseReader struct {
	sc      *bufio.Scanner
	console bool
	event   string
}

func newSSEReader(r io.Reader, console bool) *sseReader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, sseInitialBuffer), sseMaxLine)
	return &sseReader{sc: sc, console: console}
}

// next returns the next data frame. It reports false at "[DONE]", at EOF or on
// a read error (see err).
func (r *sseReader) next() (sseFrame, bool) {
	for r.sc.Scan() {
		line := r.sc.Text()
		if r.console {
			kind, value := grok.ClassifyConsoleLine(line)
			switch kind {
			case "done":
				return sseFrame{}, false
			case "event":
				r.event = value
			case "data":
				return sseFrame{Event: r.event, Data: value}, true
			}
			continue
		}
		kind, data := grok.ClassifyLine(line)
		switch kind {
		case "done":
			return sseFrame{}, false
		case "data":
			return sseFrame{Data: data}, true
		}
	}
	return sseFrame{}, false
}

// err is the read error that ended the stream, if any.
func (r *sseReader) err() error { return r.sc.Err() }
