package account

import "github.com/gentpan/grok2api/internal/platform"

// Runtime health model. A slot's Health starts at 1.0, drops multiplicatively
// on failures (never below minHealth) and recovers by successStep per success.
const (
	randomMaxFails    = 5
	successStep       = 0.12
	authFactor        = 0.55
	forbiddenFactor   = 0.25
	rateLimitFactor   = 0.45
	serverErrorFactor = 0.75
	minHealth         = 0.05
	consoleCoolingSec = 14400
)

// randomFailRecoverSec is how long (seconds since the last failure) an account
// with FailCount >= randomMaxFails stays out of random selection.
const randomFailRecoverSec = 300

// Feedback applies a request outcome to the reserved account's runtime state.
//
// remaining and resetAtMs let a caller push upstream-reported quota into the
// slot (resetAtMs is a duration in milliseconds, not an absolute time). All
// current callers pass nil, nil, so those code paths are unused; the
// parameters are kept for the integrator and callers must not rely on them.
//
// The runtime state is in memory only; persistence is RefreshService's job.
func (d *Directory) Feedback(token string, kind FeedbackKind, modeID int, remaining *int, resetAtMs *int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.slots[token]
	if !ok {
		return
	}
	nowMs := platform.NowMs()
	switch kind {
	case FbSuccess:
		d.onSuccessLocked(s, modeID)
	case FbRateLimited:
		d.onRateLimitedLocked(s, modeID, resetAtMs, nowMs)
	case FbUnauthorized:
		s.penalize(authFactor, nowMs)
		s.StatusID = StatusIDExpired
		d.reindexLocked(s)
	case FbForbidden:
		s.penalize(forbiddenFactor, nowMs)
	case FbServerError:
		s.penalize(serverErrorFactor, nowMs)
	}
	if d.strategy == StrategyQuota && remaining != nil && modeID >= 0 {
		d.overrideRemainingLocked(s, modeID, *remaining, resetAtMs != nil)
	}
}

// penalize records one failure: health drops by factor (floored), the failure
// streak grows.
func (s *slot) penalize(factor float64, nowMs int64) {
	s.Health = maxF(minHealth, s.Health*factor)
	s.FailCount++
	s.LastFailAt = nowMs
}

func (d *Directory) onSuccessLocked(s *slot, modeID int) {
	if d.strategy == StrategyQuota && modeID >= 0 {
		if w := s.Quota.Get(modeID); w != nil && w.WindowSeconds > 0 && w.Remaining > 0 {
			w.Remaining--
			s.Quota.Set(modeID, *w)
		}
	}
	s.Health = minF(1.0, s.Health+successStep)
	if d.strategy == StrategyRandom {
		// Random strategy only: a success clears the failure streak so a
		// recovered account is not permanently excluded. The quota strategy
		// keeps its existing (capped) FailCount score penalty untouched.
		s.FailCount = 0
	}
}

func (d *Directory) onRateLimitedLocked(s *slot, modeID int, resetAfterMs *int64, nowMs int64) {
	if modeID >= 0 {
		if w := s.Quota.Get(modeID); w != nil && w.WindowSeconds > 0 {
			w.Remaining = 0
			reset := nowMs + int64(w.WindowSeconds)*1000
			if resetAfterMs != nil && *resetAfterMs > 0 {
				reset = nowMs + *resetAfterMs
			}
			w.ResetAt = &reset
			s.Quota.Set(modeID, *w)
		}
	}
	if d.strategy == StrategyRandom {
		cooling := nowMs/1000 + d.poolCoolingSec(s.PoolID, modeID)
		if cooling > s.CoolingUntil {
			s.CoolingUntil = cooling
		}
	}
	s.penalize(rateLimitFactor, nowMs)
}

// maxRemaining bounds an upstream-reported remaining count.
const maxRemaining = 32767

// overrideRemainingLocked stores an upstream-reported remaining count (clamped
// to 0..maxRemaining) in the slot's window. When reindex is set the slot is
// (re-)added to the mode's selection bucket.
func (d *Directory) overrideRemainingLocked(s *slot, modeID, remaining int, reindex bool) {
	w := s.Quota.Get(modeID)
	if w == nil || w.WindowSeconds <= 0 {
		return
	}
	if remaining < 0 {
		remaining = 0
	}
	if remaining > maxRemaining {
		remaining = maxRemaining
	}
	w.Remaining = remaining
	if !reindex {
		return
	}
	s.Quota.Set(modeID, *w)
	if s.StatusID == StatusIDActive {
		k := modeKey{s.PoolID, modeID}
		if d.byMode[k] == nil {
			d.byMode[k] = map[string]struct{}{}
		}
		d.byMode[k][s.Token] = struct{}{}
	}
}

// poolCoolingSec is the random-strategy cool-down after a 429.
func (d *Directory) poolCoolingSec(pool PoolID, modeID int) int64 {
	if modeID == ModeConsole {
		return consoleCoolingSec
	}
	switch pool {
	case PoolBasic:
		return 86400
	case PoolSuper, PoolHeavy:
		return 7200
	}
	return 7200
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
