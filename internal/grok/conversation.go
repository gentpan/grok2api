package grok

import (
	"sync"
	"time"
)

// ConversationState holds the active conversation context for one account token.
type ConversationState struct {
	ConversationID string
	LastResponseID string // user response ID of the last message (parent for next reply)
	CreatedAt      time.Time
}

// ConversationTracker stores active conversation contexts per account token.
// An entry lives for ttl from the moment it was created: reads do not extend
// it. That cap matters because a conversation is anchored to the account, not
// to the caller, so a busy account would otherwise keep one caller's context
// alive (and spliced into later callers' requests) forever. Close stops the
// background sweeper.
type ConversationTracker struct {
	mu   sync.Mutex
	conv map[string]*ConversationState
	ttl  time.Duration

	stop     chan struct{}
	stopOnce sync.Once
}

// NewConversationTracker creates a tracker with the given TTL and starts its
// background sweeper; call Close to stop it.
func NewConversationTracker(ttl time.Duration) *ConversationTracker {
	ct := &ConversationTracker{
		conv: map[string]*ConversationState{},
		ttl:  ttl,
		stop: make(chan struct{}),
	}
	// NewTicker panics on a non-positive interval; a non-positive ttl just
	// means entries are always stale, so sweep on a fixed cadence instead.
	interval := ttl
	if interval <= 0 {
		interval = time.Minute
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ct.stop:
				return
			case <-ticker.C:
				ct.evictStale()
			}
		}
	}()
	return ct
}

// Close stops the background sweeper. The tracker stays usable (entries still
// expire on read), and Close is idempotent.
func (ct *ConversationTracker) Close() {
	ct.stopOnce.Do(func() { close(ct.stop) })
}

// Get returns a copy of the conversation state for a token, or nil if there is
// none or it expired (ttl after creation, regardless of use).
func (ct *ConversationTracker) Get(token string) *ConversationState {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	s, ok := ct.conv[token]
	if !ok {
		return nil
	}
	if time.Since(s.CreatedAt) > ct.ttl {
		delete(ct.conv, token)
		return nil
	}
	cp := *s
	return &cp
}

// Set stores or updates the conversation state for a token.
func (ct *ConversationTracker) Set(token, conversationID, responseID string) {
	now := time.Now()
	ct.mu.Lock()
	defer ct.mu.Unlock()
	ct.conv[token] = &ConversationState{
		ConversationID: conversationID,
		LastResponseID: responseID,
		CreatedAt:      now,
	}
}

// Clear removes the conversation state for a token (e.g. on error).
func (ct *ConversationTracker) Clear(token string) {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	delete(ct.conv, token)
}

func (ct *ConversationTracker) evictStale() {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	now := time.Now()
	for k, s := range ct.conv {
		if now.Sub(s.CreatedAt) > ct.ttl {
			delete(ct.conv, k)
		}
	}
}
