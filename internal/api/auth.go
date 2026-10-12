package api

import (
	"crypto/subtle"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/gentpan/grok2api/internal/platform"
)

// defaultAdminKey is the historical built-in admin password.
const defaultAdminKey = "grok2api"

// adminViaKey is the gin context key recording how an admin request was
// authenticated ("session" or "key").
const adminViaKey = "auth_via"

// extractBearer returns the token from an "Authorization: Bearer <x>" header.
func extractBearer(header string) string {
	header = strings.TrimSpace(header)
	if header == "" {
		return ""
	}
	const prefix = "Bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

// apiToken extracts the client-supplied token of an API request.
func apiToken(r *http.Request) string {
	token := extractBearer(r.Header.Get("Authorization"))
	if token == "" {
		token = strings.TrimSpace(r.Header.Get("x-api-key"))
	}
	return token
}

// verifyAPIKey middleware. When a gateway key is configured (config.toml or
// panel-managed), a request must carry a token: a token matching a key may use
// the shared account pool; any other token is treated as the caller's own SSO
// token and is never allowed to draw from the pool. With no key anywhere the
// API is in open mode.
//
// The resulting Caller is stored in the request context (callerFrom).
func (s *Server) verifyAPIKey() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := apiToken(c.Request)
		cfgKeys := s.configAPIKeys()
		managed := s.keyStore()

		set := func(caller Caller) {
			c.Request = c.Request.WithContext(withCaller(c.Request.Context(), caller))
		}

		// Open mode: no key anywhere → allow everything. The bearer token, if
		// any, doubles as the caller's own SSO token for pool-empty fallback.
		if len(cfgKeys) == 0 && managed.count() == 0 {
			set(Caller{PoolAllowed: true, SSOToken: token})
			c.Next()
			return
		}

		// A key is configured → the caller must provide a token.
		if token == "" {
			writeAppError(c, platform.AuthError("Missing or invalid Authorization header."))
			c.Abort()
			return
		}
		// Match against the configured keys (constant time).
		matchedID := ""
		for _, k := range cfgKeys {
			if subtle.ConstantTimeCompare([]byte(token), []byte(k)) == 1 {
				matchedID = keyID(k)
			}
		}
		if entry := managed.match(token); entry != nil {
			matchedID = keyID(entry.Key)
			managed.touch(entry)
		}
		if matchedID != "" {
			// A gateway key is a secret of this service, never an upstream SSO
			// token: it must not be forwarded to grok.com as a fallback.
			set(Caller{PoolAllowed: true, KeyID: matchedID})
		} else {
			// Bring-your-own SSO token: allowed, but only for itself.
			set(Caller{PoolAllowed: false, SSOToken: token})
		}
		c.Next()
	}
}

// verifyAdminKey is the admin-key middleware.
// Accepts the key via Authorization: Bearer <key> OR ?app_key=<key> query.
func (s *Server) verifyAdminKey() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Panel login: a valid session cookie. Unsafe methods must come from
		// the panel's own origin (CSRF guard) on top of SameSite=Strict.
		if sess := s.authStore().sessionFor(c.Request); sess != nil {
			if !isSafeMethod(c.Request.Method) && !s.sameOrigin(c.Request) {
				writeAppError(c, platform.NewAppError("Cross-origin request rejected.", platform.ErrAuthentication, "csrf_rejected", http.StatusForbidden))
				c.Abort()
				return
			}
			c.Set(adminViaKey, "session")
			c.Next()
			return
		}
		key := strings.TrimSpace(s.config().GetStr("app.app_key", ""))
		if key == "" {
			writeAppError(c, platform.AuthError("Admin key is not configured."))
			c.Abort()
			return
		}
		if key == defaultAdminKey && !isLocalDirectRequest(c.Request) {
			writeAppError(c, platform.AuthError("Admin API is restricted to localhost while app.app_key is the built-in default; set a custom app.app_key."))
			c.Abort()
			return
		}
		ip := clientIP(c.Request)
		lim := s.adminLimiter()
		if lim.blocked(ip) {
			writeAppError(c, platform.NewAppError("Too many failed attempts, try again later.", platform.ErrRateLimit, "too_many_attempts", http.StatusTooManyRequests))
			c.Abort()
			return
		}
		token := extractBearer(c.GetHeader("Authorization"))
		if token == "" {
			token = strings.TrimSpace(c.Query("app_key"))
		}
		if token == "" {
			writeAppError(c, platform.AuthError("Missing authentication token."))
			c.Abort()
			return
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(key)) != 1 {
			lim.fail(ip)
			writeAppError(c, platform.AuthError("Invalid authentication token."))
			c.Abort()
			return
		}
		c.Set(adminViaKey, "key")
		c.Next()
	}
}

// remoteHost returns the TCP peer address (never X-Forwarded-For, which a
// client can forge).
func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// isLocalDirectRequest is true for a loopback peer that did not come through
// a reverse proxy (no forwarding headers).
func isLocalDirectRequest(r *http.Request) bool {
	ip := net.ParseIP(remoteHost(r))
	if ip == nil || !ip.IsLoopback() {
		return false
	}
	for _, h := range []string{"X-Forwarded-For", "X-Real-Ip", "Forwarded", "X-Forwarded-Host"} {
		if r.Header.Get(h) != "" {
			return false
		}
	}
	return true
}

// Defaults of the admin brute-force limiter.
const (
	adminFailMax    = 10
	adminFailWindow = 5 * time.Minute
	// limiterSweepSize is the table size above which every fail() also prunes
	// the other peers, so a scan from many addresses cannot grow it forever.
	limiterSweepSize = 1024
)

// failLimiter blocks a peer after too many failed admin authentications.
type failLimiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	max    int
	window time.Duration
}

func newFailLimiter(max int, window time.Duration) *failLimiter {
	if max <= 0 {
		max = adminFailMax
	}
	if window <= 0 {
		window = adminFailWindow
	}
	return &failLimiter{hits: map[string][]time.Time{}, max: max, window: window}
}

func (l *failLimiter) prune(ip string, now time.Time) []time.Time {
	kept := l.hits[ip][:0]
	for _, t := range l.hits[ip] {
		if now.Sub(t) < l.window {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.hits, ip)
		return nil
	}
	l.hits[ip] = kept
	return kept
}

func (l *failLimiter) blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prune(ip, time.Now())) >= l.max
}

func (l *failLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if len(l.hits) > limiterSweepSize {
		for other := range l.hits {
			l.prune(other, now)
		}
	}
	l.prune(ip, now)
	l.hits[ip] = append(l.hits[ip], now)
}
