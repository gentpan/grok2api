package api

import (
	"context"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/gentpan/grok2api/internal/account"
	"github.com/gentpan/grok2api/internal/grok"
	"github.com/gentpan/grok2api/internal/jsonstore"
	"github.com/gentpan/grok2api/internal/logger"
	"github.com/gentpan/grok2api/internal/model"
	"github.com/gentpan/grok2api/internal/platform"
)

var modelIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{1,79}$`)

// customModelEntry is a model added from the panel.
type customModelEntry struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"` // "console-chat" | "web-chat"
	Mode      string `json:"mode,omitempty"`
	Tier      string `json:"tier,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

type modelStoreFile struct {
	Custom   []customModelEntry `json:"custom"`
	Disabled []string           `json:"disabled"`
}

// modelStore persists panel-made model changes and replays them into a model
// registry at startup. The file format is {"custom":[...],"disabled":[...]}.
type modelStore struct {
	f *jsonstore.File[modelStoreFile]
}

func newModelStore(path string) *modelStore {
	f, err := jsonstore.Open(path, jsonstore.Config[modelStoreFile]{
		Indent: "  ",
		OnError: func(err error) {
			logger.Warnf("model store persist failed: error=%v", err)
		},
	})
	if err != nil {
		logger.Warnf("model store unreadable, starting empty: error=%v", err)
	}
	return &modelStore{f: f}
}

// path is the backing file (tests reload from it).
func (m *modelStore) path() string { return m.f.Path() }

// apply registers stored custom models and disables stored ids in reg.
func (m *modelStore) apply(reg ModelRegistry) {
	var data modelStoreFile
	m.f.View(func(d *modelStoreFile) {
		data.Custom = append([]customModelEntry(nil), d.Custom...)
		data.Disabled = append([]string(nil), d.Disabled...)
	})
	for _, e := range data.Custom {
		spec, err := specFromEntry(e)
		if err != nil {
			logger.Warnf("custom model skipped: id=%s error=%v", e.ID, err)
			continue
		}
		if err := reg.Register(spec); err != nil && err != model.ErrModelExists {
			logger.Warnf("custom model not registered: id=%s error=%v", e.ID, err)
		}
	}
	for _, id := range data.Disabled {
		reg.SetEnabled(id, false)
	}
}

// addCustom persists a new custom model entry.
func (m *modelStore) addCustom(e customModelEntry) error {
	return m.f.Update(func(d *modelStoreFile) error {
		d.Custom = append(d.Custom, e)
		return nil
	})
}

// removeCustom persists the deletion of a custom model (and forgets any
// disabled flag it carried).
func (m *modelStore) removeCustom(id string) error {
	return m.f.Update(func(d *modelStoreFile) error {
		kept := make([]customModelEntry, 0, len(d.Custom))
		for _, e := range d.Custom {
			if e.ID != id {
				kept = append(kept, e)
			}
		}
		d.Custom = kept
		d.Disabled = withoutString(d.Disabled, id)
		return nil
	})
}

// setEnabled persists the enabled flag of a model (built-in or custom).
func (m *modelStore) setEnabled(id string, enabled bool) error {
	return m.f.Update(func(d *modelStoreFile) error {
		d.Disabled = withoutString(d.Disabled, id)
		if !enabled {
			d.Disabled = append(d.Disabled, id)
		}
		return nil
	})
}

func withoutString(list []string, drop string) []string {
	out := make([]string, 0, len(list))
	for _, v := range list {
		if v != drop {
			out = append(out, v)
		}
	}
	return out
}

var webModes = map[string]model.ModeId{
	"auto": model.ModeAuto, "fast": model.ModeFast, "expert": model.ModeExpert, "heavy": model.ModeHeavy,
}

func specFromEntry(e customModelEntry) (model.Spec, error) {
	if !modelIDRE.MatchString(e.ID) {
		return model.Spec{}, errString("模型名需为 2–80 位字母、数字或 . _ : -，且以字母数字开头")
	}
	name := strings.TrimSpace(e.Name)
	if name == "" {
		name = e.ID
	}
	spec := model.Spec{ModelName: e.ID, PublicName: name, Enabled: true}
	switch e.Kind {
	case "console-chat":
		spec.ModeId = model.ModeConsole
		spec.Tier = model.TierBasic
		spec.Capability = model.CapConsoleChat
	case "web-chat":
		mode, ok := webModes[e.Mode]
		if !ok {
			return model.Spec{}, errString("网页聊天模型需要选择模式：auto / fast / expert / heavy")
		}
		tier, ok := model.TierFromName(e.Tier)
		if !ok {
			return model.Spec{}, errString("账号池档位只能是 basic / super / heavy")
		}
		spec.ModeId = mode
		spec.Tier = tier
		spec.Capability = model.CapChat
	default:
		return model.Spec{}, errString("类型只能是 console-chat 或 web-chat")
	}
	return spec, nil
}

type errString string

func (e errString) Error() string { return string(e) }

// modelKind is the display kind of a registry entry.
func modelKind(s *model.Spec) string {
	switch {
	case s.IsConsoleChat():
		return "console-chat"
	case s.IsImageEdit():
		return "image-edit"
	case s.IsImage():
		return "image"
	case s.IsVideo():
		return "video"
	case s.IsChat():
		return "web-chat"
	}
	return "other"
}

func (s *Server) handleModelsAdminList(c *gin.Context) {
	out := []gin.H{}
	for _, sp := range s.modelRegistry().All() {
		out = append(out, gin.H{
			"id": sp.ModelName, "name": sp.PublicName, "kind": modelKind(sp),
			"mode": sp.ModeId.ApiStr(), "tier": sp.Tier.Name(),
			"builtin": !sp.Custom, "enabled": sp.Enabled, "available": s.modelAvailable(sp),
		})
	}
	c.JSON(http.StatusOK, gin.H{"models": out})
}

func (s *Server) handleModelAdd(c *gin.Context) {
	var e customModelEntry
	if err := readJSON(c, &e); err != nil {
		writeAppError(c, err)
		return
	}
	e.ID = strings.TrimSpace(e.ID)
	spec, err := specFromEntry(e)
	if err != nil {
		writeAppError(c, platform.ValidationError(err.Error(), "id"))
		return
	}
	reg := s.modelRegistry()
	if err := reg.Register(spec); err != nil {
		if err == model.ErrModelExists {
			writeAppError(c, platform.NewAppError("模型名已存在："+e.ID, platform.ErrValidation, "model_exists", http.StatusConflict))
			return
		}
		writeAppError(c, platform.ValidationError(err.Error(), "id"))
		return
	}
	e.CreatedAt = time.Now().UnixMilli()
	if err := s.customModels().addCustom(e); err != nil {
		reg.Remove(e.ID)
		writeAppError(c, platform.NewAppError("保存失败："+err.Error(), platform.ErrServer, "model_store_failed", http.StatusInternalServerError))
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "success", "id": e.ID})
}

func (s *Server) handleModelDelete(c *gin.Context) {
	id := c.Param("id")
	reg := s.modelRegistry()
	if cur := reg.Get(id); cur == nil || !cur.Custom {
		writeAppError(c, platform.ValidationErrorCode("只能删除自定义模型", "id", "not_custom"))
		return
	}
	// Persist first: a model that is gone from memory but still on disk would
	// silently come back after a restart.
	if err := s.customModels().removeCustom(id); err != nil {
		writeAppError(c, platform.NewAppError("保存失败："+err.Error(), platform.ErrServer, "model_store_failed", http.StatusInternalServerError))
		return
	}
	reg.Remove(id)
	c.JSON(http.StatusOK, gin.H{"status": "success"})
}

func (s *Server) handleModelEnabled(c *gin.Context) {
	id := c.Param("id")
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := readJSON(c, &body); err != nil {
		writeAppError(c, err)
		return
	}
	reg := s.modelRegistry()
	if reg.Get(id) == nil {
		writeAppError(c, platform.ValidationErrorCode("模型不存在", "id", "model_not_found"))
		return
	}
	if err := s.customModels().setEnabled(id, body.Enabled); err != nil {
		writeAppError(c, platform.NewAppError("保存失败："+err.Error(), platform.ErrServer, "model_store_failed", http.StatusInternalServerError))
		return
	}
	reg.SetEnabled(id, body.Enabled)
	c.JSON(http.StatusOK, gin.H{"status": "success", "enabled": body.Enabled})
}

// parseModelIDs pulls model ids out of the shapes model-list endpoints use:
// {"data":[{"id":..}]}, {"models":[{"id"|"name"|"model":..}]} or string lists.
func parseModelIDs(raw map[string]any) []string {
	seen := map[string]bool{}
	var ids []string
	add := func(v any) {
		switch t := v.(type) {
		case string:
			if modelIDRE.MatchString(t) && !seen[t] {
				seen[t] = true
				ids = append(ids, t)
			}
		case map[string]any:
			for _, k := range []string{"id", "name", "model"} {
				if s, ok := t[k].(string); ok && modelIDRE.MatchString(s) {
					if !seen[s] {
						seen[s] = true
						ids = append(ids, s)
					}
					break
				}
			}
		}
	}
	for _, key := range []string{"data", "models"} {
		if arr, ok := raw[key].([]any); ok {
			for _, v := range arr {
				add(v)
			}
		}
	}
	sort.Strings(ids)
	return ids
}

// consoleModelsEndpoint is the OpenAI-style model list of the Console API.
const consoleModelsEndpoint = "/v1/models"

// handleModelsDiscover asks upstream which models exist and marks the ones
// this gateway does not know yet. The response shape of the upstream list is
// not documented, so the result says how it was obtained and what came back.
func (s *Server) handleModelsDiscover(c *gin.Context) {
	var body struct {
		Token string `json:"token"`
	}
	_ = c.ShouldBindJSON(&body)
	token := platform.SanitizeToken(body.Token)

	ctx, cancel := context.WithTimeout(c.Request.Context(), 40*time.Second)
	defer cancel()

	if token == "" && s.Repo != nil {
		if page, err := s.Repo.ListAccounts(ctx, account.ListQuery{Page: 1, PageSize: 200}); err == nil {
			for _, rec := range page.Items {
				if rec.Status == account.StatusActive {
					token = rec.Token
					break
				}
			}
		}
	}
	if token == "" {
		writeAppError(c, platform.NewAppError("账号池里没有可用账号，无法向上游查询模型列表。请先导入账号。",
			platform.ErrValidation, "no_account", http.StatusConflict))
		return
	}

	if s.Transport == nil {
		writeAppError(c, errUpstreamUnavailable())
		return
	}
	res := gin.H{"source": "console.x.ai" + consoleModelsEndpoint, "account": platform.MaskToken(token)}
	raw, err := s.Transport.GetJSON(ctx, grok.ConsoleBase+consoleModelsEndpoint, token,
		grok.WithConsoleMode(), grok.WithTimeout(30*time.Second))
	if err != nil {
		res["ok"] = false
		res["error"] = err.Error()
		c.JSON(http.StatusOK, res)
		return
	}
	ids := parseModelIDs(raw)
	list := make([]gin.H, 0, len(ids))
	newCount := 0
	for _, id := range ids {
		known := s.modelRegistry().Get(id) != nil
		if !known {
			newCount++
		}
		list = append(list, gin.H{"id": id, "known": known})
	}
	res["ok"] = true
	res["models"] = list
	res["new_count"] = newCount
	if len(ids) == 0 {
		res["raw"] = raw
	}
	c.JSON(http.StatusOK, res)
}
