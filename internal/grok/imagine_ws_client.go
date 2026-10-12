package grok

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/websocket"
	"github.com/google/uuid"
)

// Limits that keep a failing or moderated prompt from being re-submitted (and
// billed) over and over.
const (
	// imagineRoundTimeout bounds one prompt round (reset + request + frames).
	imagineRoundTimeout = 120 * time.Second
	// imagineWriteTimeout bounds a single WS write.
	imagineWriteTimeout = 15 * time.Second
	// imagineHandshakeTimeout bounds the WS upgrade.
	imagineHandshakeTimeout = 30 * time.Second
	// imagineExtraRounds is how many prompt submissions beyond n are allowed in
	// total (n assumes the worst case of one image per round).
	imagineExtraRounds = 2
	// imagineMaxIdleRounds is how many consecutive rounds may yield no image.
	imagineMaxIdleRounds = 2
	// imagineMaxConnections / imagineMaxConnectFailures bound reconnects.
	imagineMaxConnections     = 3
	imagineMaxConnectFailures = 3
	// imagineBackoffBase is multiplied by the attempt number between connects.
	imagineBackoffBase = time.Second
	// imagineEventBuffer is the producer's channel buffer.
	imagineEventBuffer = 64
)

// ImagineStream handles the WS-based image generation protocol.
type ImagineStream struct {
	token    string
	proxyURL string
	url      string

	// Overridable for tests.
	roundTimeout time.Duration
	backoffBase  time.Duration
}

// NewImagineStream creates an ImagineStream for the given SSO token. It uses
// the same egress proxy as the HTTP transport.
func NewImagineStream(token string) *ImagineStream {
	return &ImagineStream{
		token:    token,
		proxyURL: egressProxyURL(),
		url:      wsImagineURL,

		roundTimeout: imagineRoundTimeout,
		backoffBase:  imagineBackoffBase,
	}
}

// StreamImagesContext connects to the WS imagine endpoint and sends events to
// the returned channel until n final images are collected, a bounded number of
// attempts is exhausted, or an error occurs. The channel is closed when the
// producer exits. Cancelling ctx closes the WebSocket and stops the producer;
// callers that stop reading the channel must cancel ctx.
func (s *ImagineStream) StreamImagesContext(ctx context.Context, prompt, aspectRatio string, n int, enableNSFW, enablePro bool) <-chan ImagineEvent {
	ch := make(chan ImagineEvent, imagineEventBuffer)
	run := &imagineRun{
		s: s, ctx: ctx, ch: ch,
		params: imagineParams{prompt: prompt, aspectRatio: aspectRatio, n: n, nsfw: enableNSFW, pro: enablePro},
	}
	go func() {
		defer close(ch)
		run.execute()
	}()
	return ch
}

// imagineParams is one generation request.
type imagineParams struct {
	prompt      string
	aspectRatio string
	n           int
	nsfw, pro   bool
}

// imagineRun is the connection-level state machine of one generation: it
// dials, serves prompt rounds on the connection and reconnects within the
// attempt budget.
type imagineRun struct {
	s      *ImagineStream
	ctx    context.Context
	ch     chan<- ImagineEvent
	params imagineParams

	collected  int
	rounds     int
	idleRounds int
	failed     bool // an error event was emitted (or the round reported one)
}

func (r *imagineRun) send(ev ImagineEvent) bool {
	select {
	case r.ch <- ev:
		return true
	case <-r.ctx.Done():
		return false
	}
}

func (r *imagineRun) fail(msg string) {
	r.failed = true
	r.send(ImagineEvent{Type: ImagineEventError, Error: msg})
}

// finish marks a stream that ended quietly with some, but not all, of the
// requested images, so a consumer can tell it from a complete one.
func (r *imagineRun) finish() {
	if r.failed || r.ctx.Err() != nil || r.collected == 0 || r.collected >= r.params.n {
		return
	}
	r.send(ImagineEvent{Type: ImagineEventPartial})
}

// execute runs until n images are collected or the budget is spent.
func (r *imagineRun) execute() {
	defer r.finish()
	connections, connectFailures := 0, 0
	maxRounds := r.params.n + imagineExtraRounds

	for r.collected < r.params.n {
		if r.ctx.Err() != nil {
			return
		}
		if connections >= imagineMaxConnections || r.rounds >= maxRounds {
			if r.collected == 0 {
				r.fail(fmt.Sprintf("image generation produced no image after %d attempt(s)", r.rounds))
			}
			return
		}
		if connections > 0 && !sleepCtx(r.ctx, time.Duration(connections)*r.s.backoffBase) {
			return
		}

		conn, err := r.s.dial(r.ctx)
		if err != nil {
			if r.ctx.Err() != nil {
				return
			}
			connectFailures++
			if connectFailures >= imagineMaxConnectFailures {
				r.fail(fmt.Sprintf("WS imagine connect failed: %v", err))
				return
			}
			if !sleepCtx(r.ctx, time.Duration(connectFailures)*r.s.backoffBase) {
				return
			}
			continue
		}
		connectFailures = 0
		connections++

		if r.serve(conn, maxRounds) {
			return
		}
	}
}

// serve runs prompt rounds on conn until the stream is finished (true) or the
// connection is unusable / the round budget is spent (false, the caller may
// reconnect). It owns the connection: it is always closed on return.
func (r *imagineRun) serve(conn *websocket.Conn, maxRounds int) (finished bool) {
	// Closing the conn is the only way to unblock a pending read.
	stopWatch := make(chan struct{})
	go func() {
		select {
		case <-r.ctx.Done():
			_ = conn.Close()
		case <-stopWatch:
		}
	}()
	defer func() {
		close(stopWatch)
		_ = conn.Close()
	}()

	for r.collected < r.params.n {
		r.rounds++
		res := r.s.runOneRound(r.ctx, conn, r.send, r.params, r.params.n-r.collected)
		r.collected += res.collected
		switch {
		case res.stop:
			r.failed = true // upstream error frame or ctx done; already reported
			return true
		case res.collected == 0 && res.moderated > 0 && !res.closed:
			// Every slot of the round was rejected: re-sending the same prompt
			// cannot help.
			if r.collected == 0 {
				r.fail("image generation was blocked by content moderation")
			}
			return true
		case res.collected == 0:
			r.idleRounds++
			if r.idleRounds >= imagineMaxIdleRounds {
				if r.collected == 0 {
					r.fail("image generation returned no image")
				}
				return true
			}
		default:
			r.idleRounds = 0
		}
		if res.closed || r.rounds >= maxRounds {
			return false
		}
	}
	return false
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (s *ImagineStream) dial(ctx context.Context) (*websocket.Conn, error) {
	dialer := &websocket.Dialer{HandshakeTimeout: imagineHandshakeTimeout}
	if s.proxyURL != "" {
		if proxyURL, err := url.Parse(s.proxyURL); err == nil {
			dialer.Proxy = fhttp.ProxyURL(proxyURL)
		}
	}
	conn, _, err := dialer.DialContext(ctx, s.url, resolveProxyProfile().imagineHeaders(s.token))
	return conn, err
}

// roundResult is the outcome of one prompt round.
type roundResult struct {
	collected int  // final images emitted this round
	moderated int  // slots rejected by moderation this round
	closed    bool // the connection is no longer usable
	stop      bool // an event was emitted that ends the stream (error / ctx done)
}

// runOneRound sends reset+prompt and processes frames until all slots
// complete, the round times out, or the WS closes. It never re-sends the
// prompt by itself.
func (s *ImagineStream) runOneRound(ctx context.Context, conn *websocket.Conn, send func(ImagineEvent) bool, p imagineParams, needed int) (res roundResult) {
	round := newImagineRound(needed)
	defer func() { res.collected, res.moderated = round.collected, round.moderated }()

	_ = conn.SetWriteDeadline(time.Now().Add(imagineWriteTimeout))
	if err := conn.WriteJSON(buildImagineResetMessage()); err != nil {
		res.closed = true
		return res
	}
	_ = conn.SetWriteDeadline(time.Now().Add(imagineWriteTimeout))
	if err := conn.WriteJSON(buildImagineRequestMessage(uuid.NewString(), p.prompt, p.aspectRatio, p.nsfw, p.pro)); err != nil {
		res.closed = true
		return res
	}
	// One deadline for the whole round. A read timeout permanently fails the
	// connection's read side, so it cannot be used as a poll interval.
	_ = conn.SetReadDeadline(time.Now().Add(s.roundTimeout))

	emit := func(events []ImagineEvent) bool {
		for _, ev := range events {
			if !send(ev) {
				return false
			}
		}
		return true
	}

	for {
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			res.closed = true
			if ctx.Err() != nil || !emit(round.flush()) {
				res.stop = true
			}
			return res
		}
		if msgType != websocket.TextMessage {
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		events, outcome := round.handleFrame(msg)
		if !emit(events) {
			res.stop = true
			return res
		}
		switch outcome {
		case frameRoundComplete:
			return res
		case frameUpstreamError:
			// An upstream error is final: re-sending the prompt would only
			// repeat (and be billed for) the same failing request.
			res.closed, res.stop = true, true
			return res
		}
	}
}

// --- round state -----------------------------------------------------------

// imagineSlot tracks one in-flight image generation slot.
type imagineSlot struct {
	imageID  string
	order    int
	width    int
	height   int
	lastBlob string
	lastURL  string
	done     bool
	progress int
}

type frameOutcome int

const (
	frameContinue      frameOutcome = iota // keep reading
	frameRoundComplete                     // every slot is done / enough images
	frameUpstreamError                     // the server reported a fatal error
)

// imagineRound is the pure state of one prompt round: it turns server frames
// into events without touching the socket.
type imagineRound struct {
	needed    int
	slots     map[string]*imagineSlot
	seq       []string // slot ids in creation order
	collected int      // final images produced (including flushed previews)
	moderated int      // slots rejected by moderation
}

func newImagineRound(needed int) *imagineRound {
	return &imagineRound{needed: needed, slots: map[string]*imagineSlot{}}
}

// handleFrame processes one decoded server frame.
func (r *imagineRound) handleFrame(msg map[string]any) ([]ImagineEvent, frameOutcome) {
	switch kind, _ := msg["type"].(string); kind {
	case "json":
		return r.onStatus(msg)
	case "image":
		return r.onImage(msg), frameContinue
	case "error":
		errMsg, _ := msg["err_msg"].(string)
		if errMsg == "" {
			errMsg = fmt.Sprintf("upstream imagine error: %v", msg)
		}
		return []ImagineEvent{{Type: ImagineEventError, Error: errMsg}}, frameUpstreamError
	}
	return nil, frameContinue
}

func (r *imagineRound) onStatus(msg map[string]any) ([]ImagineEvent, frameOutcome) {
	parsed := parseImagineJSONFrame(msg)
	if parsed == nil {
		return nil, frameContinue
	}
	switch parsed.Status {
	case "start_stage":
		if _, exists := r.slots[parsed.ImageID]; !exists {
			r.seq = append(r.seq, parsed.ImageID)
		}
		r.slots[parsed.ImageID] = &imagineSlot{
			imageID: parsed.ImageID, order: parsed.Order,
			width: parsed.Width, height: parsed.Height, progress: 10,
		}
		return []ImagineEvent{{
			Type: ImagineEventProgress, ImageID: parsed.ImageID, Order: parsed.Order, Progress: 10,
		}}, frameContinue

	case "completed":
		slot, ok := r.slots[parsed.ImageID]
		if !ok || slot.done {
			return nil, frameContinue
		}
		slot.done = true
		var ev ImagineEvent
		if parsed.Moderated {
			r.moderated++
			ev = ImagineEvent{Type: ImagineEventModerated, ImageID: parsed.ImageID, Order: slot.order, Moderated: true}
		} else {
			r.collected++
			ev = slot.finalEvent()
		}
		if r.collected >= r.needed || r.allDone() {
			return []ImagineEvent{ev}, frameRoundComplete
		}
		return []ImagineEvent{ev}, frameContinue
	}
	return nil, frameContinue
}

func (r *imagineRound) onImage(msg map[string]any) []ImagineEvent {
	urlStr, _ := msg["url"].(string)
	blob, _ := msg["blob"].(string)
	imageID, _, ok := parseImageURL(urlStr)
	if !ok {
		return nil
	}
	slot, ok := r.slots[imageID]
	if !ok || slot.done {
		return nil
	}
	slot.lastBlob = blob
	slot.lastURL = urlStr
	progress := 50
	if pf, ok := msg["percentage_complete"].(float64); ok {
		progress = int(pf)
	}
	progress = min(max(progress, 10), 99)
	if progress <= slot.progress {
		return nil
	}
	slot.progress = progress
	return []ImagineEvent{{Type: ImagineEventProgress, ImageID: imageID, Order: slot.order, Progress: progress}}
}

// flush emits the last preview of every unfinished slot as a final image, in
// slot order, and counts them as collected.
func (r *imagineRound) flush() []ImagineEvent {
	var pending []*imagineSlot
	for _, id := range r.seq {
		if slot := r.slots[id]; !slot.done && slot.lastBlob != "" {
			pending = append(pending, slot)
		}
	}
	sort.SliceStable(pending, func(i, j int) bool { return pending[i].order < pending[j].order })
	events := make([]ImagineEvent, 0, len(pending))
	for _, slot := range pending {
		slot.done = true
		r.collected++
		events = append(events, slot.finalEvent())
	}
	return events
}

func (r *imagineRound) allDone() bool {
	for _, s := range r.slots {
		if !s.done {
			return false
		}
	}
	return len(r.slots) > 0
}

func (s *imagineSlot) finalEvent() ImagineEvent {
	return ImagineEvent{
		Type: ImagineEventImage, ImageID: s.imageID, Order: s.order,
		URL: s.lastURL, Blob: s.lastBlob, Width: s.width, Height: s.height, IsFinal: true,
	}
}
