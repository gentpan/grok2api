package app

import (
	"strings"
	"testing"
	"time"
)

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestParseEnvDefaults(t *testing.T) {
	env, warns := ParseEnv(envFrom(nil))
	if len(warns) != 0 {
		t.Fatalf("warnings = %v", warns)
	}
	if env.Addr() != "0.0.0.0:8000" {
		t.Fatalf("addr = %q", env.Addr())
	}
	if env.LogLevel != "INFO" || !env.FileLogging {
		t.Fatalf("logging = %q file=%v", env.LogLevel, env.FileLogging)
	}
	if env.SyncIdle != 30*time.Second || env.SyncActive != 3*time.Second {
		t.Fatalf("sync = %v / %v", env.SyncIdle, env.SyncActive)
	}
}

func TestParseEnvOverrides(t *testing.T) {
	env, warns := ParseEnv(envFrom(map[string]string{
		"SERVER_HOST":                  " 127.0.0.1 ",
		"SERVER_PORT":                  "9000",
		"LOG_LEVEL":                    "debug",
		"LOG_FILE_ENABLED":             "off",
		"ACCOUNT_SYNC_INTERVAL":        "60",
		"ACCOUNT_SYNC_ACTIVE_INTERVAL": "1",
		"CONFIG_PATH":                  "/x/config.toml",
		"ACCOUNT_LOCAL_PATH":           "/x/accounts.jsonl",
	}))
	if len(warns) != 0 {
		t.Fatalf("warnings = %v", warns)
	}
	if env.Addr() != "127.0.0.1:9000" || env.LogLevel != "debug" || env.FileLogging {
		t.Fatalf("env = %+v", env)
	}
	if env.SyncIdle != time.Minute || env.SyncActive != time.Second {
		t.Fatalf("sync = %v / %v", env.SyncIdle, env.SyncActive)
	}
	if env.UserPath != "/x/config.toml" || env.AccountPath != "/x/accounts.jsonl" {
		t.Fatalf("paths = %q %q", env.UserPath, env.AccountPath)
	}
}

// A zero or negative sync interval used to make the directory poll spin in a
// tight loop; it must fall back to the default and say so.
func TestParseEnvRejectsNonPositiveIntervals(t *testing.T) {
	for _, v := range []string{"0", "-5", "abc"} {
		env, warns := ParseEnv(envFrom(map[string]string{
			"ACCOUNT_SYNC_INTERVAL":        v,
			"ACCOUNT_SYNC_ACTIVE_INTERVAL": v,
		}))
		if env.SyncIdle != 30*time.Second || env.SyncActive != 3*time.Second {
			t.Fatalf("value %q: sync = %v / %v, want defaults", v, env.SyncIdle, env.SyncActive)
		}
		if len(warns) != 2 {
			t.Fatalf("value %q: warnings = %v", v, warns)
		}
	}
}

func TestParseEnvWarnsAboutUnsupportedStorage(t *testing.T) {
	_, warns := ParseEnv(envFrom(map[string]string{"ACCOUNT_STORAGE": "redis"}))
	if len(warns) != 1 || !strings.Contains(warns[0], "redis") {
		t.Fatalf("warnings = %v", warns)
	}
	if _, w := ParseEnv(envFrom(map[string]string{"ACCOUNT_STORAGE": "local"})); len(w) != 0 {
		t.Fatalf("local storage warned: %v", w)
	}
}

func TestFileLogLevelIsCappedByAnExplicitLogLevel(t *testing.T) {
	cases := []struct {
		name       string
		env        map[string]string
		configured string
		want       string
	}{
		{"no LOG_LEVEL: file level is the configured one", nil, "DEBUG", "DEBUG"},
		{"LOG_LEVEL=WARN caps an INFO file level", map[string]string{"LOG_LEVEL": "WARN"}, "INFO", "WARN"},
		{"a stricter file level is kept", map[string]string{"LOG_LEVEL": "INFO"}, "ERROR", "ERROR"},
		{"unset file level follows LOG_LEVEL", map[string]string{"LOG_LEVEL": "WARN"}, "", "WARN"},
		{"unknown names pass through", map[string]string{"LOG_LEVEL": "loud"}, "INFO", "INFO"},
	}
	for _, c := range cases {
		env, _ := ParseEnv(envFrom(c.env))
		if got := env.FileLogLevel(c.configured); got != c.want {
			t.Errorf("%s: FileLogLevel(%q) = %q, want %q", c.name, c.configured, got, c.want)
		}
	}
}
