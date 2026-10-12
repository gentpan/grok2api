package grok

import (
	"fmt"
	"regexp"
	"strings"
)

var chromeVersionRE = regexp.MustCompile(`[Cc]hrome/(\d{2,3})`)

// chromeMajor returns the Chrome major version from a user-agent ("" when the
// UA does not carry one).
func chromeMajor(ua string) string {
	if m := chromeVersionRE.FindStringSubmatch(ua); len(m) > 1 {
		return m[1]
	}
	return ""
}

// clientHints returns the Sec-Ch-Ua-* request headers matching ua. The version
// comes from the UA itself so User-Agent and Sec-Ch-Ua always agree. It returns
// nil for non-Chromium user agents.
//
// Chrome's reduced user-agent keeps the real major version and zeroes the rest
// (Chrome/148.0.0.0), and Sec-Ch-Ua carries that same major: v="148".
func clientHints(ua string) map[string]string {
	u := strings.ToLower(ua)
	isChromium := strings.Contains(u, "chrome") || strings.Contains(u, "chromium") || strings.Contains(u, "edg")
	if !isChromium || strings.Contains(u, "firefox") ||
		(strings.Contains(u, "safari") && !strings.Contains(u, "chrome")) {
		return nil
	}
	major := chromeMajor(u)
	if major == "" {
		return nil
	}
	plat := platformFromUA(u)
	arch := archFromUA(u)
	mobile := "?0"
	if strings.Contains(u, "mobile") || plat == "Android" || plat == "iOS" {
		mobile = "?1"
	}

	hints := map[string]string{
		"Sec-Ch-Ua":                  fmt.Sprintf(`"Google Chrome";v="%s", "Chromium";v="%s", "Not/A)Brand";v="99"`, major, major),
		"Sec-Ch-Ua-Mobile":           mobile,
		"Sec-Ch-Ua-Model":            `""`,
		"Sec-Ch-Ua-Full-Version":     fmt.Sprintf(`"%s.0.0.0"`, major),
		"Sec-Ch-Ua-Platform-Version": `"13.0.0"`,
	}
	if plat != "" {
		hints["Sec-Ch-Ua-Platform"] = fmt.Sprintf(`"%s"`, plat)
	}
	if arch != "" {
		hints["Sec-Ch-Ua-Arch"] = fmt.Sprintf(`"%s"`, arch)
		hints["Sec-Ch-Ua-Bitness"] = `"64"`
	}
	return hints
}

func platformFromUA(u string) string {
	switch {
	case strings.Contains(u, "windows"):
		return "Windows"
	case strings.Contains(u, "mac os x") || strings.Contains(u, "macintosh"):
		return "macOS"
	case strings.Contains(u, "android"):
		return "Android"
	case strings.Contains(u, "iphone") || strings.Contains(u, "ipad"):
		return "iOS"
	case strings.Contains(u, "linux"):
		return "Linux"
	}
	return ""
}

func archFromUA(u string) string {
	switch {
	case strings.Contains(u, "aarch64") || strings.Contains(u, "arm"):
		return "arm"
	case strings.Contains(u, "x86_64") || strings.Contains(u, "x64") ||
		strings.Contains(u, "win64") || strings.Contains(u, "intel"):
		return "x86"
	}
	return ""
}
