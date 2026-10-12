package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// defaultAdminKey is the built-in admin password shipped in the defaults file.
const defaultAdminKey = "grok2api"

// Validate inspects the effective configuration and returns human-readable
// problems, one per entry (empty when everything looks fine). None of them
// stops the process: callers typically log each entry at WARN level.
//
// It covers security posture (default admin key, open /v1 API), numeric
// ranges (non-positive intervals/timeouts, negative limits), values that the
// getters would silently ignore (non-numeric retry codes, non-integer
// numbers) and malformed URLs.
func (s *Snapshot) Validate() []string {
	_ = s.Load()
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }

	// Security posture.
	if k := strings.TrimSpace(s.GetStr("app.app_key", "")); k == "" || k == defaultAdminKey {
		add("SECURITY: app.app_key is empty or the built-in default; the admin API only accepts localhost requests until a custom app.app_key is set")
	}
	if len(s.GetList("app.api_key", nil)) == 0 {
		add("SECURITY: app.api_key is empty; /v1 endpoints are open and anyone who can reach this server can use the account pool")
	}
	if st := strings.ToLower(strings.TrimSpace(os.Getenv("ACCOUNT_STORAGE"))); st != "" && st != "local" {
		add("ACCOUNT_STORAGE=%s is not implemented in this build; accounts are stored in the local JSONL file", st)
	}

	// Strictly positive integers: intervals and timeouts. A zero interval
	// makes refresh loops spin; a zero timeout fails every upstream call.
	if s.GetBool("account.refresh.enabled", false) {
		for _, k := range []string{
			"account.refresh.basic_interval_sec",
			"account.refresh.super_interval_sec",
			"account.refresh.heavy_interval_sec",
		} {
			s.checkInt(&out, k, 1)
		}
	}
	for _, k := range []string{
		"asset.upload_timeout",
		"asset.list_timeout",
		"asset.delete_timeout",
		"nsfw.timeout",
		"account.selection.max_inflight",
	} {
		s.checkInt(&out, k, 1)
	}
	// Non-negative integers where 0 is meaningful.
	for _, k := range []string{
		"retry.max_retries",
		"cache.local.image_max_mb",
		"cache.local.video_max_mb",
	} {
		s.checkInt(&out, k, 0)
	}

	// HTTP status code lists.
	for _, k := range []string{"retry.on_codes", "retry.reset_session_status_codes"} {
		for _, c := range s.GetList(k, nil) {
			if n, err := strconv.Atoi(strings.TrimSpace(c)); err != nil || n < 100 || n > 599 {
				add("%s: %q is not an HTTP status code and will be ignored", k, c)
			}
		}
	}

	// URLs.
	if p := strings.TrimSpace(s.GetStr("proxy.egress.proxy_url", "")); p != "" {
		if u, err := url.Parse(p); err != nil || u.Host == "" {
			add("proxy.egress.proxy_url: %q is not a valid proxy URL (expected scheme://host:port)", p)
		}
	}
	if o := strings.TrimSpace(s.GetStr("app.public_origin", "")); o != "" {
		if u, err := url.Parse(o); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			add("app.public_origin: %q must be an absolute http(s) origin such as https://example.com", o)
		}
	}
	return out
}

// checkInt appends a message to out when key is set but is not an integer or
// is below min. Unset keys are fine: the code-side default applies.
func (s *Snapshot) checkInt(out *[]string, key string, min int) {
	v := s.getScalar(key)
	var n int
	switch t := v.(type) {
	case nil:
		return
	case int64:
		n = int(t)
	case int:
		n = t
	case float64:
		n = int(t)
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			*out = append(*out, fmt.Sprintf("%s: %q is not an integer; the built-in default is used instead", key, t))
			return
		}
		n = parsed
	default:
		*out = append(*out, fmt.Sprintf("%s: unexpected value type %T; the built-in default is used instead", key, v))
		return
	}
	if n < min {
		if min >= 1 {
			*out = append(*out, fmt.Sprintf("%s: %d must be >= %d", key, n, min))
		} else {
			*out = append(*out, fmt.Sprintf("%s: %d must not be negative", key, n))
		}
	}
}

// Validate validates the process-global snapshot; see Snapshot.Validate.
func Validate() []string { return global.Validate() }
