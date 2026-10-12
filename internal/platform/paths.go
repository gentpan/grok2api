// Package platform collects cross-cutting platform helpers: data/log paths,
// the application error type, token sanitising/masking, atomic file writes
// and the clock. It imports no other project package.
package platform

import (
	"os"
	"path/filepath"
	"strings"
)

// DataDir returns the configured data root (env DATA_DIR, default ./data).
func DataDir() string {
	d := os.Getenv("DATA_DIR")
	if d == "" {
		d = "./data"
	}
	return d
}

// DataPath joins a relative path under the data directory.
func DataPath(parts ...string) string {
	return filepath.Join(append([]string{DataDir()}, parts...)...)
}

// LogDir returns the configured log directory (env LOG_DIR, default ./logs).
func LogDir() string {
	d := os.Getenv("LOG_DIR")
	if d == "" {
		d = "./logs"
	}
	return d
}

// ProjectRoot returns the directory of the running binary's working dir.
func ProjectRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

// SanitizeToken normalizes an SSO token the same way the Python project does:
// translate Unicode dash/space/zero-width variants to ASCII, strip all
// whitespace, strip a leading "sso=" prefix, keep ASCII-only.
func SanitizeToken(raw string) string {
	if raw == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range raw {
		switch r {
		case ' ', '\t', '\n', '\r', '\u00a0', '\u200b', '\u200c', '\u200d',
			'\u2060', '\ufeff', '\u3000':
			continue
		case '‐', '‑', '‒', '–', '—', '―', '－':
			b.WriteByte('-')
		case '“', '”', '‘', '’':
			b.WriteByte('"')
		default:
			if r < 128 {
				b.WriteRune(r)
			}
		}
	}
	s := b.String()
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "sso=")
	return s
}

// MaskToken returns a log-safe rendering of an SSO token: tokens of 8 or fewer
// characters are fully hidden ("***"); longer tokens keep the first 4 and last
// 4 characters ("abcd...wxyz"). It never panics, whatever the input length.
func MaskToken(token string) string {
	r := []rune(token)
	if len(r) <= 8 {
		return "***"
	}
	return string(r[:4]) + "..." + string(r[len(r)-4:])
}

// NowMs returns the current time in milliseconds since epoch.
func NowMs() int64 {
	return timeNow().UnixMilli()
}
