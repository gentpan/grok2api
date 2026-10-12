package grok

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	fhttp "github.com/bogdanfinn/fhttp"

	"github.com/gentpan/grok2api/internal/config"
	"github.com/gentpan/grok2api/internal/platform"
)

// proxyProfile is the per-request identity that every upstream request is
// built from: browser user-agent, Cloudflare clearance, the x-* anti-bot
// cookies and the statsig settings. It is resolved from config once per
// request (resolveProxyProfile) and then handed to the header builders below,
// so a single request never observes a half-reloaded configuration and the
// builders themselves stay pure.
type proxyProfile struct {
	UserAgent   string
	CFCookies   string
	CFClearance string

	DeviceID   string // grok_device_id; derived per token when empty
	AnonUserID string // x-anonuserid
	Challenge  string // x-challenge
	Signature  string // x-signature
	UserID     string // x-userid

	StatsigID       string // full manual x-statsig-id override
	StatsigSeed     string // genuine (seed, HEX) pair override
	StatsigHEX      string
	StatsigFromHTML bool // refresh the pair from grok.com's HTML
}

// profileSource is the slice of config.Snapshot the profile needs.
type profileSource interface {
	GetStr(key, def string) string
	GetBool(key string, def bool) bool
}

// resolveProxyProfile snapshots the current configuration.
func resolveProxyProfile() proxyProfile { return profileFromSource(config.Global()) }

func profileFromSource(cfg profileSource) proxyProfile {
	str := func(key string) string { return strings.TrimSpace(cfg.GetStr(key, "")) }
	cookies := cfg.GetStr("proxy.clearance.cf_cookies", "")
	clearance := strings.TrimSpace(extractCookieValue(cookies, "cf_clearance"))
	if clearance == "" {
		clearance = str("proxy.clearance.cf_clearance")
	}
	return proxyProfile{
		UserAgent:   str("proxy.clearance.user_agent"),
		CFCookies:   cookies,
		CFClearance: clearance,

		DeviceID:   str("proxy.clearance.device_id"),
		AnonUserID: str("proxy.clearance.x_anonuserid"),
		Challenge:  str("proxy.clearance.x_challenge"),
		Signature:  str("proxy.clearance.x_signature"),
		UserID:     str("proxy.clearance.x_userid"),

		StatsigID:       str("proxy.clearance.statsig_id"),
		StatsigSeed:     str("proxy.clearance.statsig_seed"),
		StatsigHEX:      str("proxy.clearance.statsig_hex"),
		StatsigFromHTML: cfg.GetBool("proxy.clearance.statsig_from_html", false),
	}
}

// ua returns the effective user-agent. This is the single place the
// "empty means DefaultUserAgent" fallback lives.
func (p proxyProfile) ua() string {
	if ua := strings.TrimSpace(p.UserAgent); ua != "" {
		return ua
	}
	return DefaultUserAgent
}

// userID returns the x-userid known without a network call: the configured
// value, else one embedded in cf_cookies. Empty when neither is present.
func (p proxyProfile) userID() string {
	if p.UserID != "" {
		return p.UserID
	}
	return extractCookieValue(p.CFCookies, "x-userid")
}

// --- cookies ---------------------------------------------------------------

var (
	cookieValueRE = regexp.MustCompile(`(?:^|;\s*)cf_clearance=([^;]*)`)
	// cfClearanceReplaceRE captures the leading separator so it can be re-emitted.
	cfClearanceReplaceRE = regexp.MustCompile(`(^|;\s*)cf_clearance=[^;]*`)
)

// extractCookieValue returns the value of cookie name in a Cookie header
// ("" when absent). Names match whole, never as a suffix of another name.
func extractCookieValue(cookieHeader, name string) string {
	if cookieHeader == "" {
		return ""
	}
	for _, part := range strings.Split(cookieHeader, ";") {
		part = strings.TrimSpace(part)
		if v, ok := strings.CutPrefix(part, name+"="); ok {
			return v
		}
	}
	return ""
}

// cookieToken normalises an SSO token for use as a cookie value. On top of
// platform.SanitizeToken it drops ';' and control characters, so a stored
// token can never smuggle extra cookie attributes into the Cookie header.
func cookieToken(raw string) string {
	tok := platform.SanitizeToken(raw)
	return strings.Map(func(r rune) rune {
		if r == ';' || r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, tok)
}

// deviceIDFor returns the grok_device_id to present for token: the configured
// value when set, otherwise a UUID derived from the token. Deriving keeps the
// id stable across requests (a real browser's device id never changes) while
// still differing between accounts.
func (p proxyProfile) deviceIDFor(token string) string {
	if p.DeviceID != "" {
		return p.DeviceID
	}
	sum := sha256.Sum256([]byte("grok2api/device-id/" + token))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x40 // version 4 layout
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// buildSSOCookie builds the Cookie header value for an SSO-authenticated
// grok.com request. It includes all cookies observed in a browser: sso,
// sso-rw, grok_device_id, x-anonuserid, x-challenge, x-signature, x-userid,
// cf_clearance, __cf_bm and the other cf cookies.
func buildSSOCookie(ssoToken string, p proxyProfile) string {
	tok := cookieToken(ssoToken)
	parts := []string{"sso=" + tok, "sso-rw=" + tok, "grok_device_id=" + p.deviceIDFor(tok)}
	for _, kv := range [...]struct{ name, value string }{
		{"x-anonuserid", p.AnonUserID},
		{"x-challenge", p.Challenge},
		{"x-signature", p.Signature},
		{"x-userid", p.UserID},
	} {
		if kv.value != "" {
			parts = append(parts, kv.name+"="+kv.value)
		}
	}

	cfCookies := strings.TrimSpace(p.CFCookies)
	clearance := strings.TrimSpace(p.CFClearance)
	switch {
	case clearance != "" && cfCookies != "":
		if cookieValueRE.MatchString(cfCookies) {
			// ${1} keeps the "; " separator that precedes the cookie; dropping
			// it would glue cf_clearance onto the previous cookie's value.
			cfCookies = cfClearanceReplaceRE.ReplaceAllString(cfCookies, "${1}cf_clearance="+strings.ReplaceAll(clearance, "$", "$$"))
		} else {
			cfCookies = strings.TrimRight(cfCookies, "; ") + "; cf_clearance=" + clearance
		}
	case clearance != "":
		cfCookies = "cf_clearance=" + clearance
	}
	if cfCookies != "" {
		parts = append(parts, cfCookies)
	}
	return strings.Join(parts, "; ")
}

// buildGatewayCookie is the SSO cookie with x-userid forced to userID (the
// Gateway authenticates the uid query parameter against it).
func buildGatewayCookie(token string, p proxyProfile, userID string) string {
	parts := strings.Split(buildSSOCookie(token, p), ";")
	filtered := make([]string, 0, len(parts)+1)
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || strings.HasPrefix(strings.ToLower(part), "x-userid=") {
			continue
		}
		filtered = append(filtered, part)
	}
	if userID = strings.TrimSpace(userID); userID != "" {
		filtered = append(filtered, "x-userid="+userID)
	}
	return strings.Join(filtered, "; ")
}

// consoleCookie is the minimal cookie console.x.ai needs.
func (p proxyProfile) consoleCookie(token string) string {
	tok := cookieToken(token)
	cookie := "sso=" + tok + "; sso-rw=" + tok
	if p.CFClearance != "" {
		cookie += "; cf_clearance=" + p.CFClearance
	}
	return cookie
}

// htmlCookie is the cookie sent with the anonymous grok.com HTML fetch.
func (p proxyProfile) htmlCookie() string {
	cookie := strings.TrimSpace(p.CFCookies)
	if p.CFClearance != "" && extractCookieValue(cookie, "cf_clearance") == "" {
		if cookie != "" {
			cookie += "; "
		}
		cookie += "cf_clearance=" + p.CFClearance
	}
	return cookie
}

// --- headers ---------------------------------------------------------------

const (
	acceptEncoding  = "gzip, deflate, br, zstd"
	defaultJSONType = "application/json"
)

// httpHeaders builds the headers for a grok.com REST request.
//
// Only the minimal set grok's anti-bot requires is sent — Content-Type,
// User-Agent, Cookie (sso + cf_clearance + anti-bot tokens) and x-statsig-id.
// Extra browser-fingerprint headers (sentry-trace, Baggage, Sec-Ch-Ua*, Origin,
// Referer ...) are omitted because grok detects mismatches between these and a
// real browser session, causing code:7 anti-bot rejection. The minimal set was
// verified to pass anti-bot (HTTP 404 code:5 = "Model not found", confirming
// acceptance). That is also why WithOrigin / WithReferer have no effect on
// these hosts.
func (p proxyProfile) httpHeaders(token, contentType, statsigID string) http.Header {
	if contentType == "" {
		contentType = defaultJSONType
	}
	h := http.Header{}
	h.Set("Content-Type", contentType)
	h.Set("User-Agent", p.ua())
	h.Set("Accept-Encoding", acceptEncoding)
	h.Set("x-statsig-id", statsigID)
	h.Set("Cookie", buildSSOCookie(token, p))
	return h
}

// consoleHeaders builds the headers for a console.x.ai request.
func (p proxyProfile) consoleHeaders(token, contentType string) http.Header {
	if contentType == "" {
		contentType = defaultJSONType
	}
	ua := p.ua()
	h := http.Header{}
	h.Set("Accept", "*/*")
	h.Set("Accept-Encoding", acceptEncoding)
	h.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	h.Set("Content-Type", contentType)
	h.Set("Cookie", p.consoleCookie(token))
	h.Set("Origin", "https://console.x.ai")
	h.Set("Priority", "u=1, i")
	h.Set("Referer", "https://console.x.ai/")
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set("Sec-Fetch-Site", "same-origin")
	h.Set("User-Agent", ua)
	h.Set("x-cluster", "https://us-east-1.api.x.ai")
	for k, v := range clientHints(ua) {
		h.Set(k, v)
	}
	return h
}

// plainHeaders is the credential-free header set for hosts outside the
// trusted domains: never SSO/cf cookies, never statsig.
func (p proxyProfile) plainHeaders(contentType string, hasBody bool) http.Header {
	if contentType == "" {
		contentType = defaultJSONType
	}
	h := http.Header{}
	h.Set("User-Agent", p.ua())
	h.Set("Accept-Encoding", acceptEncoding)
	if hasBody {
		h.Set("Content-Type", contentType)
	}
	return h
}

// mediaHeaders is the header set for downloading a media file from a CDN.
func (p proxyProfile) mediaHeaders() http.Header {
	h := http.Header{}
	h.Set("Accept", "video/*,*/*;q=0.8")
	h.Set("User-Agent", p.ua())
	return h
}

// htmlHeaders is the header set of a top-level navigation to grok.com.
func (p proxyProfile) htmlHeaders() http.Header {
	ua := p.ua()
	h := http.Header{}
	h.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8")
	h.Set("Accept-Encoding", acceptEncoding)
	h.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Pragma", "no-cache")
	h.Set("Sec-Fetch-Dest", "document")
	h.Set("Sec-Fetch-Mode", "navigate")
	h.Set("Sec-Fetch-Site", "none")
	h.Set("Sec-Fetch-User", "?1")
	h.Set("Upgrade-Insecure-Requests", "1")
	h.Set("User-Agent", ua)
	for k, v := range clientHints(ua) {
		h.Set(k, v)
	}
	if cookie := p.htmlCookie(); cookie != "" {
		h.Set("Cookie", cookie)
	}
	return h
}

// gatewayHeaders builds the browser-like WebSocket handshake headers for the
// text-chat Gateway.
func (p proxyProfile) gatewayHeaders(origin, userID, token string) fhttp.Header {
	h := fhttp.Header{}
	h.Set("Origin", origin)
	h.Set("Accept-Language", "zh-CN,zh;q=0.9,en-US;q=0.8,en;q=0.7")
	h.Set("Cache-Control", "no-cache")
	h.Set("Pragma", "no-cache")
	h.Set("User-Agent", p.ua())
	h.Set("Cookie", buildGatewayCookie(token, p, userID))
	return h
}

// imagineHeaders builds the WebSocket handshake headers for the imagine stream.
func (p proxyProfile) imagineHeaders(token string) fhttp.Header {
	ua := p.ua()
	h := fhttp.Header{}
	h.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Origin", "https://grok.com")
	h.Set("Pragma", "no-cache")
	h.Set("User-Agent", ua)
	h.Set("Cookie", buildSSOCookie(token, p))
	for k, v := range clientHints(ua) {
		h.Set(k, v)
	}
	return h
}

// pathOf returns the URL path (statsig is computed over the pathname only).
func pathOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Path
}
