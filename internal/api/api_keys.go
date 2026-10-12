package api

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/gentpan/grok2api/internal/jsonstore"
	"github.com/gentpan/grok2api/internal/logger"
	"github.com/gentpan/grok2api/internal/platform"
)

// apiKeyEntry is one panel-managed client key.
type apiKeyEntry struct {
	Key        string `json:"key"`
	Label      string `json:"label"`
	CreatedAt  int64  `json:"created_at"`
	LastUsedAt int64  `json:"last_used_at"`
	Requests   int    `json:"requests"`
}

// apiKeyFile is the persisted shape: {"keys":[...]}.
type apiKeyFile struct {
	Keys []*apiKeyEntry `json:"keys"`
}

// apiKeyStoreSaveDelay is how long usage counters (requests / last use) may
// stay unflushed; creation and revocation are written immediately.
const apiKeyStoreSaveDelay = 15 * time.Second

// apiKeyStore holds panel-managed keys in a 0600 JSON file. Keys configured
// in config.toml (app.api_key) keep working alongside them. A nil store has no
// keys.
type apiKeyStore struct {
	f *jsonstore.File[apiKeyFile]
}

func newAPIKeyStore(path string) *apiKeyStore {
	f, err := jsonstore.Open(path, jsonstore.Config[apiKeyFile]{
		Debounce: apiKeyStoreSaveDelay,
		OnError:  func(err error) { logger.Warnf("api key store persist failed: error=%v", err) },
		Normalize: func(d *apiKeyFile) {
			kept := d.Keys[:0]
			for _, k := range d.Keys {
				if k != nil && k.Key != "" {
					kept = append(kept, k)
				}
			}
			d.Keys = kept
		},
	})
	if err != nil {
		logger.Warnf("api key store unreadable, starting empty: error=%v", err)
	}
	return &apiKeyStore{f: f}
}

// match returns the managed entry whose key equals token (constant time).
func (s *apiKeyStore) match(token string) *apiKeyEntry {
	if s == nil || token == "" {
		return nil
	}
	var hit *apiKeyEntry
	s.f.View(func(d *apiKeyFile) {
		for _, k := range d.Keys {
			if subtle.ConstantTimeCompare([]byte(token), []byte(k.Key)) == 1 {
				hit = k
			}
		}
	})
	return hit
}

func (s *apiKeyStore) count() int {
	if s == nil {
		return 0
	}
	n := 0
	s.f.View(func(d *apiKeyFile) { n = len(d.Keys) })
	return n
}

// snapshot returns copies of every entry, oldest first.
func (s *apiKeyStore) snapshot() []apiKeyEntry {
	if s == nil {
		return nil
	}
	var out []apiKeyEntry
	s.f.View(func(d *apiKeyFile) {
		for _, k := range d.Keys {
			out = append(out, *k)
		}
	})
	return out
}

// touch records a use of the key, persisting lazily.
func (s *apiKeyStore) touch(k *apiKeyEntry) {
	if s == nil || k == nil {
		return
	}
	s.f.Mutate(func(*apiKeyFile) {
		k.Requests++
		k.LastUsedAt = time.Now().UnixMilli()
	})
}

var errKeyStoreUnavailable = errors.New("api key store unavailable")

func (s *apiKeyStore) create(label string) (*apiKeyEntry, error) {
	if s == nil {
		return nil, errKeyStoreUnavailable
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	e := &apiKeyEntry{
		Key:       "sk-" + hex.EncodeToString(buf),
		Label:     strings.TrimSpace(label),
		CreatedAt: time.Now().UnixMilli(),
	}
	err := s.f.Update(func(d *apiKeyFile) error {
		d.Keys = append(d.Keys, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return e, nil
}

// remove revokes the key with the given id and persists immediately. It
// reports whether a key was removed; a persistence failure is logged but the
// key stays revoked in memory.
func (s *apiKeyStore) remove(id string) bool {
	if s == nil {
		return false
	}
	found := false
	err := s.f.Update(func(d *apiKeyFile) error {
		for i, k := range d.Keys {
			if keyID(k.Key) == id {
				d.Keys = append(d.Keys[:i], d.Keys[i+1:]...)
				found = true
				return nil
			}
		}
		return nil
	})
	if err != nil {
		logger.Warnf("api key store persist failed: error=%v", err)
		// Update rolled the removal back; revoke in memory regardless so a
		// revoked key never keeps working because the disk is unhappy.
		s.f.Mutate(func(d *apiKeyFile) {
			for i, k := range d.Keys {
				if keyID(k.Key) == id {
					d.Keys = append(d.Keys[:i], d.Keys[i+1:]...)
					found = true
					return
				}
			}
		})
	}
	return found
}

// flush writes pending usage counters.
func (s *apiKeyStore) flush() error {
	if s == nil {
		return nil
	}
	return s.f.Flush()
}

// keyID is a stable, non-secret identifier for a key.
func keyID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:6])
}

// configAPIKeys returns the non-empty keys from config.toml.
func (s *Server) configAPIKeys() []string {
	var out []string
	for _, k := range s.config().GetList("app.api_key", nil) {
		if k = strings.TrimSpace(k); k != "" {
			out = append(out, k)
		}
	}
	return out
}

// handleKeysList lists every key that currently grants pool access.
func (s *Server) handleKeysList(c *gin.Context) {
	out := []gin.H{}
	for _, k := range s.configAPIKeys() {
		out = append(out, gin.H{"id": keyID(k), "key": k, "label": "配置文件中的密钥", "source": "config"})
	}
	for _, k := range s.keyStore().snapshot() {
		out = append(out, gin.H{
			"id": keyID(k.Key), "key": k.Key, "label": k.Label, "source": "managed",
			"created_at": k.CreatedAt, "last_used_at": k.LastUsedAt, "requests": k.Requests,
		})
	}
	c.JSON(http.StatusOK, gin.H{"keys": out, "open_mode": len(out) == 0})
}

// handleKeysCreate generates a new random key.
func (s *Server) handleKeysCreate(c *gin.Context) {
	var body struct {
		Label string `json:"label"`
	}
	_ = c.ShouldBindJSON(&body)
	if len([]rune(body.Label)) > 60 {
		writeAppError(c, platform.ValidationError("Label is too long (max 60 characters)", "label"))
		return
	}
	e, err := s.keyStore().create(body.Label)
	if err != nil {
		writeAppError(c, platform.NewAppError("cannot persist key: "+err.Error(), platform.ErrServer, "key_store_failed", http.StatusInternalServerError))
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": keyID(e.Key), "key": e.Key, "label": e.Label, "created_at": e.CreatedAt})
}

// handleKeysDelete revokes a key. The last remaining key cannot be removed,
// because that would silently turn the API into open access.
func (s *Server) handleKeysDelete(c *gin.Context) {
	id := c.Param("id")
	cfgKeys := s.configAPIKeys()
	keys := s.keyStore()
	total := len(cfgKeys) + keys.count()
	if total <= 1 {
		writeAppError(c, platform.NewAppError(
			"Cannot remove the last API key: the API would become open to everyone. Create another key first.",
			platform.ErrValidation, "last_key", http.StatusConflict))
		return
	}
	if keys.remove(id) {
		c.JSON(http.StatusOK, gin.H{"status": "success"})
		return
	}
	for i, k := range cfgKeys {
		if keyID(k) != id {
			continue
		}
		rest := append(append([]string{}, cfgKeys[:i]...), cfgKeys[i+1:]...)
		cfg := s.config()
		if err := cfg.Update(map[string]any{"app": map[string]any{"api_key": strings.Join(rest, ",")}}); err != nil {
			writeAppError(c, platform.NewAppError("config update failed: "+err.Error(), platform.ErrServer, "config_update_failed", http.StatusInternalServerError))
			return
		}
		_ = cfg.Load()
		c.JSON(http.StatusOK, gin.H{"status": "success"})
		return
	}
	writeAppError(c, platform.ValidationErrorCode("Key not found", "id", "key_not_found"))
}
