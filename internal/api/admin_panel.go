package api

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/gentpan/grok2api/internal/account"
	"github.com/gentpan/grok2api/internal/grok"
	"github.com/gentpan/grok2api/internal/model"
	"github.com/gentpan/grok2api/internal/platform"
)

// The admin panel is a dependency-free single page app embedded in the
// binary. Its shell is public; every piece of data it shows comes from the
// authenticated /admin/api endpoints.
//
//go:embed web/index.html web/app.js web/app.css
var panelFS embed.FS

// panelCSP forbids inline script/style and any third-party origin.
const panelCSP = "default-src 'none'; script-src 'self'; style-src 'self'; " +
	"img-src 'self' https: data:; connect-src 'self'; base-uri 'none'; " +
	"form-action 'none'; frame-ancestors 'none'"

// maxTokenTestBatch bounds one /tokens/test request.
const maxTokenTestBatch = 50

// tokenTestConcurrency is how many tokens one /tokens/test checks at once.
const tokenTestConcurrency = 4

// panelAsset serves one embedded file with strict security headers.
func panelAsset(name, contentType string) gin.HandlerFunc {
	data, err := panelFS.ReadFile("web/" + name)
	if err != nil {
		panic("admin panel asset missing: " + name)
	}
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", panelCSP)
		c.Data(http.StatusOK, contentType, data)
	}
}

// modeName maps an upstream quota mode id to a stable display name.
func modeName(id int) string {
	switch id {
	case int(model.ModeAuto):
		return "auto"
	case int(model.ModeFast):
		return "fast"
	case int(model.ModeExpert):
		return "expert"
	case int(model.ModeHeavy):
		return "heavy"
	case int(model.ModeConsole):
		return "console"
	}
	return fmt.Sprintf("mode_%d", id)
}

// handleTokensTest checks whether tokens are accepted upstream and what
// quota they have. It never changes the account pool, so it can be used on
// tokens that have not been imported yet.
func (s *Server) handleTokensTest(c *gin.Context) {
	var body struct {
		Tokens []string `json:"tokens"`
	}
	if err := readJSON(c, &body); err != nil {
		writeAppError(c, err)
		return
	}
	tokens := sanitizeTokenList(body.Tokens)
	if len(tokens) == 0 {
		writeAppError(c, platform.ValidationError("No valid tokens provided", "tokens"))
		return
	}
	if len(tokens) > maxTokenTestBatch {
		writeAppError(c, platform.ValidationError(
			fmt.Sprintf("At most %d tokens per request", maxTokenTestBatch), "tokens"))
		return
	}

	if s.Transport == nil {
		writeAppError(c, errUpstreamUnavailable())
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 90*time.Second)
	defer cancel()

	inPool := map[string]bool{}
	if s.Repo != nil {
		if recs, err := s.Repo.GetAccounts(ctx, tokens); err == nil {
			for _, rec := range recs {
				if rec != nil && !rec.IsDeleted() {
					inPool[rec.Token] = true
				}
			}
		}
	}

	fetcher := s.Transport.UsageFetcher()
	results := make([]gin.H, len(tokens))
	indexes := make([]int, len(tokens))
	for i := range indexes {
		indexes[i] = i
	}
	forEachBounded(indexes, tokenTestConcurrency, func(i int) {
		results[i] = s.testOneToken(ctx, fetcher, i, tokens[i], inPool[tokens[i]])
	})
	c.JSON(http.StatusOK, gin.H{"results": results})
}

func (s *Server) testOneToken(ctx context.Context, f account.UsageFetcher, index int, tok string, inPool bool) gin.H {
	start := time.Now()
	quotas, err := f.FetchAllQuotas(ctx, tok, "basic", true)
	res := gin.H{
		"index":      index,
		"token":      platform.MaskToken(tok),
		"in_pool":    inPool,
		"latency_ms": time.Since(start).Milliseconds(),
	}
	if err != nil && len(quotas) == 0 {
		res["ok"] = false
		res["error"] = err.Error()
		res["invalid"] = looksInvalidCredentials(err)
		return res
	}
	q := gin.H{}
	for id, mq := range quotas {
		q[modeName(id)] = gin.H{"remaining": mq.Remaining, "total": mq.Total}
	}
	res["ok"] = len(quotas) > 0
	res["invalid"] = false
	res["quota"] = q
	if len(quotas) == 0 {
		res["error"] = "no quota information returned"
	}
	return res
}

// looksInvalidCredentials reports whether err means the token itself is bad
// (as opposed to a network or upstream hiccup).
func looksInvalidCredentials(err error) bool {
	if appErr := platformAppError(err); appErr != nil {
		if appErr.Status == http.StatusUnauthorized || appErr.Status == http.StatusForbidden {
			return true
		}
		if platform.IsInvalidCredentialsBody(appErr.Body) {
			return true
		}
	}
	return strings.Contains(strings.ToLower(err.Error()), "invalid credentials")
}

// handleTestModels lists every enabled model with whether the current pool
// can serve it, for the panel's chat tester.
func (s *Server) handleTestModels(c *gin.Context) {
	out := []gin.H{}
	for _, spec := range s.modelRegistry().ListEnabled() {
		capName := modelKind(spec)
		if capName == "web-chat" {
			capName = "chat" // the tester's own vocabulary
		}
		out = append(out, gin.H{
			"id":         spec.ModelName,
			"name":       spec.PublicName,
			"capability": capName,
			"tier":       spec.PoolName(),
			"available":  s.modelAvailable(spec),
		})
	}
	c.JSON(http.StatusOK, gin.H{"models": out})
}

// handleTestChat runs a normal chat completion on behalf of the
// authenticated admin. Without "token" it draws from the shared pool; with
// "token" it uses exactly that SSO token (handy for isolating a bad account).
func (s *Server) handleTestChat(c *gin.Context) {
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		writeAppError(c, platform.ValidationError("Cannot read request body", "body"))
		return
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))

	tok, poolOK := testChatAccess(raw)
	s.chatCompletions(c, Caller{PoolAllowed: poolOK, SSOToken: tok})
}

// testChatAccess decides how a panel chat test is served: with an explicit
// "token" it uses exactly that SSO token and never the shared pool; without
// one it may draw from the pool.
func testChatAccess(raw []byte) (token string, poolAllowed bool) {
	var probe struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(raw, &probe)
	if tok := platform.SanitizeToken(probe.Token); tok != "" {
		return tok, false
	}
	return "", true
}

// subscriptionEndpoint is queried for plan / renewal information. It is the
// endpoint the grok.com web app itself uses; its response shape is not
// documented, so the panel always shows the raw payload next to the parsed
// summary.
const subscriptionEndpoint = "/rest/subscriptions"

// handleTokenInfo asks upstream for the plan/subscription details of one
// token. It is read-only and never changes the account pool.
func (s *Server) handleTokenInfo(c *gin.Context) {
	var body struct {
		Token string `json:"token"`
	}
	if err := readJSON(c, &body); err != nil {
		writeAppError(c, err)
		return
	}
	tok := platform.SanitizeToken(body.Token)
	if tok == "" {
		writeAppError(c, platform.ValidationError("Missing token", "token"))
		return
	}
	if s.Transport == nil {
		writeAppError(c, errUpstreamUnavailable())
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	res := gin.H{"endpoint": subscriptionEndpoint, "token": platform.MaskToken(tok)}
	raw, err := s.Transport.GetJSON(ctx, grok.Base+subscriptionEndpoint, tok)
	if err != nil {
		res["ok"] = false
		res["error"] = err.Error()
		res["invalid"] = looksInvalidCredentials(err)
		c.JSON(http.StatusOK, res)
		return
	}
	res["ok"] = true
	res["raw"] = raw
	res["summary"] = summarizeSubscription(raw)
	c.JSON(http.StatusOK, res)
}

// summarizeSubscription extracts plan / status / date fields from an
// arbitrary subscription payload by key name. It is deliberately heuristic.
func summarizeSubscription(raw map[string]any) []gin.H {
	out := []gin.H{}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch t := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				p := k
				if path != "" {
					p = path + "." + k
				}
				walk(p, t[k])
			}
		case []any:
			for i, e := range t {
				walk(fmt.Sprintf("%s[%d]", path, i), e)
			}
		default:
			if len(out) >= 24 {
				return
			}
			if item := classifySubscriptionField(path, v); item != nil {
				out = append(out, item)
			}
		}
	}
	walk("", raw)
	return out
}

func classifySubscriptionField(path string, v any) gin.H {
	key := strings.ToLower(path)
	if i := strings.LastIndex(key, "."); i >= 0 {
		key = key[i+1:]
	}
	if i := strings.Index(key, "["); i >= 0 {
		key = key[:i]
	}
	label := ""
	isTime := false
	switch {
	case strings.Contains(key, "periodend"), strings.Contains(key, "expir"), strings.Contains(key, "enddate"),
		strings.Contains(key, "validuntil"), strings.Contains(key, "renew"), strings.Contains(key, "endtime"):
		label, isTime = "到期 / 续期时间", true
	case strings.Contains(key, "periodstart"), strings.Contains(key, "startdate"), strings.Contains(key, "starttime"):
		label, isTime = "周期开始时间", true
	case strings.Contains(key, "cancel"):
		label = "取消相关"
	case strings.Contains(key, "tier"), strings.Contains(key, "plan"), strings.Contains(key, "product"):
		label = "套餐 / 等级"
	case key == "status", strings.HasSuffix(key, "status"):
		label = "状态"
	default:
		return nil
	}
	item := gin.H{"label": label, "path": path, "value": v}
	if isTime {
		if ms, ok := toUnixMs(v); ok {
			item["time_ms"] = ms
			item["time"] = time.UnixMilli(ms).UTC().Format(time.RFC3339)
		}
	}
	return item
}

// toUnixMs normalises seconds/milliseconds numbers and RFC3339 strings.
func toUnixMs(v any) (int64, bool) {
	switch t := v.(type) {
	case float64:
		switch {
		case t > 1e12:
			return int64(t), true
		case t > 1e9:
			return int64(t * 1000), true
		}
	case string:
		if ts, err := time.Parse(time.RFC3339, t); err == nil {
			return ts.UnixMilli(), true
		}
	}
	return 0, false
}
