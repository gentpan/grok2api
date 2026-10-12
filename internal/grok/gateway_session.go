package grok

import (
	"github.com/gentpan/grok2api/internal/platform"
)

// gatewaySession is the client-side state machine of one Gateway turn:
//
//	session.create sent
//	  -> session.created (for our event id)      created
//	  -> conversation.attached                   attached
//	  -> response.create sent once both hold     turnSent
//	  -> response.chunk* / response.output_text.* deltas
//	  -> response.done                           finished
//
// It is pure: handle consumes one decoded server envelope and returns what the
// I/O loop must do, so the protocol can be tested without a socket.
type gatewaySession struct {
	initialID   string
	prompt      string
	attachments []string

	created  bool
	attached bool
	turnSent bool
	parsed   gatewayEventResult
}

func newGatewaySession(initialID, prompt string, attachments []string) *gatewaySession {
	return &gatewaySession{initialID: initialID, prompt: prompt, attachments: attachments}
}

// gatewayStep is the outcome of handling one server event.
type gatewayStep struct {
	// events are normalized events for the consumer, in order.
	events []GatewayEvent
	// send, when non-nil, is a client message to write.
	send map[string]any
	// done reports that the turn finished successfully.
	done bool
	// err ends the turn with a failure.
	err error
}

// handle processes one server envelope.
func (s *gatewaySession) handle(envelope map[string]any) gatewayStep {
	if sessionID, _ := envelope["session_id"].(string); sessionID != "" {
		s.parsed.SessionID = sessionID
	}
	event := unwrapGatewayEvent(envelope)
	var step gatewayStep
	if err := parseGatewayEvent(event, &s.parsed); err != nil {
		step.err = err
		return step
	}
	if s.parsed.lastContent != "" {
		step.events = append(step.events, GatewayEvent{
			Kind: s.parsed.lastKind, Content: s.parsed.lastContent,
			ConversationID: s.parsed.ConversationID, ResponseID: s.parsed.ResponseID,
		})
		s.parsed.lastContent = ""
	}

	typeName, _ := event["type"].(string)
	switch typeName {
	case "session.created":
		clientID, _ := event["client_event_id"].(string)
		if clientID == "" {
			clientID, _ = event["event_id"].(string)
		}
		s.created = clientID == "" || clientID == s.initialID
	case "conversation.attached":
		s.attached = true
	case "response.done":
		step.events = append(step.events, GatewayEvent{
			Kind: EventSoftStop, ConversationID: s.parsed.ConversationID, ResponseID: s.parsed.ResponseID,
		})
		step.done = true
		return step
	case "session.ended":
		step.err = platform.UpstreamError("Grok Gateway session ended before response completion", 502, "")
		return step
	}
	if s.created && s.attached && !s.turnSent {
		step.send = buildGatewayTurnMessages(s.parsed.SessionID, s.prompt, s.attachments)
		s.turnSent = true
	}
	return step
}
