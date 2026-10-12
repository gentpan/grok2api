package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// serverWithKeys is a Server whose managed-key store lives in a temp dir.
func serverWithKeys(t *testing.T) (*Server, *apiKeyStore) {
	t.Helper()
	st := newAPIKeyStore(filepath.Join(t.TempDir(), "api_keys.json"))
	return &Server{keys: st}, st
}

func TestManagedKeyGrantsPoolAndRevocationIsImmediate(t *testing.T) {
	useConfig(t, "[app]\napi_key = \"\"\n")
	srv, st := serverWithKeys(t)
	e, poolOK := apiEngine(srv)

	status := func(auth string) int {
		*poolOK = false
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		e.ServeHTTP(w, r)
		return w.Code
	}
	// No keys at all → open mode.
	if c := status(""); c != http.StatusOK {
		t.Fatalf("open mode = %d", c)
	}
	k, err := st.create("my project")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(k.Key, "sk-") || len(k.Key) < 40 {
		t.Fatalf("weak key format: %q", k.Key)
	}
	// A key exists → auth is now required; the key may use the pool.
	if c := status(""); c != http.StatusUnauthorized {
		t.Fatalf("no key after creation = %d, want 401", c)
	}
	if c := status("Bearer " + k.Key); c != http.StatusOK || !*poolOK {
		t.Fatalf("managed key: status=%d pool=%v", c, *poolOK)
	}
	if k.Requests != 1 {
		t.Fatalf("requests = %d, want 1", k.Requests)
	}
	// Another string is only BYO-SSO: allowed but never the pool.
	if c := status("Bearer something-else"); c != http.StatusOK || *poolOK {
		t.Fatalf("foreign token: status=%d pool=%v", c, *poolOK)
	}
	// Revoked → pool access gone immediately (a second key keeps auth on;
	// the API forbids removing the last key so it can never go open).
	spare, err := st.create("spare")
	if err != nil {
		t.Fatal(err)
	}
	if !st.remove(keyID(k.Key)) {
		t.Fatal("remove failed")
	}
	if c := status("Bearer " + k.Key); c != http.StatusOK || *poolOK {
		t.Fatalf("revoked key: status=%d pool=%v, want BYO-only", c, *poolOK)
	}
	if c := status("Bearer " + spare.Key); c != http.StatusOK || !*poolOK {
		t.Fatalf("spare key: status=%d pool=%v", c, *poolOK)
	}
}

func TestKeyStorePersistsWith0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "api_keys.json")
	st := newAPIKeyStore(path)
	k, err := st.create("a")
	if err != nil {
		t.Fatal(err)
	}
	reloaded := newAPIKeyStore(path)
	if reloaded.match(k.Key) == nil {
		t.Fatal("key lost after reload")
	}
	if reloaded.match("sk-nope") != nil || reloaded.match("") != nil {
		t.Fatal("matched an unknown key")
	}
}

func TestCannotRemoveLastKey(t *testing.T) {
	useConfig(t, "[app]\napi_key = \"\"\n")
	s, st := serverWithKeys(t)
	k, _ := st.create("only")
	gin.SetMode(gin.TestMode)
	e := gin.New()
	e.DELETE("/keys/:id", s.handleKeysDelete)

	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/keys/"+keyID(k.Key), nil))
	if w.Code != http.StatusConflict {
		t.Fatalf("delete last key = %d, want 409", w.Code)
	}
	if st.count() != 1 {
		t.Fatal("last key was removed")
	}
	// With a second key, removal works.
	k2, _ := st.create("second")
	w = httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/keys/"+keyID(k.Key), nil))
	if w.Code != http.StatusOK || st.count() != 1 || st.match(k2.Key) == nil {
		t.Fatalf("delete with spare key: code=%d count=%d", w.Code, st.count())
	}
}

func TestUsageRecorderCountsAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	r := newUsageRecorder(path)
	r.record("grok-4", 200, 100*time.Millisecond)
	r.record("grok-4", 502, 300*time.Millisecond)
	r.record("", 200, 50*time.Millisecond)
	r.flush()

	snap := r.snapshot(3)
	tot := snap["totals"].(gin.H)
	if tot["requests"] != 3 || tot["errors"] != 1 {
		t.Fatalf("totals = %v", tot)
	}
	models := snap["models"].([]gin.H)
	if models[0]["model"] != "grok-4" || models[0]["requests"] != 2 || models[0]["errors"] != 1 {
		t.Fatalf("models = %v", models)
	}
	// Reload from disk keeps the counters.
	again := newUsageRecorder(path)
	b, _ := json.Marshal(again.snapshot(3)["totals"])
	if !strings.Contains(string(b), `"requests":3`) {
		t.Fatalf("not persisted: %s", b)
	}
}

func TestSummarizeSubscription(t *testing.T) {
	raw := map[string]any{
		"subscriptions": []any{map[string]any{
			"tier":   "SUBSCRIPTION_TIER_SUPER",
			"status": "SUBSCRIPTION_STATUS_ACTIVE",
			"stripe": map[string]any{"currentPeriodEnd": float64(1893456000), "currentPeriodStart": "2026-01-01T00:00:00Z"},
			"note":   "ignored",
		}},
	}
	got := summarizeSubscription(raw)
	byLabel := map[string][]gin.H{}
	for _, it := range got {
		byLabel[it["label"].(string)] = append(byLabel[it["label"].(string)], it)
	}
	if len(byLabel["套餐 / 等级"]) != 1 || len(byLabel["状态"]) != 1 {
		t.Fatalf("tier/status not found: %v", got)
	}
	end := byLabel["到期 / 续期时间"]
	if len(end) != 1 || end[0]["time"] != "2030-01-01T00:00:00Z" {
		t.Fatalf("period end = %v", end)
	}
	if len(byLabel["周期开始时间"]) != 1 {
		t.Fatalf("period start missing: %v", got)
	}
}
