package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// route is one entry of a route table.
type route struct {
	method  string
	path    string
	handler gin.HandlerFunc
}

func register(g gin.IRoutes, routes []route) {
	for _, r := range routes {
		g.Handle(r.method, r.path, r.handler)
	}
}

// Router builds the gin.Engine for the whole API surface.
func (s *Server) Router() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery())
	engine.Use(bodyLimitMiddleware(maxRequestBodyBytes))
	engine.Use(s.configReloadMiddleware())
	engine.Use(corsMiddleware())

	register(engine, s.publicRoutes())
	register(engine.Group("/admin/auth"), s.panelAuthRoutes())

	v1 := engine.Group("/v1", useEnvelope(envOpenAI), s.verifyAPIKey(), s.usageMiddleware())
	register(v1, s.openAIRoutes())

	// Anthropic-compatible endpoints answer errors in Anthropic's envelope.
	msg := engine.Group("/v1/messages", useEnvelope(envAnthropic), s.verifyAPIKey(), s.usageMiddleware())
	register(msg, s.anthropicRoutes())

	admin := engine.Group("/admin/api", useEnvelope(envOpenAI), s.verifyAdminKey())
	register(admin, s.adminRoutes())

	return engine
}

// publicRoutes: health/meta, local media (file IDs are unguessable) and the
// static panel shell (its data comes from /admin/api).
func (s *Server) publicRoutes() []route {
	return []route{
		{http.MethodGet, "/health", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) }},
		{http.MethodGet, "/meta", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"version": Version}) }},
		{http.MethodGet, "/v1/files/image", s.handleFileImage},
		{http.MethodGet, "/v1/files/video", s.handleFileVideo},
		{http.MethodGet, "/admin", panelAsset("index.html", "text/html; charset=utf-8")},
		{http.MethodGet, "/admin/app.js", panelAsset("app.js", "text/javascript; charset=utf-8")},
		{http.MethodGet, "/admin/app.css", panelAsset("app.css", "text/css; charset=utf-8")},
	}
}

// panelAuthRoutes: panel sign-in (public; each handler enforces its own
// checks).
func (s *Server) panelAuthRoutes() []route {
	return []route{
		{http.MethodGet, "/status", s.handleAuthStatus},
		{http.MethodPost, "/setup", s.handleAuthSetup},
		{http.MethodPost, "/login", s.handleAuthLogin},
		{http.MethodPost, "/logout", s.handleAuthLogout},
		{http.MethodPost, "/passkey/login/begin", s.handlePasskeyLoginBegin},
		{http.MethodPost, "/passkey/login/finish", s.handlePasskeyLoginFinish},
	}
}

// openAIRoutes: the OpenAI-compatible API under /v1 (API-key protected).
func (s *Server) openAIRoutes() []route {
	return []route{
		{http.MethodGet, "/models", s.handleModels},
		{http.MethodGet, "/models/:id", s.handleModelGet},
		{http.MethodPost, "/chat/completions", s.handleChatCompletions},
		{http.MethodPost, "/responses", s.handleResponses},
		{http.MethodPost, "/images/generations", s.handleImageGenerations},
		{http.MethodPost, "/images/edits", s.handleImageEdits},
		{http.MethodPost, "/videos", s.handleVideoCreate},
		{http.MethodGet, "/videos/:id", s.handleVideoGet},
		{http.MethodGet, "/videos/:id/content", s.handleVideoGet},
	}
}

// anthropicRoutes: the Anthropic-compatible API (POST /v1/messages).
func (s *Server) anthropicRoutes() []route {
	return []route{
		{http.MethodPost, "", s.handleMessages},
	}
}

// adminRoutes: the panel's JSON API under /admin/api.
func (s *Server) adminRoutes() []route {
	return []route{
		{http.MethodGet, "/verify", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "success"}) }},
		{http.MethodGet, "/config", s.handleConfigGet},
		{http.MethodPost, "/config", s.handleConfigUpdate},
		{http.MethodGet, "/storage", s.handleStorageGet},
		{http.MethodGet, "/status", s.handleStatusGet},
		{http.MethodPost, "/sync", s.handleSync},
		{http.MethodGet, "/usage", s.handleUsageGet},

		{http.MethodGet, "/tokens", s.handleTokensList},
		{http.MethodPost, "/tokens", s.handleTokensReplace},
		{http.MethodPost, "/tokens/add", s.handleTokensAdd},
		{http.MethodPost, "/tokens/test", s.handleTokensTest},
		{http.MethodPost, "/tokens/info", s.handleTokenInfo},
		{http.MethodDelete, "/tokens", s.handleTokensDelete},
		{http.MethodDelete, "/tokens/invalid", s.handleTokensDeleteInvalid},
		{http.MethodPut, "/tokens/edit", s.handleTokensEdit},
		{http.MethodPost, "/tokens/disabled", s.handleTokensToggleDisabled},
		{http.MethodPost, "/tokens/disabled/batch", s.handleTokensToggleDisabledBatch},
		{http.MethodPut, "/pool", s.handlePoolReplace},

		{http.MethodPost, "/batch/nsfw", s.handleBatchNSFW},
		{http.MethodPost, "/batch/refresh", s.handleBatchRefresh},
		{http.MethodPost, "/batch/cache-clear", s.handleBatchCacheClear},
		{http.MethodGet, "/assets", s.handleAssetsList},
		{http.MethodPost, "/assets/delete-item", s.handleAssetsDeleteItem},
		{http.MethodPost, "/assets/clear-token", s.handleAssetsClearToken},

		{http.MethodGet, "/cache", s.handleCacheStats},
		{http.MethodGet, "/cache/list", s.handleCacheList},
		{http.MethodPost, "/cache/clear", s.handleCacheClear},
		{http.MethodPost, "/cache/item/delete", s.handleCacheItemDelete},
		{http.MethodPost, "/cache/items/delete", s.handleCacheItemsDelete},

		{http.MethodGet, "/auth/account", s.handleAccountGet},
		{http.MethodPost, "/auth/account", s.handleAccountUpdate},
		{http.MethodPost, "/auth/passkey/register/begin", s.handlePasskeyRegisterBegin},
		{http.MethodPost, "/auth/passkey/register/finish", s.handlePasskeyRegisterFinish},
		{http.MethodDelete, "/auth/passkey/:id", s.handlePasskeyDelete},

		{http.MethodGet, "/models", s.handleModelsAdminList},
		{http.MethodPost, "/models", s.handleModelAdd},
		{http.MethodPost, "/models/discover", s.handleModelsDiscover},
		{http.MethodDelete, "/models/:id", s.handleModelDelete},
		{http.MethodPost, "/models/:id/enabled", s.handleModelEnabled},

		{http.MethodGet, "/keys", s.handleKeysList},
		{http.MethodPost, "/keys", s.handleKeysCreate},
		{http.MethodDelete, "/keys/:id", s.handleKeysDelete},

		{http.MethodGet, "/test/models", s.handleTestModels},
		{http.MethodPost, "/test/chat", s.handleTestChat},
	}
}
