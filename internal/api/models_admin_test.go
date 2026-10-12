package api

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/gentpan/grok2api/internal/model"
)

func TestParseModelIDs(t *testing.T) {
	cases := []struct {
		name string
		raw  map[string]any
		want []string
	}{
		{"openai style", map[string]any{"data": []any{map[string]any{"id": "grok-5"}, map[string]any{"id": "grok-4.5"}}}, []string{"grok-4.5", "grok-5"}},
		{"models of objects", map[string]any{"models": []any{map[string]any{"name": "grok-x"}, map[string]any{"model": "grok-y"}}}, []string{"grok-x", "grok-y"}},
		{"string list + dedupe", map[string]any{"models": []any{"grok-a", "grok-a", "grok-b"}}, []string{"grok-a", "grok-b"}},
		{"junk ignored", map[string]any{"data": []any{map[string]any{"id": "has space"}, map[string]any{"id": "../etc"}, 7, map[string]any{}}}, nil},
		{"empty", map[string]any{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseModelIDs(tc.raw); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCustomModelLifecycleThroughAdminAPI(t *testing.T) {
	h := newAuthHarness(t)
	h.setup()
	const id = "grok-5-test-model"
	t.Cleanup(func() { model.Remove(id) })

	// Invalid input is refused.
	for name, body := range map[string]gin.H{
		"bad id":           {"id": "has space", "kind": "console-chat"},
		"bad kind":         {"id": id, "kind": "nope"},
		"web without mode": {"id": id, "kind": "web-chat"},
		"built-in name":    {"id": "grok-4.20-fast", "kind": "console-chat"},
	} {
		w := h.do(http.MethodPost, "/admin/api/models", body)
		if w.Code != http.StatusBadRequest && w.Code != http.StatusConflict {
			t.Fatalf("%s = %d, want 4xx (%s)", name, w.Code, w.Body.String())
		}
	}

	// Add → resolvable by the request path and listed as custom.
	w := h.do(http.MethodPost, "/admin/api/models", gin.H{"id": id, "name": "Grok 5 (test)", "kind": "console-chat"})
	if w.Code != http.StatusOK {
		t.Fatalf("add = %d %s", w.Code, w.Body.String())
	}
	spec, ok := model.Resolve(id)
	if !ok || !spec.IsConsoleChat() || !spec.Custom {
		t.Fatalf("registered spec = %+v ok=%v", spec, ok)
	}
	if w := h.do(http.MethodPost, "/admin/api/models", gin.H{"id": id, "kind": "console-chat"}); w.Code != http.StatusConflict {
		t.Fatalf("duplicate add = %d, want 409", w.Code)
	}

	// Disable / enable.
	if w := h.do(http.MethodPost, "/admin/api/models/"+id+"/enabled", gin.H{"enabled": false}); w.Code != http.StatusOK {
		t.Fatalf("disable = %d %s", w.Code, w.Body.String())
	}
	if _, ok := model.Resolve(id); ok {
		t.Fatal("disabled model still resolves")
	}
	if w := h.do(http.MethodPost, "/admin/api/models/"+id+"/enabled", gin.H{"enabled": true}); w.Code != http.StatusOK {
		t.Fatalf("enable = %d", w.Code)
	}

	// Persisted and replayed at "startup".
	model.Remove(id)
	reloaded := newModelStore(h.srv.customModels().path())
	reloaded.apply(globalModels{})
	if _, ok := model.Resolve(id); !ok {
		t.Fatal("custom model not restored from disk")
	}

	// Delete: custom only.
	if w := h.do(http.MethodDelete, "/admin/api/models/grok-4.20-fast", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("delete built-in = %d, want 400", w.Code)
	}
	if w := h.do(http.MethodDelete, "/admin/api/models/"+id, nil); w.Code != http.StatusOK {
		t.Fatalf("delete custom = %d", w.Code)
	}
	if model.Get(id) != nil {
		t.Fatal("model still registered after delete")
	}
}

func TestDisabledBuiltInIsPersisted(t *testing.T) {
	h := newAuthHarness(t)
	h.setup()
	const id = "grok-4.3-fast"
	t.Cleanup(func() { model.SetEnabled(id, true) })

	if w := h.do(http.MethodPost, "/admin/api/models/"+id+"/enabled", gin.H{"enabled": false}); w.Code != http.StatusOK {
		t.Fatalf("disable built-in = %d", w.Code)
	}
	model.SetEnabled(id, true) // simulate a restart: registry back to defaults
	newModelStore(h.srv.customModels().path()).apply(globalModels{})
	if _, ok := model.Resolve(id); ok {
		t.Fatal("disabled built-in came back after restart")
	}
}
