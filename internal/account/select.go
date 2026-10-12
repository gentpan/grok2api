package account

import (
	"math/rand"

	"github.com/gentpan/grok2api/internal/platform"
)

// Reserve picks the best account for the (pool, mode) candidates. modeID must
// be a concrete mode (0..5): there is no "any mode" reservation.
func (d *Directory) Reserve(poolCandidates []int, modeID int, excludeTokens []string, preferTags []string) (*Lease, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	nowMs := platform.NowMs()
	nowSec := nowMs / 1000
	exclude := map[string]struct{}{}
	for _, t := range excludeTokens {
		exclude[t] = struct{}{}
	}
	for _, poolID := range poolCandidates {
		var chosen *slot
		if d.strategy == StrategyQuota {
			chosen = d.quotaSelectLocked(poolID, modeID, exclude, nowMs)
		} else {
			chosen = d.randomSelectLocked(poolID, modeID, exclude, nowSec)
		}
		if chosen != nil {
			chosen.Inflight++
			chosen.LastUseAt = nowMs
			d.leaseSeq++
			return &Lease{
				ID:         d.leaseSeq,
				Token:      chosen.Token,
				PoolID:     chosen.PoolID,
				ModeID:     modeID,
				SelectedAt: nowMs,
			}, modeID
		}
	}
	return nil, modeID
}

func (d *Directory) quotaSelectLocked(poolID int, modeID int, exclude map[string]struct{}, nowMs int64) *slot {
	key := modeKey{PoolID(poolID), modeID}
	bucket := d.byMode[key]
	if len(bucket) == 0 {
		return nil
	}
	nowSec := nowMs / 1000
	var best *slot
	bestScore := 0.0
	hasBest := false
	for tok := range bucket {
		if _, skip := exclude[tok]; skip {
			continue
		}
		s := d.slots[tok]
		if s == nil {
			continue
		}
		w := s.Quota.Get(modeID)
		if w == nil {
			continue
		}
		if w.IsWindowExpired(nowMs) {
			w.Remaining = w.Total
			reset := nowMs + int64(w.WindowSeconds)*1000
			w.ResetAt = &reset
			s.Quota.Set(modeID, *w)
		}
		if w.Remaining <= 0 || s.Inflight >= d.maxInflight {
			continue
		}
		score := quotaScore(s, w.Remaining, nowSec)
		if !hasBest || score > bestScore {
			best = s
			bestScore = score
			hasBest = true
		}
	}
	return best
}

func quotaScore(s *slot, remaining int, nowSec int64) float64 {
	score := s.Health*100.0 + float64(remaining)*25.0 - float64(s.Inflight)*20.0
	failPenalty := float64(s.FailCount)
	if failPenalty > 10 {
		failPenalty = 10
	}
	score -= failPenalty * 4.0
	if s.LastUseAt > 0 {
		ageSec := nowSec - s.LastUseAt/1000
		if ageSec < 60 {
			score -= (1 - float64(ageSec)/60) * 15.0
		}
	}
	return score
}

func (d *Directory) randomSelectLocked(poolID int, modeID int, exclude map[string]struct{}, nowSec int64) *slot {
	// Union of all mode buckets for the pool (random strategy ignores specific mode).
	candidates := []*slot{}
	for _, m := range SupportedModeIDs(PoolID(poolID).Name()) {
		for tok := range d.byMode[modeKey{PoolID(poolID), m}] {
			if _, skip := exclude[tok]; skip {
				continue
			}
			s := d.slots[tok]
			if s == nil {
				continue
			}
			if s.CoolingUntil > nowSec {
				continue
			}
			if s.Inflight >= d.maxInflight {
				continue
			}
			// Accounts at/above the failure threshold are skipped, but only for
			// a recovery window after their last failure; afterwards they are
			// probed again (a success resets FailCount, see Feedback). Without
			// this an account excluded here could never succeed again.
			if s.FailCount >= randomMaxFails &&
				nowSec*1000-s.LastFailAt < randomFailRecoverSec*1000 {
				continue
			}
			candidates = append(candidates, s)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	return candidates[rand.Intn(len(candidates))]
}

// Release decrements the inflight counter for a lease.
func (d *Directory) Release(l *Lease) {
	if l == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.slots[l.Token]
	if !ok {
		return
	}
	if s.Inflight > 0 {
		s.Inflight--
	}
}
