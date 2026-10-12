package grok

import (
	"context"
	"errors"
	"sync"
	"time"
)

// maxDPoPSessions bounds the cache: every entry holds an ECDSA key, and
// sessions for rotated or removed tokens would otherwise accumulate for the
// life of the process.
const maxDPoPSessions = 1024

// errDPoPLoaderAborted is what waiters see when the goroutine fetching their
// session panicked; they retry instead of failing.
var errDPoPLoaderAborted = errors.New("dpop session loader aborted")

// dpopCall is one in-flight session fetch shared by all callers of a key.
type dpopCall struct {
	done    chan struct{}
	session dpopSession
	err     error
	// retry is set when the fetch ended because of the loader's own context
	// (or a panic), which says nothing about the other waiters' requests.
	retry bool
}

// dpopStore caches DPoP sessions per (base URL, token) with single-flight
// loading: concurrent callers for one key share a single fetch, and its
// outcome — including a failure — instead of each retrying serially.
type dpopStore struct {
	mu       sync.Mutex
	sessions map[string]dpopSession
	loading  map[string]*dpopCall
}

func newDPoPStore() *dpopStore {
	return &dpopStore{
		sessions: map[string]dpopSession{},
		loading:  map[string]*dpopCall{},
	}
}

func (s *dpopSession) usable(now time.Time) bool {
	return s.expiresAt.After(now.UTC().Add(dpopRefreshSkew))
}

// get returns a usable cached session for key or loads one with fetch.
func (s *dpopStore) get(ctx context.Context, key string, fetch func(context.Context) (dpopSession, error)) (dpopSession, error) {
	for {
		s.mu.Lock()
		if sess, ok := s.sessions[key]; ok && sess.usable(time.Now()) {
			s.mu.Unlock()
			return sess, nil
		}
		if call := s.loading[key]; call != nil {
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return dpopSession{}, ctx.Err()
			case <-call.done:
			}
			if call.err == nil {
				return call.session, nil
			}
			if call.retry {
				continue
			}
			return dpopSession{}, call.err
		}
		call := &dpopCall{done: make(chan struct{})}
		s.loading[key] = call
		s.mu.Unlock()
		return s.load(ctx, key, call, fetch)
	}
}

// load runs fetch as the single in-flight loader for key. The loading marker
// is always cleared and waiters released (also on panic), otherwise every
// later caller for this key would block forever.
func (s *dpopStore) load(ctx context.Context, key string, call *dpopCall, fetch func(context.Context) (dpopSession, error)) (session dpopSession, err error) {
	finished := false
	defer func() {
		s.mu.Lock()
		delete(s.loading, key)
		switch {
		case !finished:
			call.err, call.retry = errDPoPLoaderAborted, true
		case err != nil:
			call.err, call.retry = err, ctx.Err() != nil
		default:
			call.session = session
			s.storeLocked(key, session)
		}
		s.mu.Unlock()
		close(call.done)
	}()
	session, err = fetch(ctx)
	finished = true
	return session, err
}

// invalidate drops the cached session for key, but only when it is still the
// one identified by accessToken (an empty accessToken matches any), so a
// request that failed with a stale session cannot evict a fresh one.
func (s *dpopStore) invalidate(key, accessToken string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.sessions[key]; ok && (accessToken == "" || cur.accessToken == accessToken) {
		delete(s.sessions, key)
	}
}

// size reports the number of cached sessions.
func (s *dpopStore) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// storeLocked inserts a session and evicts: first everything already expired,
// then (if the cache is still over its bound) the sessions closest to expiry.
func (s *dpopStore) storeLocked(key string, session dpopSession) {
	s.sessions[key] = session
	now := time.Now().UTC()
	for k, v := range s.sessions {
		if k != key && !v.expiresAt.After(now) {
			delete(s.sessions, k)
		}
	}
	for len(s.sessions) > maxDPoPSessions {
		var oldestKey string
		var oldest time.Time
		for k, v := range s.sessions {
			if k == key {
				continue
			}
			if oldestKey == "" || v.expiresAt.Before(oldest) {
				oldestKey, oldest = k, v.expiresAt
			}
		}
		if oldestKey == "" {
			break
		}
		delete(s.sessions, oldestKey)
	}
}
