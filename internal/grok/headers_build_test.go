package grok

import (
	"strings"
	"testing"
)

const chrome148UA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36"

func TestHTTPHeadersMinimalSet(t *testing.T) {
	prof := proxyProfile{UserAgent: "UA/9", CFCookies: "__cf_bm=bm", CFClearance: "clr"}
	h := prof.httpHeaders("my-sso", "", "sid-value")

	want := map[string]string{
		"Content-Type":    "application/json",
		"User-Agent":      "UA/9",
		"Accept-Encoding": "gzip, deflate, br, zstd",
		"X-Statsig-Id":    "sid-value",
	}
	for k, v := range want {
		if got := h.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	cookie := h.Get("Cookie")
	for _, w := range []string{"sso=my-sso", "sso-rw=my-sso", "grok_device_id=", "__cf_bm=bm", "cf_clearance=clr"} {
		if !strings.Contains(cookie, w) {
			t.Errorf("Cookie lacks %q: %q", w, cookie)
		}
	}
	// The minimal set is deliberate (see proxyProfile.httpHeaders): no browser
	// hints and no Origin/Referer.
	for _, absent := range []string{"Origin", "Referer", "Sec-Ch-Ua", "Accept", "Sec-Fetch-Mode"} {
		if v := h.Get(absent); v != "" {
			t.Errorf("unexpected header %s = %q", absent, v)
		}
	}
}

func TestEmptyProfileFallsBackToDefaultUserAgent(t *testing.T) {
	prof := proxyProfile{}
	if got := prof.httpHeaders("tok", "text/plain", "s").Get("User-Agent"); got != DefaultUserAgent {
		t.Errorf("http User-Agent = %q", got)
	}
	if got := prof.consoleHeaders("tok", "").Get("User-Agent"); got != DefaultUserAgent {
		t.Errorf("console User-Agent = %q", got)
	}
	if got := prof.plainHeaders("", false).Get("User-Agent"); got != DefaultUserAgent {
		t.Errorf("plain User-Agent = %q", got)
	}
	if got := prof.mediaHeaders().Get("User-Agent"); got != DefaultUserAgent {
		t.Errorf("media User-Agent = %q", got)
	}
	if got := prof.htmlHeaders().Get("User-Agent"); got != DefaultUserAgent {
		t.Errorf("html User-Agent = %q", got)
	}
	if got := prof.gatewayHeaders("https://grok.com", "u", "tok").Get("User-Agent"); got != DefaultUserAgent {
		t.Errorf("gateway User-Agent = %q", got)
	}
	if got := prof.imagineHeaders("tok").Get("User-Agent"); got != DefaultUserAgent {
		t.Errorf("imagine User-Agent = %q", got)
	}
	// Hints are derived from the effective UA even when the profile is empty.
	if prof.consoleHeaders("tok", "").Get("Sec-Ch-Ua") == "" {
		t.Error("console headers lack client hints for the default UA")
	}
	if prof.httpHeaders("tok", "text/plain", "s").Get("Content-Type") != "text/plain" {
		t.Error("explicit content type ignored")
	}
}

func TestConsoleHeadersSet(t *testing.T) {
	prof := proxyProfile{UserAgent: chrome148UA, CFClearance: "clr"}
	h := prof.consoleHeaders("tok", "")
	want := map[string]string{
		"Accept":          "*/*",
		"Accept-Encoding": "gzip, deflate, br, zstd",
		"Content-Type":    "application/json",
		"Cookie":          "sso=tok; sso-rw=tok; cf_clearance=clr",
		"Origin":          "https://console.x.ai",
		"Referer":         "https://console.x.ai/",
		"Sec-Fetch-Dest":  "empty",
		"Sec-Fetch-Mode":  "cors",
		"Sec-Fetch-Site":  "same-origin",
		"User-Agent":      chrome148UA,
		"X-Cluster":       "https://us-east-1.api.x.ai",
		"Priority":        "u=1, i",
	}
	for k, v := range want {
		if got := h.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if h.Get("Sec-Ch-Ua-Platform") != `"Windows"` {
		t.Errorf("platform hint = %q", h.Get("Sec-Ch-Ua-Platform"))
	}
}

func TestPlainHeadersCarryNoCredentials(t *testing.T) {
	prof := proxyProfile{UserAgent: "UA", CFCookies: "a=b", CFClearance: "x", UserID: "u"}
	h := prof.plainHeaders("application/json", true)
	for k := range h {
		switch k {
		case "User-Agent", "Accept-Encoding", "Content-Type":
		default:
			t.Errorf("unexpected header %q", k)
		}
	}
	if prof.plainHeaders("application/json", false).Get("Content-Type") != "" {
		t.Error("Content-Type set on a bodyless request")
	}
}

func TestSSOCookieFromProfile(t *testing.T) {
	prof := proxyProfile{DeviceID: "dev-123", UserID: "u-1", AnonUserID: "anon", Challenge: "ch", Signature: "sig"}
	got := buildSSOCookie("tok", prof)
	want := "sso=tok; sso-rw=tok; grok_device_id=dev-123; x-anonuserid=anon; x-challenge=ch; x-signature=sig; x-userid=u-1"
	if got != want {
		t.Fatalf("cookie = %q\nwant     %q", got, want)
	}
}

func TestSSOCookieDeviceIDIsStablePerToken(t *testing.T) {
	prof := proxyProfile{}
	a1, a2 := buildSSOCookie("token-a", prof), buildSSOCookie("token-a", prof)
	b := buildSSOCookie("token-b", prof)
	if a1 != a2 {
		t.Fatalf("grok_device_id changed between calls: %q vs %q", a1, a2)
	}
	if a1 == b {
		t.Fatal("different accounts share one derived grok_device_id")
	}
	id := extractCookieValue(a1, "grok_device_id")
	if len(id) != 36 || id[14] != '4' {
		t.Fatalf("device id %q is not a v4-shaped uuid", id)
	}
	// A configured id always wins.
	if got := buildSSOCookie("token-a", proxyProfile{DeviceID: "fixed"}); !strings.Contains(got, "grok_device_id=fixed") {
		t.Fatalf("cookie = %q", got)
	}
}

func TestCookieTokenStripsAttributeInjection(t *testing.T) {
	got := buildSSOCookie("good; Path=/; evil=1\r\nX: y", proxyProfile{DeviceID: "d"})
	if strings.Count(got, ";") != 2 { // sso; sso-rw; grok_device_id — exactly 2 separators
		t.Fatalf("token injected extra cookie attributes: %q", got)
	}
	if !strings.HasPrefix(got, "sso=goodPath=/evil=1X:y; sso-rw=") {
		t.Fatalf("cookie = %q", got)
	}
}

func TestProfileFromConfig(t *testing.T) {
	useConfig(t, `[proxy.clearance]
user_agent = "  Custom/1  "
cf_cookies = "a=1; cf_clearance=fromcookies; x-userid=cookie-user"
cf_clearance = "fromkey"
device_id = "dev"
x_anonuserid = "anon"
x_challenge = "ch"
x_signature = "sig"
x_userid = "cfg-user"
statsig_id = "fixed-sid"
statsig_seed = "seed"
statsig_hex = "hex"
statsig_from_html = true
`)
	p := resolveProxyProfile()
	want := proxyProfile{
		UserAgent: "Custom/1", CFCookies: "a=1; cf_clearance=fromcookies; x-userid=cookie-user", CFClearance: "fromcookies",
		DeviceID: "dev", AnonUserID: "anon", Challenge: "ch", Signature: "sig", UserID: "cfg-user",
		StatsigID: "fixed-sid", StatsigSeed: "seed", StatsigHEX: "hex", StatsigFromHTML: true,
	}
	if p != want {
		t.Fatalf("profile = %#v\nwant      %#v", p, want)
	}
	if p.userID() != "cfg-user" {
		t.Fatalf("userID = %q", p.userID())
	}

	useConfig(t, "[proxy.clearance]\ncf_cookies = \"x-userid=from-cookie\"\ncf_clearance = \"only-key\"\n")
	p = resolveProxyProfile()
	if p.CFClearance != "only-key" || p.userID() != "from-cookie" || p.ua() != DefaultUserAgent {
		t.Fatalf("fallback profile = %#v (userID %q)", p, p.userID())
	}
}

func TestClientHintsTable(t *testing.T) {
	cases := []struct {
		name     string
		ua       string
		wantNil  bool
		major    string
		platform string
		arch     string
		mobile   string
	}{
		{"windows chrome", chrome148UA, false, "148", `"Windows"`, `"x86"`, "?0"},
		{"mac chrome", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36", false, "146", `"macOS"`, `"x86"`, "?0"},
		{"linux chrome", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36", false, "120", `"Linux"`, `"x86"`, "?0"},
		{"three digit", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/99.0.0.0 Safari/537.36", false, "99", `"Linux"`, `"x86"`, "?0"},
		{"android mobile", "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Mobile Safari/537.36", false, "130", `"Android"`, "", "?1"},
		{"firefox", "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:130.0) Gecko/20100101 Firefox/130.0", true, "", "", "", ""},
		{"safari", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15", true, "", "", "", ""},
		{"empty", "", true, "", "", "", ""},
		{"no version", "Mozilla/5.0 Chrome", true, "", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := clientHints(tc.ua)
			if tc.wantNil {
				if h != nil {
					t.Fatalf("hints = %#v, want nil", h)
				}
				return
			}
			if h == nil {
				t.Fatal("hints = nil")
			}
			// Sec-Ch-Ua must carry the same major version as the UA string.
			wantUA := `"Google Chrome";v="` + tc.major + `", "Chromium";v="` + tc.major + `", "Not/A)Brand";v="99"`
			if h["Sec-Ch-Ua"] != wantUA {
				t.Errorf("Sec-Ch-Ua = %q, want %q", h["Sec-Ch-Ua"], wantUA)
			}
			if h["Sec-Ch-Ua-Full-Version"] != `"`+tc.major+`.0.0.0"` {
				t.Errorf("Sec-Ch-Ua-Full-Version = %q", h["Sec-Ch-Ua-Full-Version"])
			}
			if h["Sec-Ch-Ua-Platform"] != tc.platform {
				t.Errorf("platform = %q, want %q", h["Sec-Ch-Ua-Platform"], tc.platform)
			}
			if h["Sec-Ch-Ua-Arch"] != tc.arch {
				t.Errorf("arch = %q, want %q", h["Sec-Ch-Ua-Arch"], tc.arch)
			}
			if h["Sec-Ch-Ua-Mobile"] != tc.mobile {
				t.Errorf("mobile = %q, want %q", h["Sec-Ch-Ua-Mobile"], tc.mobile)
			}
			if h["Sec-Ch-Ua-Model"] != `""` {
				t.Errorf("model = %q", h["Sec-Ch-Ua-Model"])
			}
		})
	}
}

func TestPlatformAndArchFromUA(t *testing.T) {
	if platformFromUA("iphone; cpu iphone os") != "iOS" || platformFromUA("something") != "" {
		t.Fatal("platformFromUA")
	}
	if archFromUA("aarch64") != "arm" || archFromUA("win64; x64") != "x86" || archFromUA("zzz") != "" {
		t.Fatal("archFromUA")
	}
	if chromeMajor("chrome/148.0.0.0") != "148" || chromeMajor("firefox/1") != "" {
		t.Fatal("chromeMajor")
	}
}

func TestExtractCookieValue(t *testing.T) {
	c := "a=1; cf_clearance=xyz; b=2"
	if extractCookieValue(c, "cf_clearance") != "xyz" || extractCookieValue(c, "a") != "1" || extractCookieValue(c, "b") != "2" {
		t.Fatal("extractCookieValue")
	}
	if extractCookieValue(c, "missing") != "" || extractCookieValue("", "a") != "" {
		t.Fatal("extractCookieValue missing")
	}
	// A name must match whole, not as a suffix of another cookie name.
	if extractCookieValue("xcf_clearance=1", "cf_clearance") != "" {
		t.Fatal("suffix match")
	}
}

func TestPathOf(t *testing.T) {
	if pathOf("https://grok.com/rest/a?b=1") != "/rest/a" || pathOf("") != "" || pathOf("https://grok.com") != "" {
		t.Fatal("pathOf")
	}
}
