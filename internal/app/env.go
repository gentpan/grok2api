package app

import (
	"strconv"
	"strings"
	"time"
)

// Env is the process environment the service reads at startup. It is parsed
// once, in one place, with defaults and range checks, instead of being sprinkled
// through main.
type Env struct {
	Host string // SERVER_HOST
	Port string // SERVER_PORT

	LogLevel    string // LOG_LEVEL
	LogLevelSet bool   // LOG_LEVEL was given explicitly
	FileLogging bool   // LOG_FILE_ENABLED

	// SyncIdle / SyncActive are the account directory poll intervals once the
	// directory went quiet / while changes are still arriving
	// (ACCOUNT_SYNC_INTERVAL / ACCOUNT_SYNC_ACTIVE_INTERVAL, seconds).
	SyncIdle   time.Duration
	SyncActive time.Duration

	// DefaultsPath / UserPath override the config file locations
	// (CONFIG_DEFAULTS_PATH / CONFIG_PATH); empty means "derive".
	DefaultsPath string
	UserPath     string

	// AccountPath overrides the account JSONL location (ACCOUNT_LOCAL_PATH).
	AccountPath string
}

const (
	defaultSyncIdleSec   = 30
	defaultSyncActiveSec = 3
)

// ParseEnv reads Env through getenv (os.Getenv in production). Invalid values
// fall back to their default and are reported in warnings; it never fails.
func ParseEnv(getenv func(string) string) (env Env, warnings []string) {
	get := func(k string) string { return strings.TrimSpace(getenv(k)) }
	str := func(k, def string) string {
		if v := get(k); v != "" {
			return v
		}
		return def
	}
	seconds := func(k string, def int) time.Duration {
		v := get(k)
		if v == "" {
			return time.Duration(def) * time.Second
		}
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			warnings = append(warnings, k+"="+v+" is not a positive number of seconds; using "+strconv.Itoa(def))
			return time.Duration(def) * time.Second
		}
		return time.Duration(n) * time.Second
	}

	env.Host = str("SERVER_HOST", "0.0.0.0")
	env.Port = str("SERVER_PORT", "8000")
	env.LogLevel = str("LOG_LEVEL", "INFO")
	env.LogLevelSet = get("LOG_LEVEL") != ""
	env.FileLogging = parseBool(get("LOG_FILE_ENABLED"), true)
	env.SyncIdle = seconds("ACCOUNT_SYNC_INTERVAL", defaultSyncIdleSec)
	env.SyncActive = seconds("ACCOUNT_SYNC_ACTIVE_INTERVAL", defaultSyncActiveSec)
	env.DefaultsPath = get("CONFIG_DEFAULTS_PATH")
	env.UserPath = get("CONFIG_PATH")
	env.AccountPath = get("ACCOUNT_LOCAL_PATH")

	if st := strings.ToLower(get("ACCOUNT_STORAGE")); st != "" && st != "local" {
		warnings = append(warnings, "ACCOUNT_STORAGE="+st+" is not implemented in this build; accounts are stored in the local JSONL file")
	}
	return env, warnings
}

// Addr is the HTTP listen address.
func (e Env) Addr() string { return e.Host + ":" + e.Port }

func parseBool(v string, def bool) bool {
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

// logLevelRank orders level names; unknown names are not ranked.
var logLevelRank = map[string]int{"DEBUG": 0, "INFO": 1, "WARN": 2, "WARNING": 2, "ERROR": 3}

// FileLogLevel is the level to give the log file. logging.file_level may ask
// for more detail than the console, but an explicit LOG_LEVEL is the operator's
// cap for the whole process: LOG_LEVEL=WARN must not keep writing INFO lines to
// disk just because the default file level is INFO.
func (e Env) FileLogLevel(configured string) string {
	if !e.LogLevelSet {
		return configured
	}
	cap, ok := logLevelRank[strings.ToUpper(e.LogLevel)]
	if !ok {
		return configured
	}
	want, ok := logLevelRank[strings.ToUpper(strings.TrimSpace(configured))]
	if !ok || configured == "" {
		return e.LogLevel // unset: follow the cap
	}
	if want < cap {
		return e.LogLevel
	}
	return configured
}
