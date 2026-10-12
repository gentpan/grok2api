package grok

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
)

// SSE line classification kinds returned by ClassifyLine / ClassifyConsoleLine.
const (
	SSEKindData  = "data"
	SSEKindDone  = "done"
	SSEKindSkip  = "skip"
	SSEKindEvent = "event"
)

const (
	// sseInitialBuffer / sseMaxLine bound the scanner: upstream lines carry
	// whole JSON frames (and inline base64 images), so the default 64 KiB
	// token limit is far too small.
	sseInitialBuffer = 64 * 1024
	sseMaxLine       = 4 * 1024 * 1024
)

// ClassifyLine parses a raw line of a grok.com chat SSE stream and returns
// (kind, data). kind is one of SSEKindData, SSEKindDone, SSEKindSkip.
//
// The chat endpoints also emit bare JSON lines without a "data:" prefix; those
// are data too.
func ClassifyLine(line string) (kind, data string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return SSEKindSkip, ""
	}
	if payload, ok := strings.CutPrefix(line, "data:"); ok {
		payload = strings.TrimSpace(payload)
		if payload == "[DONE]" {
			return SSEKindDone, ""
		}
		return SSEKindData, payload
	}
	if strings.HasPrefix(line, "event:") {
		return SSEKindSkip, ""
	}
	if strings.HasPrefix(line, "{") {
		return SSEKindData, line
	}
	return SSEKindSkip, ""
}

// ClassifyConsoleLine parses a raw line of a console.x.ai (Responses API) SSE
// stream into (kind, value). kind is one of SSEKindEvent (value = event
// name), SSEKindData, SSEKindDone, SSEKindSkip. Unlike ClassifyLine, bare JSON
// lines are not data here.
func ClassifyConsoleLine(line string) (kind, value string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return SSEKindSkip, ""
	}
	if name, ok := strings.CutPrefix(line, "event:"); ok {
		return SSEKindEvent, strings.TrimSpace(name)
	}
	if payload, ok := strings.CutPrefix(line, "data:"); ok {
		payload = strings.TrimSpace(payload)
		if payload == "[DONE]" {
			return SSEKindDone, ""
		}
		return SSEKindData, payload
	}
	return SSEKindSkip, ""
}

// SSEDialect selects how ReadSSE classifies lines.
type SSEDialect int

const (
	// SSEChat is the grok.com app-chat dialect (ClassifyLine).
	SSEChat SSEDialect = iota
	// SSEConsole is the console.x.ai Responses dialect (ClassifyConsoleLine);
	// messages carry the preceding "event:" name.
	SSEConsole
)

// SSEMessage is one data record of a stream.
type SSEMessage struct {
	// Event is the SSE event name for SSEConsole streams ("" for SSEChat, or
	// when the record had none).
	Event string
	// Data is the payload after "data:".
	Data string
}

// ErrStopSSE may be returned by a ReadSSE callback to end the stream early
// without reporting an error.
var ErrStopSSE = errors.New("stop sse")

// ReadSSE reads a Server-Sent-Events body line by line and calls fn for every
// data record until the stream ends. It replaces the scanner → classify → feed
// loop each handler used to copy.
//
// It returns nil on EOF, on a "[DONE]" record and when fn returns ErrStopSSE.
// It returns fn's error as is, ctx.Err() once ctx is done, and the read error
// (including bufio.ErrTooLong for a line above 4 MiB) when the body breaks
// mid-stream; callers that treat their own request cancellation as a clean
// end should check ctx.Err() on a non-nil result.
func ReadSSE(ctx context.Context, r io.Reader, dialect SSEDialect, fn func(SSEMessage) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, sseInitialBuffer), sseMaxLine)
	event := ""
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := scanner.Text()
		var kind, value string
		if dialect == SSEConsole {
			kind, value = ClassifyConsoleLine(line)
		} else {
			kind, value = ClassifyLine(line)
		}
		switch kind {
		case SSEKindDone:
			return nil
		case SSEKindEvent:
			event = value
		case SSEKindData:
			err := fn(SSEMessage{Event: event, Data: value})
			event = ""
			if errors.Is(err, ErrStopSSE) {
				return nil
			}
			if err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return ctx.Err()
}
