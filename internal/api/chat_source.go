package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gentpan/grok2api/internal/account"
	"github.com/gentpan/grok2api/internal/grok"
	"github.com/gentpan/grok2api/internal/model"
	"github.com/gentpan/grok2api/internal/platform"
)

// chatAttemptTimeout bounds one upstream chat attempt.
const chatAttemptTimeout = 5 * time.Minute

// imageAttemptTimeout bounds one WebSocket image generation attempt.
const imageAttemptTimeout = 10 * time.Minute

// chatEventKind is a backend-neutral unit of model output.
type chatEventKind int

const (
	evText          chatEventKind = iota + 1 // answer text
	evThinking                               // reasoning text
	evImage                                  // a finished image (Content = URL)
	evImageProgress                          // image generation percent (Content = "42")
)

type chatEvent struct {
	Kind    chatEventKind
	Content string
	ImageID string
}

// chatMeta is what a source knows once it has ended.
type chatMeta struct {
	ConversationID string
	ResponseID     string
	// Usage is the upstream token usage object, when the backend reports one.
	Usage map[string]any
	// ImageURLs is every image URL the upstream announced (web backend).
	ImageURLs [][2]string
	// TrimThinking asks aggregators to drop a trailing newline of the
	// reasoning text (image progress lines carry their own).
	TrimThinking bool
}

// chatSource is an opened upstream response, whatever the backend.
type chatSource interface {
	// Next returns the next batch of events. io.EOF marks a clean end; any
	// other error is a failure of the upstream (or a cancelled request).
	Next() ([]chatEvent, error)
	// Meta is valid once Next returned io.EOF.
	Meta() chatMeta
	Close()
}

// ctxError turns a finished context into the error to report.
func ctxError(ctx context.Context) error {
	err := ctx.Err()
	if errors.Is(err, context.DeadlineExceeded) {
		return platform.UpstreamError("upstream request timed out", 504, "")
	}
	return err
}

// --- web (grok.com app-chat) ---

type webSource struct {
	ctx  context.Context
	body io.ReadCloser
	rd   *sseReader
	ad   *grok.StreamAdapter
}

func newWebSource(ctx context.Context, body io.ReadCloser) *webSource {
	return &webSource{ctx: ctx, body: body, rd: newSSEReader(body, false), ad: grok.NewStreamAdapter()}
}

func (w *webSource) Next() ([]chatEvent, error) {
	for {
		f, ok := w.rd.next()
		if !ok {
			if err := w.rd.err(); err != nil {
				if w.ctx.Err() != nil {
					return nil, ctxError(w.ctx)
				}
				return nil, platform.UpstreamError("upstream stream interrupted: "+err.Error(), 502, "")
			}
			if w.ctx.Err() != nil {
				return nil, ctxError(w.ctx)
			}
			return nil, io.EOF
		}
		frames, perr := w.ad.Feed([]byte(f.Data))
		if perr != nil {
			return nil, perr
		}
		var out []chatEvent
		for _, ev := range frames {
			switch ev.Kind {
			case grok.EventText:
				out = append(out, chatEvent{Kind: evText, Content: ev.Content})
			case grok.EventThinking:
				out = append(out, chatEvent{Kind: evThinking, Content: ev.Content})
			case grok.EventImage:
				out = append(out, chatEvent{Kind: evImage, Content: ev.Content, ImageID: ev.ImageID})
			case grok.EventImageProgress:
				out = append(out, chatEvent{Kind: evImageProgress, Content: ev.Content, ImageID: ev.ImageID})
			}
		}
		if len(out) > 0 {
			return out, nil
		}
	}
}

func (w *webSource) Meta() chatMeta {
	return chatMeta{ConversationID: w.ad.ConversationID, ResponseID: w.ad.LastResponseID, ImageURLs: w.ad.ImageURLs}
}

func (w *webSource) Close() { _ = w.body.Close() }

// --- console.x.ai ---

type consoleSource struct {
	ctx  context.Context
	body io.ReadCloser
	rd   *sseReader
	ad   *grok.ConsoleStreamAdapter
}

func newConsoleSource(ctx context.Context, body io.ReadCloser) *consoleSource {
	return &consoleSource{ctx: ctx, body: body, rd: newSSEReader(body, true), ad: grok.NewConsoleStreamAdapter()}
}

func (s *consoleSource) Next() ([]chatEvent, error) {
	for {
		f, ok := s.rd.next()
		if !ok {
			if err := s.rd.err(); err != nil {
				if s.ctx.Err() != nil {
					return nil, ctxError(s.ctx)
				}
				return nil, platform.UpstreamError("upstream stream interrupted: "+err.Error(), 502, "")
			}
			if s.ctx.Err() != nil {
				return nil, ctxError(s.ctx)
			}
			return nil, io.EOF
		}
		tokens, perr := s.ad.Feed(f.Event, f.Data)
		if perr != nil {
			return nil, perr
		}
		if len(tokens) == 0 {
			continue
		}
		out := make([]chatEvent, 0, len(tokens))
		for _, t := range tokens {
			out = append(out, chatEvent{Kind: evText, Content: t})
		}
		return out, nil
	}
}

func (s *consoleSource) Meta() chatMeta { return chatMeta{Usage: s.ad.Usage} }
func (s *consoleSource) Close()         { _ = s.body.Close() }

// --- grok-4.6 WebSocket gateway ---

type gatewaySource struct {
	ctx     context.Context
	ch      <-chan grok.GatewayEvent
	pending *grok.GatewayEvent
	closed  bool
	convID  string
	respID  string
}

// openGatewaySource starts the gateway turn and waits for its first event, so
// that connect / handshake failures surface before any response byte is
// written and the request can still fail over to another account.
func openGatewaySource(ctx context.Context, ch <-chan grok.GatewayEvent) (*gatewaySource, error) {
	g := &gatewaySource{ctx: ctx, ch: ch}
	select {
	case ev, ok := <-ch:
		if !ok {
			g.closed = true
			return g, nil
		}
		if ev.Err != nil {
			return nil, ev.Err
		}
		g.pending = &ev
	case <-ctx.Done():
		return nil, ctxError(ctx)
	}
	return g, nil
}

func (g *gatewaySource) Next() ([]chatEvent, error) {
	for {
		var ev grok.GatewayEvent
		switch {
		case g.pending != nil:
			ev = *g.pending
			g.pending = nil
		case g.closed:
			return nil, io.EOF
		default:
			select {
			case e, ok := <-g.ch:
				if !ok {
					g.closed = true
					if g.ctx.Err() != nil {
						return nil, ctxError(g.ctx)
					}
					return nil, io.EOF
				}
				ev = e
			case <-g.ctx.Done():
				return nil, ctxError(g.ctx)
			}
		}
		if ev.Err != nil {
			return nil, ev.Err
		}
		if ev.ConversationID != "" {
			g.convID = ev.ConversationID
		}
		if ev.ResponseID != "" {
			g.respID = ev.ResponseID
		}
		switch ev.Kind {
		case grok.EventText:
			return []chatEvent{{Kind: evText, Content: ev.Content}}, nil
		case grok.EventThinking:
			return []chatEvent{{Kind: evThinking, Content: ev.Content}}, nil
		}
	}
}

func (g *gatewaySource) Meta() chatMeta {
	return chatMeta{ConversationID: g.convID, ResponseID: g.respID}
}
func (g *gatewaySource) Close() {}

// --- WebSocket image generation ---

type imagineSource struct {
	ctx       context.Context
	ch        <-chan grok.ImagineEvent
	pending   *grok.ImagineEvent
	closed    bool
	n         int
	emitThink bool

	progress  map[string]int
	completed map[string]bool
	last      int
}

// openImagineSource waits for the first upstream event (see
// openGatewaySource for why).
func openImagineSource(ctx context.Context, ch <-chan grok.ImagineEvent, n int, emitThink bool) (*imagineSource, error) {
	s := &imagineSource{
		ctx: ctx, ch: ch, n: n, emitThink: emitThink,
		progress: map[string]int{}, completed: map[string]bool{}, last: -1,
	}
	select {
	case ev, ok := <-ch:
		if !ok {
			s.closed = true
			return s, nil
		}
		if ev.Type == grok.ImagineEventError {
			return nil, platform.UpstreamError(ev.Error, 502, "")
		}
		s.pending = &ev
	case <-ctx.Done():
		return nil, ctxError(ctx)
	}
	return s, nil
}

func (s *imagineSource) Next() ([]chatEvent, error) {
	for {
		var ev grok.ImagineEvent
		switch {
		case s.pending != nil:
			ev = *s.pending
			s.pending = nil
		case s.closed:
			return nil, io.EOF
		default:
			select {
			case e, ok := <-s.ch:
				if !ok {
					s.closed = true
					if s.ctx.Err() != nil {
						return nil, ctxError(s.ctx)
					}
					return nil, io.EOF
				}
				ev = e
			case <-s.ctx.Done():
				return nil, ctxError(s.ctx)
			}
		}
		switch ev.Type {
		case grok.ImagineEventProgress:
			s.progress[ev.ImageID] = ev.Progress
			aggregate := computeAggregateProgress(s.progress, s.n)
			if s.emitThink && aggregate > s.last {
				s.last = aggregate
				reason := grok.FormatImageProgress("图片", aggregate, len(s.completed), s.n)
				return []chatEvent{{Kind: evThinking, Content: reason + "\n"}}, nil
			}
		case grok.ImagineEventImage:
			s.completed[ev.ImageID] = true
			s.progress[ev.ImageID] = 100
			if ev.URL != "" {
				url := grok.ImageBaseURL + strings.TrimPrefix(ev.URL, "/")
				return []chatEvent{{Kind: evImage, Content: url, ImageID: ev.ImageID}}, nil
			}
		case grok.ImagineEventError:
			return nil, platform.UpstreamError(ev.Error, 502, "")
		}
	}
}

func (s *imagineSource) Meta() chatMeta { return chatMeta{TrimThinking: true} }
func (s *imagineSource) Close()         {}

// computeAggregateProgress averages the per-slot progress over the expected
// number of images.
func computeAggregateProgress(progressMap map[string]int, total int) int {
	if total <= 0 {
		return 100
	}
	if len(progressMap) == 0 {
		return 0
	}
	sum := 0
	for _, v := range progressMap {
		p := v
		if p < 0 {
			p = 0
		}
		if p > 100 {
			p = 100
		}
		sum += p
	}
	result := sum / total
	if result > 100 {
		result = 100
	}
	return result
}

// --- opening ---

// chatBackend identifies the upstream a chat request is served by.
type chatBackend int

const (
	backendWeb chatBackend = iota
	backendConsole
	backendGateway
	backendImagine
)

// chatCall is one parsed chat-shaped request, independent of the protocol it
// arrived in (chat completions, responses, messages).
type chatCall struct {
	spec      *model.Spec
	modelName string
	messages  []map[string]any
	stream    bool
	emitThink bool

	temperature float64
	topP        float64
	effort      string

	// image generation (WS imagine models)
	imageN    int
	imageSize string

	backend    chatBackend
	message    string   // flattened prompt (web, gateway)
	fileInputs []string // uploaded file references (web, gateway)
	prompt     string   // image prompt (imagine)
}

// Sampling defaults when the request names none.
const (
	defaultTemperature = 0.8
	defaultTopP        = 0.95
	// maxChatImages caps n for chat-based WS image generation.
	maxChatImages = 10
)

// gatewayModels lists the models served by the WebSocket gateway, mapped to
// the upstream model id the gateway expects.
var gatewayModels = map[string]string{"grok-4.6": "expert"}

func usesGatewayChat(modelName string) bool {
	_, ok := gatewayModels[modelName]
	return ok
}

func gatewayUpstreamModel(modelName string) string {
	if up, ok := gatewayModels[modelName]; ok {
		return up
	}
	return modelName
}

// prepareChat validates the request and flattens its prompt for the backend
// the model belongs to. Errors are client errors: no account is involved yet.
func (s *Server) prepareChat(call *chatCall) *platform.AppError {
	spec := call.spec
	switch {
	case spec.IsConsoleChat():
		call.backend = backendConsole
		return nil
	case spec.IsImage() && !spec.IsImageEdit() && grok.IsWSImageModel(spec.ModelName):
		call.backend = backendImagine
		call.prompt = extractImagePrompt(call.messages)
		if call.prompt == "" {
			return platform.ValidationError("Empty prompt for image generation", "messages")
		}
		return nil
	}
	call.backend = backendWeb
	if usesGatewayChat(call.modelName) {
		call.backend = backendGateway
	}
	message, files, perr := extractMessages(call.messages)
	if perr != nil {
		return perr
	}
	call.message, call.fileInputs = message, files
	return nil
}

// convTrack remembers where to store the upstream conversation ids of a
// finished attempt (conversation continuation).
type convTrack struct {
	store ConversationStore
	key   string
}

func (t *convTrack) save(meta chatMeta) {
	if t == nil || meta.ConversationID == "" || meta.ResponseID == "" {
		return
	}
	t.store.Set(t.key, meta.ConversationID, meta.ResponseID)
}

// convKey scopes a conversation to account *and* model: continuing a
// conversation across models would splice unrelated contexts.
func convKey(token, modelName string) string { return token + "\x00" + modelName }

// conversationContinuationEnabled reports whether follow-up requests may
// continue the previous upstream conversation of the same account.
func (s *Server) conversationContinuationEnabled() bool {
	return s.config().GetBool("features.conversation_continuation", false)
}

// openChat starts the upstream call for call on lease. It returns before any
// byte of the HTTP response is written, so a failure here can still be
// retried on another account.
func (s *Server) openChat(ctx context.Context, lease *account.Lease, call *chatCall) (chatSource, *convTrack, error) {
	if s.Transport == nil {
		return nil, nil, errUpstreamUnavailable()
	}
	switch call.backend {
	case backendConsole:
		payload := grok.BuildConsolePayload(call.messages, call.modelName, call.temperature, call.topP, call.effort, true)
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, nil, platform.UpstreamError("encode console payload: "+err.Error(), 500, "")
		}
		// Always ask upstream for an SSE stream: with stream=false it answers
		// with a single JSON document, which the SSE parser would silently
		// drop and the caller would see an empty completion.
		rc, err := s.Transport.PostStream(ctx, grok.ConsoleResponses, lease.Token, body, grok.WithConsoleMode())
		if err != nil {
			return nil, nil, err
		}
		return newConsoleSource(ctx, rc), nil, nil

	case backendGateway:
		ch := s.Transport.StreamGatewayChat(ctx, lease.Token, gatewayUpstreamModel(call.modelName), call.message, call.fileInputs)
		src, err := openGatewaySource(ctx, ch)
		if err != nil {
			return nil, nil, err
		}
		// The gateway cannot continue a conversation: nothing to track.
		return src, nil, nil

	case backendImagine:
		n := call.imageN
		if n < 1 {
			n = 1
		}
		if n > maxChatImages {
			n = maxChatImages
		}
		size := call.imageSize
		if size == "" {
			size = "1024x1024"
		}
		cfg := s.config()
		ch := s.Transport.StreamImages(ctx, lease.Token, call.prompt, grok.ResolveAspectRatio(size), n,
			cfg.GetBool("features.enable_nsfw", true), grok.IsProImageModel(call.spec.ModelName))
		src, err := openImagineSource(ctx, ch, n, call.emitThink)
		if err != nil {
			return nil, nil, err
		}
		return src, nil, nil
	}

	// grok.com app-chat.
	continuation := s.conversationContinuationEnabled() && s.ConvTracker != nil
	key := convKey(lease.Token, call.modelName)
	var conv *grok.ConversationState
	if continuation {
		conv = s.ConvTracker.Get(key)
	}
	var (
		payload map[string]any
		target  string
		isNew   bool
	)
	if conv != nil && conv.ConversationID != "" && conv.LastResponseID != "" {
		// Follow-up message in an existing conversation.
		payload = grok.BuildResponsePayload(call.message, conv.LastResponseID, model.ModeId(lease.ModeID), call.fileInputs, nil, nil)
		target = fmt.Sprintf(grok.Responses, conv.ConversationID)
	} else {
		payload = grok.BuildChatPayload(call.message, model.ModeId(lease.ModeID), call.fileInputs, nil, nil, nil)
		target = grok.Chat
		isNew = true
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, platform.UpstreamError("encode chat payload: "+err.Error(), 500, "")
	}
	rc, err := s.Transport.PostStream(ctx, target, lease.Token, body)
	if err != nil {
		if continuation {
			// A stale follow-up context: start a new conversation next time.
			s.ConvTracker.Clear(key)
		}
		return nil, nil, err
	}
	var track *convTrack
	if continuation && isNew {
		track = &convTrack{store: s.ConvTracker, key: key}
	}
	return newWebSource(ctx, rc), track, nil
}
