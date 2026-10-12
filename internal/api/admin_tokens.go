package api

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/gentpan/grok2api/internal/account"
	"github.com/gentpan/grok2api/internal/logger"
	"github.com/gentpan/grok2api/internal/platform"
)

// --- Tokens CRUD ---

// defaultPool is the pool new tokens land in when the request names none.
const defaultPool = "basic"

func (s *Server) handleTokensList(c *gin.Context) {
	if s.Repo == nil {
		writeAppError(c, errRepositoryUnavailable())
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	recs, err := s.listAllAccounts(ctx)
	if err != nil {
		writeAppError(c, err)
		return
	}
	out := []map[string]any{}
	for _, rec := range recs {
		out = append(out, serializeRecord(rec))
	}
	c.JSON(http.StatusOK, gin.H{"tokens": out})
}

func serializeRecord(rec *account.Record) map[string]any {
	qs := rec.QuotaSet()
	quota := map[string]any{}
	addQ := func(name string, w *account.QuotaWindow) {
		if w == nil || w.WindowSeconds <= 0 {
			return
		}
		entry := map[string]any{"remaining": w.Remaining, "total": w.Total, "window_seconds": w.WindowSeconds}
		if w.ResetAt != nil {
			entry["reset_at"] = *w.ResetAt
		}
		quota[name] = entry
	}
	auto := qs.Auto
	fast := qs.Fast
	expert := qs.Expert
	addQ("auto", &auto)
	addQ("fast", &fast)
	addQ("expert", &expert)
	addQ("heavy", qs.Heavy)
	addQ("console", qs.Console)
	lastUsed := int64(0)
	if rec.LastUseAt != nil {
		lastUsed = *rec.LastUseAt
	}
	pool := rec.Pool
	if pool == "" {
		pool = defaultPool
	}
	return map[string]any{
		"token":        rec.Token,
		"pool":         pool,
		"status":       string(rec.Status),
		"quota":        quota,
		"use_count":    rec.UsageUseCount,
		"last_used_at": lastUsed,
		"tags":         rec.Tags,
	}
}

func (s *Server) handleTokensReplace(c *gin.Context) {
	var body map[string]any
	if err := readJSON(c, &body); err != nil {
		writeAppError(c, err)
		return
	}
	if s.Repo == nil {
		writeAppError(c, errRepositoryUnavailable())
		return
	}
	total := 0
	for poolName, items := range body {
		if poolName == "" {
			continue
		}
		if _, ok := account.PoolFromName(poolName); !ok {
			continue
		}
		tokenList, _ := items.([]any)
		upserts := []account.Upsert{}
		for _, item := range tokenList {
			var token string
			var tags []string
			switch v := item.(type) {
			case string:
				token = v
			case map[string]any:
				if t, ok := v["token"].(string); ok {
					token = t
				}
				if tagList, ok := v["tags"].([]any); ok {
					for _, t := range tagList {
						if ts, ok := t.(string); ok {
							tags = append(tags, ts)
						}
					}
				}
			}
			token = platform.SanitizeToken(token)
			if token == "" {
				continue
			}
			upserts = append(upserts, account.Upsert{Token: token, Pool: poolName, Tags: account.SortTags(tags)})
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
		_, err := s.Repo.ReplacePool(ctx, poolName, upserts)
		cancel()
		if err != nil {
			writeAppError(c, err)
			return
		}
		total += len(upserts)
	}
	c.JSON(http.StatusOK, gin.H{"status": "success", "count": total})
}

func (s *Server) handleTokensAdd(c *gin.Context) {
	var body struct {
		Tokens []string `json:"tokens"`
		Pool   string   `json:"pool"`
		Tags   []string `json:"tags"`
	}
	if err := readJSON(c, &body); err != nil {
		writeAppError(c, err)
		return
	}
	if s.Repo == nil {
		writeAppError(c, errRepositoryUnavailable())
		return
	}
	pool := body.Pool
	if pool == "" {
		pool = defaultPool
	}
	autoDetect := pool == "auto"
	if autoDetect {
		pool = defaultPool // temporary; will be inferred after refresh
	}
	if _, ok := account.PoolFromName(pool); !ok && !autoDetect {
		writeAppError(c, platform.ValidationErrorCode("Invalid pool '"+pool+"'", "pool", "invalid_pool"))
		return
	}
	tags := account.SortTags(body.Tags)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()

	existing, err := s.Repo.GetAccounts(ctx, body.Tokens)
	if err != nil {
		writeAppError(c, err)
		return
	}
	existingActive := map[string]bool{}
	for _, rec := range existing {
		if rec != nil && !rec.IsDeleted() {
			existingActive[rec.Token] = true
		}
	}
	var newTokens []string
	upserts := []account.Upsert{}
	skipped := 0
	for _, tok := range sanitizeTokenList(body.Tokens) {
		if existingActive[tok] {
			skipped++
			continue
		}
		upserts = append(upserts, account.Upsert{Token: tok, Pool: pool, Tags: tags})
		newTokens = append(newTokens, tok)
	}
	if len(upserts) > 0 {
		if _, err := s.Repo.UpsertAccounts(ctx, upserts); err != nil {
			writeAppError(c, err)
			return
		}
	}

	// Refresh quotas (and optionally infer the pool) for the new tokens in
	// the background; the request does not wait for upstream.
	if len(newTokens) > 0 && s.Refresh != nil {
		s.background("tokens-add-refresh", 120*time.Second, func(ctx context.Context) {
			if _, _, err := s.Refresh.RefreshTokens(ctx, newTokens); err != nil || !autoDetect {
				return
			}
			s.inferPools(ctx, newTokens)
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"status":  "success",
		"count":   len(upserts),
		"skipped": skipped,
	})
}

// inferPools moves freshly refreshed accounts to the pool their quota windows
// say they belong to.
func (s *Server) inferPools(ctx context.Context, tokens []string) {
	recs, _ := s.Repo.GetAccounts(ctx, tokens)
	for _, rec := range recs {
		if rec == nil || rec.IsDeleted() {
			continue
		}
		inferred := account.InferPool(rec.QuotaSet())
		if inferred == "" || inferred == rec.Pool {
			continue
		}
		prev := rec.Pool
		patch := account.Patch{Token: rec.Token, Pool: &inferred}
		if _, err := s.Repo.PatchAccounts(ctx, []account.Patch{patch}); err != nil {
			logger.Warnf("admin auto-detect pool not persisted: token=%s error=%v", platform.MaskToken(rec.Token), err)
			continue
		}
		logger.Infof("admin auto-detect pool: token=%s previous=%s current=%s", platform.MaskToken(rec.Token), prev, inferred)
	}
}

func (s *Server) handleTokensDelete(c *gin.Context) {
	var tokens []string
	if err := readJSON(c, &tokens); err != nil {
		writeAppError(c, err)
		return
	}
	clean := sanitizeTokenList(tokens)
	if len(clean) == 0 {
		writeAppError(c, platform.ValidationError("No valid tokens provided", "tokens"))
		return
	}
	if s.Repo == nil {
		writeAppError(c, errRepositoryUnavailable())
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	result, err := s.Repo.DeleteAccounts(ctx, clean)
	if err != nil {
		writeAppError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": result.Deleted})
}

func (s *Server) handleTokensDeleteInvalid(c *gin.Context) {
	if s.Repo == nil {
		writeAppError(c, errRepositoryUnavailable())
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	recs, err := s.listAllAccounts(ctx)
	if err != nil {
		writeAppError(c, err)
		return
	}
	tokens := []string{}
	for _, rec := range recs {
		if rec.Status == account.StatusActive || rec.Status == account.StatusCooling || rec.Status == account.StatusDisabled {
			continue
		}
		tokens = append(tokens, rec.Token)
	}
	if len(tokens) == 0 {
		c.JSON(http.StatusOK, gin.H{"deleted": 0})
		return
	}
	result, err := s.Repo.DeleteAccounts(ctx, tokens)
	if err != nil {
		writeAppError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": result.Deleted})
}

// handleTokensEdit renames a token and/or moves it to another pool. Omitting
// "pool" keeps the account in its current pool and carries its quota and usage
// state over; naming a different pool re-seeds the quota windows from that
// pool's defaults, as importing the token there would.
func (s *Server) handleTokensEdit(c *gin.Context) {
	var body struct {
		OldToken string `json:"old_token"`
		Token    string `json:"token"`
		Pool     string `json:"pool"`
	}
	if err := readJSON(c, &body); err != nil {
		writeAppError(c, err)
		return
	}
	old := platform.SanitizeToken(body.OldToken)
	newTok := platform.SanitizeToken(body.Token)
	if old == "" || newTok == "" {
		writeAppError(c, platform.ValidationError("Missing token", "body"))
		return
	}
	if body.Pool != "" {
		if _, ok := account.PoolFromName(body.Pool); !ok {
			writeAppError(c, platform.ValidationErrorCode("Invalid pool '"+body.Pool+"'", "pool", "invalid_pool"))
			return
		}
	}
	if s.Repo == nil {
		writeAppError(c, errRepositoryUnavailable())
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	recs, err := s.Repo.GetAccounts(ctx, []string{old, newTok})
	if err != nil {
		writeAppError(c, err)
		return
	}
	var oldRec, newRec *account.Record
	for _, rec := range recs {
		if rec == nil {
			continue
		}
		if rec.Token == old {
			oldRec = rec
		} else if rec.Token == newTok {
			newRec = rec
		}
	}
	if oldRec == nil || oldRec.IsDeleted() {
		writeAppError(c, platform.ValidationErrorCode("Account not found", "old_token", "account_not_found"))
		return
	}
	if old != newTok && newRec != nil && !newRec.IsDeleted() {
		writeAppError(c, platform.NewAppError("Token conflict", platform.ErrValidation, "token_conflict", http.StatusConflict))
		return
	}

	currentPool := oldRec.Pool
	if currentPool == "" {
		currentPool = defaultPool
	}
	pool := body.Pool
	keepState := pool == "" || pool == currentPool
	if pool == "" {
		pool = currentPool
	}
	if keepState && old == newTok {
		// Nothing to change: same token, same pool.
		c.JSON(http.StatusOK, gin.H{"status": "success", "token": newTok, "pool": pool})
		return
	}

	upserts := []account.Upsert{{Token: newTok, Pool: pool, Tags: oldRec.Tags, Ext: oldRec.Ext}}
	if _, err := s.Repo.UpsertAccounts(ctx, upserts); err != nil {
		writeAppError(c, err)
		return
	}
	if keepState {
		if _, err := s.Repo.PatchAccounts(ctx, []account.Patch{carryOverPatch(newTok, oldRec)}); err != nil {
			writeAppError(c, err)
			return
		}
	}
	if old != newTok {
		if _, err := s.Repo.DeleteAccounts(ctx, []string{old}); err != nil {
			writeAppError(c, err)
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"status": "success", "token": newTok, "pool": pool})
}

// carryOverPatch copies the runtime state of rec (status, quota windows and
// usage counters) onto a freshly upserted account.
func carryOverPatch(token string, rec *account.Record) account.Patch {
	p := account.Patch{Token: token}
	st := rec.Status
	p.Status = &st
	p.StateReason = rec.StateReason
	p.LastUseAt = rec.LastUseAt
	p.LastFailAt = rec.LastFailAt
	p.LastFailReason = rec.LastFailReason
	p.LastSyncAt = rec.LastSyncAt
	p.LastClearAt = rec.LastClearAt
	use, fail, sync := rec.UsageUseCount, rec.UsageFailCount, rec.UsageSyncCount
	p.UsageUseDelta, p.UsageFailDelta, p.UsageSyncDelta = &use, &fail, &sync
	window := func(key string) *map[string]any {
		if m, ok := rec.Quota[key].(map[string]any); ok {
			cp := make(map[string]any, len(m))
			for k, v := range m {
				cp[k] = v
			}
			return &cp
		}
		return nil
	}
	p.QuotaAuto = window("auto")
	p.QuotaFast = window("fast")
	p.QuotaExpert = window("expert")
	p.QuotaHeavy = window("heavy")
	p.QuotaGrok43 = window("grok_4_3")
	p.QuotaConsole = window("console")
	return p
}

func (s *Server) handleTokensToggleDisabled(c *gin.Context) {
	var body struct {
		Token    string `json:"token"`
		Disabled bool   `json:"disabled"`
	}
	if err := readJSON(c, &body); err != nil {
		writeAppError(c, err)
		return
	}
	token := platform.SanitizeToken(body.Token)
	if token == "" {
		writeAppError(c, platform.ValidationError("Missing token", "token"))
		return
	}
	if s.Repo == nil {
		writeAppError(c, errRepositoryUnavailable())
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	recs, err := s.Repo.GetAccounts(ctx, []string{token})
	if err != nil {
		writeAppError(c, err)
		return
	}
	if len(recs) == 0 {
		writeAppError(c, platform.ValidationErrorCode("Account not found", "token", "account_not_found"))
		return
	}
	patches := []account.Patch{buildTogglePatch(token, body.Disabled)}
	if _, err := s.Repo.PatchAccounts(ctx, patches); err != nil {
		writeAppError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "success", "token": token, "disabled": body.Disabled})
}

func (s *Server) handleTokensToggleDisabledBatch(c *gin.Context) {
	var body struct {
		Tokens   []string `json:"tokens"`
		Disabled bool     `json:"disabled"`
	}
	if err := readJSON(c, &body); err != nil {
		writeAppError(c, err)
		return
	}
	if s.Repo == nil {
		writeAppError(c, errRepositoryUnavailable())
		return
	}
	clean := sanitizeTokenList(body.Tokens)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	recs, err := s.Repo.GetAccounts(ctx, clean)
	if err != nil {
		writeAppError(c, err)
		return
	}
	if len(recs) == 0 {
		writeAppError(c, platform.ValidationErrorCode("No accounts found", "tokens", "account_not_found"))
		return
	}
	patches := []account.Patch{}
	for _, rec := range recs {
		patches = append(patches, buildTogglePatch(rec.Token, body.Disabled))
	}
	result, err := s.Repo.PatchAccounts(ctx, patches)
	if err != nil {
		writeAppError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status":   "success",
		"disabled": body.Disabled,
		"summary": gin.H{
			"total": len(patches), "ok": result.Patched, "fail": len(patches) - result.Patched,
		},
	})
}

func buildTogglePatch(token string, disabled bool) account.Patch {
	now := platform.NowMs()
	p := account.Patch{Token: token}
	if disabled {
		st := account.StatusDisabled
		p.Status = &st
		reason := "operator_disabled"
		p.StateReason = &reason
		p.ExtMerge = map[string]any{
			"disabled_at":     now,
			"disabled_reason": "operator_disabled",
		}
	} else {
		st := account.StatusActive
		p.Status = &st
		p.ClearFailures = true
	}
	return p
}

func (s *Server) handlePoolReplace(c *gin.Context) {
	var body struct {
		Pool   string   `json:"pool"`
		Tokens []string `json:"tokens"`
		Tags   []string `json:"tags"`
	}
	if err := readJSON(c, &body); err != nil {
		writeAppError(c, err)
		return
	}
	pool := body.Pool
	if pool == "" {
		pool = defaultPool
	}
	if _, ok := account.PoolFromName(pool); !ok {
		writeAppError(c, platform.ValidationErrorCode("Invalid pool '"+pool+"'", "pool", "invalid_pool"))
		return
	}
	if s.Repo == nil {
		writeAppError(c, errRepositoryUnavailable())
		return
	}
	tags := account.SortTags(body.Tags)
	upserts := []account.Upsert{}
	for _, tok := range sanitizeTokenList(body.Tokens) {
		upserts = append(upserts, account.Upsert{Token: tok, Pool: pool, Tags: tags})
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	if _, err := s.Repo.ReplacePool(ctx, pool, upserts); err != nil {
		writeAppError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"pool": pool, "count": len(upserts)})
}
