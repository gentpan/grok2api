package api

import (
	"errors"
	"strconv"
	"strings"

	"github.com/gentpan/grok2api/internal/account"
	"github.com/gentpan/grok2api/internal/model"
	"github.com/gentpan/grok2api/internal/platform"
)

const (
	// defaultMaxRetries is retry.max_retries when the config does not set it.
	defaultMaxRetries = 1
	// randomStrategyMinRetries is the retry floor under the random selection
	// strategy: its picks are uninformed, so it gets more attempts.
	randomStrategyMinRetries = 5
)

// defaultRetryCodes is retry.on_codes when the config does not set it.
var defaultRetryCodes = []string{"429", "401", "503"}

// settings is the read side of the configuration.
type settings interface {
	GetStr(key, def string) string
	GetInt(key string, def int) int
	GetBool(key string, def bool) bool
	GetList(key string, def []string) []string
}

// retryConfig is the failover policy of one request, resolved once from the
// configuration and the pool's selection strategy.
type retryConfig struct {
	// MaxRetries is how many *additional* accounts may be tried after the
	// first one fails (0 = no failover).
	MaxRetries int
	// OnCodes are the upstream statuses that warrant another account.
	OnCodes map[int]struct{}
	// AutoModeFallback lets AUTO models fall back to FAST/EXPERT quota.
	AutoModeFallback bool
}

// newRetryConfig reads the policy from cfg. strategy is the pool's real
// selection strategy (empty when there is no pool).
func newRetryConfig(cfg settings, strategy account.Strategy) retryConfig {
	max := cfg.GetInt("retry.max_retries", defaultMaxRetries)
	if max < 0 {
		max = 0
	}
	if strategy == account.StrategyRandom && max < randomStrategyMinRetries {
		max = randomStrategyMinRetries
	}
	codes := map[int]struct{}{}
	for _, c := range cfg.GetList("retry.on_codes", defaultRetryCodes) {
		if n, err := strconv.Atoi(strings.TrimSpace(c)); err == nil {
			codes[n] = struct{}{}
		}
	}
	return retryConfig{
		MaxRetries:       max,
		OnCodes:          codes,
		AutoModeFallback: cfg.GetBool("features.auto_chat_mode_fallback", true),
	}
}

// retryConfig resolves the failover policy for the current configuration.
func (s *Server) retryConfig() retryConfig {
	var strategy account.Strategy
	if s.Directory != nil {
		strategy = s.Directory.Strategy()
	}
	return newRetryConfig(s.config(), strategy)
}

// shouldRetry reports whether err warrants a retry with a different account:
// an upstream "invalid credentials" body always does, otherwise the status
// must be one of OnCodes.
func (r retryConfig) shouldRetry(err error) bool {
	if err == nil {
		return false
	}
	var appErr *platform.AppError
	if !errors.As(err, &appErr) {
		return false
	}
	if appErr.Kind == platform.ErrUpstream && platform.IsInvalidCredentialsBody(appErr.Body) {
		return true
	}
	_, ok := r.OnCodes[appErr.Status]
	return ok
}

// modeCandidates returns the candidate mode ids for a spec. AUTO mode falls
// back to (AUTO, FAST, EXPERT) when AutoModeFallback is set.
func (r retryConfig) modeCandidates(spec *model.Spec) []int {
	if spec.ModeId == model.ModeAuto && r.AutoModeFallback {
		return []int{int(model.ModeAuto), int(model.ModeFast), int(model.ModeExpert)}
	}
	return []int{int(spec.ModeId)}
}
