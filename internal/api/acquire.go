package api

import (
	"context"
	"time"

	"github.com/gentpan/grok2api/internal/account"
	"github.com/gentpan/grok2api/internal/model"
	"github.com/gentpan/grok2api/internal/platform"
)

// attemptFunc performs one upstream attempt with the leased account.
//
// committed reports that part of the HTTP response was already written, so the
// request can neither be retried nor answered with an error body any more.
type attemptFunc func(ctx context.Context, lease *account.Lease) (committed bool, err error)

// acquireOptions tunes one acquire call.
type acquireOptions struct {
	// Exclude lists tokens that must not be leased (accounts an earlier pass of
	// the same request already used).
	Exclude []string
	// NoAccountsMessage replaces the default "No available accounts" error.
	NoAccountsMessage string
}

// acquireResult describes how an acquire call ended.
type acquireResult struct {
	// Token is the account of the last attempt ("" if none was made).
	Token string
	// BYO is true when that attempt used the caller's own SSO token instead of
	// a pooled account.
	BYO bool
	// Committed mirrors the last attempt's committed flag.
	Committed bool
}

// acquire is the single place where a request obtains an upstream account,
// runs against it, and settles the outcome:
//
//   - a caller that may use the pool reserves from it (retrying with other
//     accounts per the retry policy); a refresh-on-demand runs at most once
//     per request, when the first reservation finds the pool empty;
//   - when no pooled account is left, the caller's own SSO token (if any) is
//     used. A failed own-token attempt is never repeated with the same token;
//   - every pooled attempt is released and fed back to the directory (success
//     or classified failure). Own-token attempts touch no account state;
//   - a request cancelled by its client is not an account failure.
//
// The returned error is nil on success. When it is non-nil and
// result.Committed is false the caller still owns the response and must
// report the error; when Committed is true the response is already underway.
func (s *Server) acquire(ctx context.Context, caller Caller, spec *model.Spec, opt acquireOptions, run attemptFunc) (acquireResult, error) {
	rc := s.retryConfig()
	exclude := append([]string(nil), opt.Exclude...)
	refreshed := false
	var res acquireResult
	var lastErr error

	for attempt := 0; attempt <= rc.MaxRetries; attempt++ {
		var lease *account.Lease
		byo := false
		if caller.PoolAllowed {
			lease = s.reservePooled(rc, spec, exclude)
			if lease == nil && !refreshed && s.Refresh != nil && s.Directory != nil {
				refreshed = true
				_ = s.Refresh.RefreshOnDemand(ctx)
				lease = s.reservePooled(rc, spec, exclude)
			}
		}
		if lease == nil && caller.SSOToken != "" {
			lease = &account.Lease{Token: caller.SSOToken, ModeID: int(spec.ModeId)}
			byo = true
		}
		if lease == nil {
			if lastErr != nil {
				// A previous attempt failed and no other account is left:
				// report that real upstream error, not a generic message.
				return res, lastErr
			}
			msg := opt.NoAccountsMessage
			return res, platform.RateLimitError(msg)
		}
		exclude = append(exclude, lease.Token)

		committed, err := s.runAttempt(ctx, lease, byo, run)
		res = acquireResult{Token: lease.Token, BYO: byo, Committed: committed}
		if err == nil {
			if !byo {
				s.feedbackSuccess(lease)
			}
			return res, nil
		}
		lastErr = err
		aborted := ctx.Err() != nil
		if !byo && !aborted {
			s.feedbackError(lease.Token, err, lease.ModeID)
		}
		switch {
		case committed, aborted, byo:
			return res, err
		case !rc.shouldRetry(err), attempt == rc.MaxRetries:
			return res, err
		}
	}
	return res, lastErr
}

// runAttempt runs one attempt and releases a pooled lease when it ends, also
// when run panics (a recovered handler or job would otherwise leak the
// account's in-flight slot forever).
func (s *Server) runAttempt(ctx context.Context, lease *account.Lease, byo bool, run attemptFunc) (bool, error) {
	if !byo && s.Directory != nil {
		defer s.Directory.Release(lease)
	}
	return run(ctx, lease)
}

// reservePooled leases an account from the pool for spec, trying each
// candidate mode in turn. It returns nil when the pool has none.
func (s *Server) reservePooled(rc retryConfig, spec *model.Spec, exclude []string) *account.Lease {
	if s.Directory == nil {
		return nil
	}
	for _, modeID := range rc.modeCandidates(spec) {
		if lease, _ := s.Directory.Reserve(spec.PoolCandidates(), modeID, exclude, nil); lease != nil {
			return lease
		}
	}
	return nil
}

// feedbackSuccess records a successful request and schedules a (debounced)
// upstream quota sync for the account.
func (s *Server) feedbackSuccess(lease *account.Lease) {
	if s.Directory == nil {
		return
	}
	s.Directory.Feedback(lease.Token, account.FbSuccess, lease.ModeID, nil, nil)
	s.reconcileQuotaAsync(lease.Token, reconcileAfterSuccess)
}

// recordFailureTimeout bounds persisting an account failure.
const recordFailureTimeout = 15 * time.Second

// feedbackError classifies a failed attempt and posts it to the directory.
func (s *Server) feedbackError(token string, err error, modeID int) {
	appErr := asAppError(err)
	if appErr.Code == codeEmptyUpstream {
		return // not the account's fault; Release already happened
	}
	kind := account.FeedbackKindFromStatus(appErr.Status)
	// Invalid credentials are reported with a variety of statuses (403
	// blocked-user, 400 session not found, ...): the body decides.
	if kind != account.FbUnauthorized && platform.IsInvalidCredentialsBody(appErr.Body) {
		kind = account.FbUnauthorized
	}
	if s.Directory != nil {
		s.Directory.Feedback(token, kind, modeID, nil, nil)
	}
	if kind == account.FbRateLimited {
		// A 429 zeroes the mode locally for a whole window; confirm with
		// upstream so a transient limit does not blacklist the mode for hours.
		s.reconcileQuotaAsync(token, reconcileAfterRateLimit)
	}
	if kind == account.FbUnauthorized && s.Refresh != nil {
		// Persist the expiry; independent of the request's own context so a
		// client abort cannot lose it.
		ctx, cancel := context.WithTimeout(s.baseContext(), recordFailureTimeout)
		defer cancel()
		s.Refresh.RecordFailure(ctx, token, modeID, appErr)
	}
}
