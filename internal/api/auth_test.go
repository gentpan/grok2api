package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/gentpan/grok2api/internal/config"
)

// useConfig points the global config at a temp file for the test.
func useConfig(t *testing.T, toml string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(p, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	config.SetPaths(filepath.Join(dir, "none.toml"), p)
	if err := config.Load(); err != nil {
		t.Fatal(err)
	}
}

func apiEngine(s *Server) (*gin.Engine, *bool) {
	gin.SetMode(gin.TestMode)
	poolOK := new(bool)
	e := gin.New()
	e.Use(s.verifyAPIKey())
	e.GET("/x", func(c *gin.Context) {
		*poolOK = callerFrom(c.Request.Context()).PoolAllowed
		c.Status(http.StatusOK)
	})
	return e, poolOK
}

func TestVerifyAPIKeyPoolAccess(t *testing.T) {
	useConfig(t, "[app]\napi_key = \"good-key\"\n")
	e, poolOK := apiEngine(&Server{})

	cases := []struct {
		name, auth string
		status     int
		pool       bool
	}{
		{"missing", "", http.StatusUnauthorized, false},
		{"wrong token is BYO sso, no pool", "Bearer some-sso", http.StatusOK, false},
		{"matching key uses pool", "Bearer good-key", http.StatusOK, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			*poolOK = false
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			w := httptest.NewRecorder()
			e.ServeHTTP(w, req)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d", w.Code, tc.status)
			}
			if tc.status == http.StatusOK && *poolOK != tc.pool {
				t.Fatalf("pool access = %v, want %v", *poolOK, tc.pool)
			}
		})
	}
}

func TestVerifyAPIKeyOpenMode(t *testing.T) {
	useConfig(t, "[app]\napi_key = \"\"\n")
	e, poolOK := apiEngine(&Server{})
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	if w.Code != http.StatusOK || !*poolOK {
		t.Fatalf("open mode: status=%d pool=%v", w.Code, *poolOK)
	}
}

func adminEngine(s *Server) *gin.Engine {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	e.Use(s.verifyAdminKey())
	e.GET("/a", func(c *gin.Context) { c.Status(http.StatusOK) })
	return e
}

func adminReq(remote, key string, hdr map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/a", nil)
	r.RemoteAddr = remote
	r.Header.Set("Authorization", "Bearer "+key)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	return r
}

func TestAdminDefaultKeyLocalOnly(t *testing.T) {
	useConfig(t, "[app]\napp_key = \"grok2api\"\n")
	e := adminEngine(&Server{})
	do := func(r *http.Request) int {
		w := httptest.NewRecorder()
		e.ServeHTTP(w, r)
		return w.Code
	}
	if c := do(adminReq("127.0.0.1:5000", "grok2api", nil)); c != http.StatusOK {
		t.Fatalf("loopback with default key = %d, want 200", c)
	}
	if c := do(adminReq("203.0.113.9:5000", "grok2api", nil)); c != http.StatusUnauthorized {
		t.Fatalf("remote with default key = %d, want 401", c)
	}
	// A reverse proxy on localhost must not make remote callers look local.
	if c := do(adminReq("127.0.0.1:5000", "grok2api", map[string]string{"X-Forwarded-For": "203.0.113.9"})); c != http.StatusUnauthorized {
		t.Fatalf("proxied request with default key = %d, want 401", c)
	}
}

func TestAdminCustomKeyAndRateLimit(t *testing.T) {
	useConfig(t, "[app]\napp_key = \"a-strong-custom-key\"\n")
	e := adminEngine(&Server{limiter: newFailLimiter(3, time.Minute)})
	do := func(r *http.Request) int {
		w := httptest.NewRecorder()
		e.ServeHTTP(w, r)
		return w.Code
	}
	if c := do(adminReq("203.0.113.9:1", "a-strong-custom-key", nil)); c != http.StatusOK {
		t.Fatalf("custom key remote = %d, want 200", c)
	}
	for i := 0; i < 3; i++ {
		if c := do(adminReq("198.51.100.7:1", "wrong", nil)); c != http.StatusUnauthorized {
			t.Fatalf("wrong key #%d = %d, want 401", i, c)
		}
	}
	// Locked out now, even with the right key.
	if c := do(adminReq("198.51.100.7:1", "a-strong-custom-key", nil)); c != http.StatusTooManyRequests {
		t.Fatalf("after lockout = %d, want 429", c)
	}
	// Other peers are unaffected.
	if c := do(adminReq("203.0.113.9:1", "a-strong-custom-key", nil)); c != http.StatusOK {
		t.Fatalf("other peer = %d, want 200", c)
	}
}

// A matching gateway key must never be forwarded upstream as an SSO token.
func TestGatewayKeyIsNotUsedAsSSOFallback(t *testing.T) {
	useConfig(t, "[app]\napi_key = \"good-key\"\n")
	gin.SetMode(gin.TestMode)
	e := gin.New()
	e.Use((&Server{}).verifyAPIKey())
	var seen string
	e.GET("/x", func(c *gin.Context) {
		seen = callerFrom(c.Request.Context()).SSOToken
		c.Status(http.StatusOK)
	})
	call := func(tok string) string {
		seen = "unset"
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		e.ServeHTTP(httptest.NewRecorder(), r)
		return seen
	}
	if got := call("good-key"); got != "" {
		t.Fatalf("gateway key leaked as api_token: %q", got)
	}
	if got := call("user-own-sso-token"); got != "user-own-sso-token" {
		t.Fatalf("BYO SSO token not preserved: %q", got)
	}
}
