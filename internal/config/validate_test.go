package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validateSnapshot(t *testing.T, user string) *Snapshot {
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
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	return s
}

func hasWarning(ws []string, substr string) bool {
	for _, w := range ws {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

func TestValidateSecurityDefaults(t *testing.T) {
	ws := validateSnapshot(t, "").Validate()
	if !hasWarning(ws, "app.app_key") {
		t.Errorf("missing default-admin-key warning: %v", ws)
	}
	if !hasWarning(ws, "app.api_key") {
		t.Errorf("missing open-API warning: %v", ws)
	}

	ws = validateSnapshot(t, "[app]\napp_key = \"s3cret\"\napi_key = \"k1,k2\"\n").Validate()
	if len(ws) != 0 {
		t.Errorf("secure config produced warnings: %v", ws)
	}
}

func TestValidateRanges(t *testing.T) {
	user := `
[app]
app_key = "s3cret"
api_key = "k"
[account.refresh]
enabled = true
basic_interval_sec = 0
super_interval_sec = -5
heavy_interval_sec = "soon"
[asset]
upload_timeout = 0
[retry]
max_retries = -1
on_codes = "429,abc,700"
reset_session_status_codes = [403, "x"]
[cache.local]
image_max_mb = -1
[proxy.egress]
proxy_url = "127.0.0.1:7890"
`
	ws := validateSnapshot(t, user).Validate()
	for _, want := range []string{
		"account.refresh.basic_interval_sec: 0 must be >= 1",
		"account.refresh.super_interval_sec: -5 must be >= 1",
		`account.refresh.heavy_interval_sec: "soon" is not an integer`,
		"asset.upload_timeout: 0 must be >= 1",
		"retry.max_retries: -1 must not be negative",
		`retry.on_codes: "abc"`,
		`retry.on_codes: "700"`,
		`retry.reset_session_status_codes: "x"`,
		"cache.local.image_max_mb: -1 must not be negative",
		"proxy.egress.proxy_url",
	} {
		if !hasWarning(ws, want) {
			t.Errorf("missing warning containing %q in:\n%s", want, strings.Join(ws, "\n"))
		}
	}
	if hasWarning(ws, `"429"`) {
		t.Errorf("valid code 429 was flagged: %v", ws)
	}
}

// Refresh intervals only matter when quota refresh is enabled.
func TestValidateIntervalsIgnoredWhenRefreshDisabled(t *testing.T) {
	user := "[app]\napp_key = \"s\"\napi_key = \"k\"\n[account.refresh]\nenabled = false\nbasic_interval_sec = 0\n"
	if ws := validateSnapshot(t, user).Validate(); len(ws) != 0 {
		t.Fatalf("unexpected warnings: %v", ws)
	}
}
