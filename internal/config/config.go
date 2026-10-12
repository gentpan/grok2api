// Package config implements TOML configuration loading with environment
// variable overrides, mirroring the Python project's loader.
//
// Precedence: defaults TOML -> user config.toml -> GROK_ env overrides.
//
// Env overrides use the form GROK_<PATH>=value where <PATH> is the dotted key
// with dots replaced by underscores (GROK_FEATURES_STREAM -> features.stream,
// GROK_ACCOUNT_REFRESH_ENABLED -> account.refresh.enabled). The mapping is
// resolved against the keys known from the defaults/user files; see env.go.
package config

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/pelletier/go-toml/v2"

	"github.com/gentpan/grok2api/internal/logger"
	"github.com/gentpan/grok2api/internal/platform"
)

// ErrNoUserPath is returned by Update when no user config file path is
// configured, i.e. the change could not be persisted.
var ErrNoUserPath = errors.New("config: no user config file path configured; change not persisted")

// Snapshot is the process-global configuration store. It lazily reloads when
// the underlying user config file changes (detected by mtime).
type Snapshot struct {
	// mu guards the fields below; it is only ever held for in-memory work,
	// never across disk IO.
	mu            sync.RWMutex
	data          map[string]any
	defaultsPath  string
	userPath      string
	defaultsMtime int64 // UnixNano, 0 when the file is missing
	userMtime     int64

	// loaded is true once a load was attempted for the recorded mtimes (even
	// if it failed); loadErr is that attempt's result. This lets a broken file
	// be parsed (and reported) once per change instead of once per request.
	loaded  bool
	loadErr error

	// embeddedDefaults is used when the defaults file does not exist.
	embeddedDefaults []byte
	noticedDefaults  atomic.Bool

	// envOverrides maps dotted config keys that were set from a GROK_* env
	// var to that variable's name, as of the last successful load.
	envOverrides map[string]string

	// ioMu serialises everything that touches the disk (reloads, Update,
	// SetPaths) so Get* readers are never blocked behind file IO. Lock order:
	// ioMu before mu.
	ioMu sync.Mutex

	subMu   sync.Mutex
	subs    map[int]func(*Snapshot)
	nextSub int
}

var global = &Snapshot{}

// Global returns the process-global snapshot.
func Global() *Snapshot { return global }

// SetPaths configures the defaults and user config file paths.
func SetPaths(defaultsPath, userPath string) {
	global.ioMu.Lock()
	defer global.ioMu.Unlock()
	global.mu.Lock()
	defer global.mu.Unlock()
	global.defaultsPath = defaultsPath
	global.userPath = userPath
	global.loaded = false
}

// SetEmbeddedDefaults registers built-in default configuration (TOML). It is
// used only when the defaults file at the configured path does not exist; a
// single INFO line is logged the first time the fallback is taken.
func SetEmbeddedDefaults(b []byte) {
	global.ioMu.Lock()
	defer global.ioMu.Unlock()
	global.mu.Lock()
	defer global.mu.Unlock()
	global.embeddedDefaults = append([]byte(nil), b...)
	global.noticedDefaults.Store(false)
	global.loaded = false
}

// DefaultsPath returns the resolved defaults TOML path.
func DefaultsPath() string {
	global.mu.RLock()
	defer global.mu.RUnlock()
	return global.defaultsPath
}

// UserPath returns the resolved user config.toml path.
func UserPath() string {
	global.mu.RLock()
	defer global.mu.RUnlock()
	return global.userPath
}

// Load (re)loads the configuration if the underlying files changed.
func Load() error {
	return global.Load()
}

// Subscribe registers fn on the process-global snapshot; see Snapshot.Subscribe.
func Subscribe(fn func(*Snapshot)) (cancel func()) { return global.Subscribe(fn) }

// Load reloads the configuration if the underlying files changed.
//
// The common case (files unchanged) takes only a read lock and the stat calls
// happen outside any lock. A reload reads the files without holding the data
// lock, so Get* callers are not blocked while the disk is touched. When a file
// fails to parse, its mtime is still recorded and a WARN is logged once; the
// previous configuration stays active and the same broken file is not
// re-parsed until it changes again. Subscribers are notified after a
// successful reload, outside all locks.
func (s *Snapshot) Load() error {
	s.mu.RLock()
	dPath, uPath := s.defaultsPath, s.userPath
	s.mu.RUnlock()

	dMtime := fileMtime(dPath)
	uMtime := fileMtime(uPath)

	s.mu.RLock()
	if s.loaded && dMtime == s.defaultsMtime && uMtime == s.userMtime {
		err := s.loadErr
		s.mu.RUnlock()
		return err
	}
	s.mu.RUnlock()

	changed, err := s.reload()
	if changed {
		s.notify()
	}
	return err
}

// reload performs the slow path of Load. changed reports whether a new
// configuration was published.
func (s *Snapshot) reload() (changed bool, err error) {
	s.ioMu.Lock()
	defer s.ioMu.Unlock()

	s.mu.RLock()
	dPath, uPath := s.defaultsPath, s.userPath
	embedded := s.embeddedDefaults
	loaded := s.loaded
	prevD, prevU := s.defaultsMtime, s.userMtime
	prevErr := s.loadErr
	s.mu.RUnlock()

	dMtime, uMtime := fileMtime(dPath), fileMtime(uPath)
	// Re-check: another goroutine may have reloaded while we waited.
	if loaded && dMtime == prevD && uMtime == prevU {
		return false, prevErr
	}

	data, err := s.loadDefaults(dPath, embedded)
	if err == nil {
		var user map[string]any
		user, err = loadTOML(uPath)
		if err == nil {
			data = deepMerge(data, user)
		}
	}
	var overrides map[string]string
	if err == nil {
		overrides = applyEnv(data, "GROK_")
	}

	s.mu.Lock()
	s.defaultsMtime = dMtime
	s.userMtime = uMtime
	s.loaded = true
	s.loadErr = err
	if err == nil {
		s.data = data
		s.envOverrides = overrides
	}
	s.mu.Unlock()

	if err != nil {
		logger.Warnf("config reload failed, keeping previous configuration until the files change: error=%v", err)
		return false, err
	}
	return true, nil
}

// loadDefaults reads the defaults file, falling back to the embedded defaults
// when the file is missing. Caller holds s.ioMu.
func (s *Snapshot) loadDefaults(path string, embedded []byte) (map[string]any, error) {
	if path != "" {
		b, err := os.ReadFile(path)
		if err == nil {
			return parseTOML(b)
		}
		if !os.IsNotExist(err) {
			return nil, err
		}
	}
	if len(embedded) > 0 {
		if s.noticedDefaults.CompareAndSwap(false, true) {
			logger.Infof("config defaults file not found, using embedded defaults: path=%s", path)
		}
		return parseTOML(embedded)
	}
	if path != "" && s.noticedDefaults.CompareAndSwap(false, true) {
		logger.Warnf("config defaults file not found and no embedded defaults available: path=%s", path)
	}
	return map[string]any{}, nil
}

// Subscribe registers fn to be called after every successful (re)load, with
// the snapshot already holding the new configuration. fn runs on the goroutine
// that triggered the reload, outside the snapshot's locks (it may call Get*),
// so it should return quickly. The returned cancel func unregisters fn and is
// safe to call more than once.
func (s *Snapshot) Subscribe(fn func(*Snapshot)) (cancel func()) {
	if fn == nil {
		return func() {}
	}
	s.subMu.Lock()
	if s.subs == nil {
		s.subs = make(map[int]func(*Snapshot))
	}
	id := s.nextSub
	s.nextSub++
	s.subs[id] = fn
	s.subMu.Unlock()
	return func() {
		s.subMu.Lock()
		delete(s.subs, id)
		s.subMu.Unlock()
	}
}

func (s *Snapshot) notify() {
	s.subMu.Lock()
	if len(s.subs) == 0 {
		s.subMu.Unlock()
		return
	}
	ids := make([]int, 0, len(s.subs))
	for id := range s.subs {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	fns := make([]func(*Snapshot), 0, len(ids))
	for _, id := range ids {
		fns = append(fns, s.subs[id])
	}
	s.subMu.Unlock()
	for _, fn := range fns {
		func() {
			defer func() {
				if r := recover(); r != nil {
					logger.Errorf("config subscriber panicked: panic=%v", r)
				}
			}()
			fn(s)
		}()
	}
}

// OverriddenByEnv returns those of keys (dotted paths, e.g. the output of
// FlattenKeys) whose effective value currently comes from a GROK_* environment
// variable. Writing such a key through Update is persisted but has no effect
// until the variable is removed; admin callers can use this to tell the user.
// A key that is a table containing an overridden key is reported too.
func (s *Snapshot) OverriddenByEnv(keys []string) []string {
	_ = s.Load()
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.envOverrides) == 0 {
		return nil
	}
	var out []string
	for _, k := range keys {
		hit := false
		if _, ok := s.envOverrides[k]; ok {
			hit = true
		} else {
			for o := range s.envOverrides {
				if strings.HasPrefix(o, k+".") {
					hit = true
					break
				}
			}
		}
		if hit {
			out = append(out, k)
		}
	}
	return out
}

// Raw returns a deep copy of the current nested config data.
func (s *Snapshot) Raw() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return deepCopy(s.data)
}

// Get walks the nested config using a dotted key and returns the value or def.
// Maps and slices are returned as copies, so callers cannot mutate the live
// configuration.
func (s *Snapshot) Get(key string, def any) any {
	s.mu.RLock()
	v := getNestedLocked(s.data, key)
	if v != nil {
		v = copyValue(v)
	}
	s.mu.RUnlock()
	if v == nil {
		return def
	}
	return v
}

// getScalar is Get without the defensive copy, for the typed getters that
// only inspect scalars and build fresh slices themselves.
func (s *Snapshot) getScalar(key string) any {
	s.mu.RLock()
	v := getNestedLocked(s.data, key)
	s.mu.RUnlock()
	return v
}

// GetStr returns a string value (stringifying if needed).
func (s *Snapshot) GetStr(key, def string) string {
	v := s.getScalar(key)
	if v == nil {
		return def
	}
	switch t := v.(type) {
	case string:
		return t
	case int64:
		return strconv.FormatInt(t, 10)
	case int:
		return strconv.Itoa(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	default:
		return def
	}
}

// GetInt returns an integer value.
func (s *Snapshot) GetInt(key string, def int) int {
	v := s.getScalar(key)
	switch t := v.(type) {
	case int64:
		return int(t)
	case int:
		return t
	case float64:
		return int(t)
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
			return n
		}
		return def
	default:
		return def
	}
}

// GetBool returns a boolean value. Accepts 1/true/yes/on (case-insensitive).
func (s *Snapshot) GetBool(key string, def bool) bool {
	v := s.getScalar(key)
	switch t := v.(type) {
	case bool:
		return t
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
		return def
	case int64:
		return t != 0
	case int:
		return t != 0
	default:
		return def
	}
}

// GetList returns a list. Strings are CSV-split; TOML arrays pass through.
// The returned slice is always a fresh copy.
func (s *Snapshot) GetList(key string, def []string) []string {
	v := s.getScalar(key)
	switch t := v.(type) {
	case []string:
		return append([]string(nil), t...)
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			out = append(out, toStr(item))
		}
		return out
	case string:
		parts := strings.Split(t, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
		return out
	default:
		return def
	}
}

// Update deep-merges a patch into the user config file and persists it.
//
// It returns ErrNoUserPath when no user config path is configured (nothing
// can be persisted). The file is read, merged and written without holding the
// data lock, so concurrent Get* calls are not blocked; concurrent Update calls
// are serialised. The next Load picks the change up.
//
// Note that GROK_* environment variables take precedence over the file; use
// OverriddenByEnv to find out whether a written key will actually take effect.
func (s *Snapshot) Update(patch map[string]any) error {
	s.ioMu.Lock()
	defer s.ioMu.Unlock()

	s.mu.RLock()
	path := s.userPath
	s.mu.RUnlock()
	if path == "" {
		return ErrNoUserPath
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	existing, err := loadTOML(path)
	if err != nil {
		return err
	}
	merged := deepMerge(existing, patch)
	if err := platform.AtomicWriteFile(path, 0o600, func(w io.Writer) error {
		return toml.NewEncoder(w).Encode(merged)
	}); err != nil {
		return err
	}
	// Force reload on next access.
	s.mu.Lock()
	s.loaded = false
	s.mu.Unlock()
	return nil
}

// --- helpers ---

func loadTOML(path string) (map[string]any, error) {
	if path == "" {
		return map[string]any{}, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	return parseTOML(b)
}

func parseTOML(b []byte) (map[string]any, error) {
	var out map[string]any
	if err := toml.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

func deepMerge(base, override map[string]any) map[string]any {
	result := make(map[string]any, len(base))
	for k, v := range base {
		result[k] = v
	}
	for k, v := range override {
		if existing, ok := result[k]; ok {
			if em, ok1 := existing.(map[string]any); ok1 {
				if vm, ok2 := v.(map[string]any); ok2 {
					result[k] = deepMerge(em, vm)
					continue
				}
			}
		}
		result[k] = v
	}
	return result
}

// deepCopy copies nested maps and slices so the result shares no mutable
// state with m.
func deepCopy(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = copyValue(v)
	}
	return out
}

// copyValue deep-copies the container types a TOML document can hold; scalars
// are returned as is.
func copyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return deepCopy(t)
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = copyValue(item)
		}
		return out
	case []string:
		return append([]string(nil), t...)
	case []map[string]any:
		out := make([]map[string]any, len(t))
		for i, item := range t {
			out[i] = deepCopy(item)
		}
		return out
	default:
		return v
	}
}

func getNestedLocked(data map[string]any, dotted string) any {
	if dotted == "" {
		return data
	}
	parts := strings.Split(dotted, ".")
	var node any = data
	for _, p := range parts {
		m, ok := node.(map[string]any)
		if !ok {
			return nil
		}
		node = m[p]
		if node == nil {
			return nil
		}
	}
	return node
}

// fileMtime returns the file's modification time as UnixNano (0 when path is
// empty or the file is missing).
func fileMtime(path string) int64 {
	if path == "" {
		return 0
	}
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.ModTime().UnixNano()
}

func toStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	default:
		return ""
	}
}

// IsStartupOnlyConfigKey returns true when a dotted key path is reserved for
// startup-time configuration (storage backends) and cannot be changed at runtime.
//
// Matching is on dotted-path boundaries: the key is startup-only when it equals
// a reserved path, lies below one ("account.local.path"), or is an ancestor of
// one ("account" — replacing it would overwrite the reserved subtree). Callers
// holding a nested patch must expand it with FlattenKeys first; checking only
// the top-level keys of a nested JSON patch does not work.
func IsStartupOnlyConfigKey(dotted string) bool {
	for _, p := range startupOnlyPrefixes {
		if dotted == p || strings.HasPrefix(dotted, p+".") || strings.HasPrefix(p, dotted+".") {
			return true
		}
	}
	return false
}

// FlattenKeys expands a (possibly nested) config patch into the sorted list of
// dotted leaf paths it would write, e.g. {"account":{"local":{"path":"x"}}}
// -> ["account.local.path"]. Empty tables are reported as their own path.
func FlattenKeys(patch map[string]any) []string {
	out := []string{}
	flattenInto("", patch, &out)
	sort.Strings(out)
	return out
}

func flattenInto(prefix string, m map[string]any, out *[]string) {
	for k, v := range m {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		if sub, ok := v.(map[string]any); ok && len(sub) > 0 {
			flattenInto(key, sub, out)
			continue
		}
		*out = append(*out, key)
	}
}

var startupOnlyPrefixes = []string{
	"account.storage",
	"account.local",
	"account.redis",
	"account.mysql",
	"account.postgresql",
}
