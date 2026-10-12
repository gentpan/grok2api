package config

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

const envTestDefaults = `
[app]
app_key = "grok2api"
api_key = ""

[features]
stream = true

[account.refresh]
enabled = true
basic_interval_sec = 86400

[account.selection]
max_inflight = 8

[proxy.egress]
proxy_url = ""

[proxy.clearance]
cf_cookies = ""
`

func envSnapshot(t *testing.T, user string) *Snapshot {
	t.Helper()
	dir := t.TempDir()
	s := &Snapshot{
		defaultsPath: filepath.Join(dir, "defaults.toml"),
		userPath:     filepath.Join(dir, "config.toml"),
	}
	writeFile(t, s.defaultsPath, envTestDefaults, time.Now().Add(-time.Hour))
	if user != "" {
		writeFile(t, s.userPath, user, time.Now().Add(-time.Hour))
	}
	return s
}

// 3-level keys could not be set from the environment before: the first-
// underscore rule produced account."refresh_enabled".
func TestEnvOverlayThreeLevelKeys(t *testing.T) {
	t.Setenv("GROK_ACCOUNT_REFRESH_ENABLED", "false")
	t.Setenv("GROK_ACCOUNT_SELECTION_MAX_INFLIGHT", "3")
	t.Setenv("GROK_PROXY_EGRESS_PROXY_URL", "http://127.0.0.1:7890")
	t.Setenv("GROK_PROXY_CLEARANCE_CF_COOKIES", "a=b")
	t.Setenv("GROK_FEATURES_STREAM", "false") // 2-level keeps working
	t.Setenv("GROK_APP_APP_KEY", "secret")    // underscore inside the leaf name

	s := envSnapshot(t, "")
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	if s.GetBool("account.refresh.enabled", true) {
		t.Error("account.refresh.enabled not overridden")
	}
	if got := s.GetInt("account.selection.max_inflight", 0); got != 3 {
		t.Errorf("max_inflight = %d", got)
	}
	if got := s.GetStr("proxy.egress.proxy_url", ""); got != "http://127.0.0.1:7890" {
		t.Errorf("proxy_url = %q", got)
	}
	if got := s.GetStr("proxy.clearance.cf_cookies", ""); got != "a=b" {
		t.Errorf("cf_cookies = %q", got)
	}
	if s.GetBool("features.stream", true) {
		t.Error("features.stream not overridden")
	}
	if got := s.GetStr("app.app_key", ""); got != "secret" {
		t.Errorf("app_key = %q", got)
	}
	// No stray legacy-style key was created.
	if v := s.Get("account.refresh_enabled", nil); v != nil {
		t.Errorf("legacy split produced account.refresh_enabled = %v", v)
	}
	if v := s.Get("account.selection_max_inflight", nil); v != nil {
		t.Errorf("legacy split produced account.selection_max_inflight = %v", v)
	}
}

func TestEnvOverlayUnknownKeys(t *testing.T) {
	// Unknown leaf inside a known table: longest table prefix wins, rest of
	// the name (with underscores) is the key.
	t.Setenv("GROK_PROXY_CLEARANCE_NEW_THING", "x")
	// Entirely unknown: legacy first-underscore rule.
	t.Setenv("GROK_BRAND_NEW_KEY", "y")
	// Exactly a known table: must not clobber the table with a string.
	t.Setenv("GROK_PROXY_CLEARANCE", "z")
	// No underscore at all: ignored.
	t.Setenv("GROK_LONELY", "q")

	s := envSnapshot(t, "")
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	if got := s.GetStr("proxy.clearance.new_thing", ""); got != "x" {
		t.Errorf("proxy.clearance.new_thing = %q", got)
	}
	if got := s.GetStr("brand.new_key", ""); got != "y" {
		t.Errorf("brand.new_key = %q", got)
	}
	if _, ok := s.Get("proxy.clearance", nil).(map[string]any); !ok {
		t.Errorf("proxy.clearance table was clobbered: %v", s.Get("proxy.clearance", nil))
	}
	if s.Get("lonely", nil) != nil {
		t.Error("name without underscore must be ignored")
	}
}

// Keys defined only in the user file are "known" too.
func TestEnvOverlayKnowsUserFileKeys(t *testing.T) {
	t.Setenv("GROK_CUSTOM_DEEP_LEAF_VALUE", "env")
	s := envSnapshot(t, "[custom.deep]\nleaf_value = \"file\"\n")
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	if got := s.GetStr("custom.deep.leaf_value", ""); got != "env" {
		t.Errorf("custom.deep.leaf_value = %q, want env", got)
	}
}

func TestOverriddenByEnv(t *testing.T) {
	t.Setenv("GROK_ACCOUNT_REFRESH_ENABLED", "false")
	s := envSnapshot(t, "")
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	keys := []string{"account.refresh.enabled", "account.refresh.basic_interval_sec", "features.stream"}
	got := s.OverriddenByEnv(keys)
	if !reflect.DeepEqual(got, []string{"account.refresh.enabled"}) {
		t.Fatalf("OverriddenByEnv = %v", got)
	}
	// A table containing an overridden key is reported as well.
	if got := s.OverriddenByEnv([]string{"account.refresh"}); len(got) != 1 {
		t.Fatalf("table key not reported: %v", got)
	}
	if got := s.OverriddenByEnv([]string{"account.selection.max_inflight"}); len(got) != 0 {
		t.Fatalf("unexpected override: %v", got)
	}
}
