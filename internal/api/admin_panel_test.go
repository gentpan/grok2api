package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func panelEngine() *gin.Engine {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	e.GET("/admin", panelAsset("index.html", "text/html; charset=utf-8"))
	e.GET("/admin/app.js", panelAsset("app.js", "text/javascript; charset=utf-8"))
	e.GET("/admin/app.css", panelAsset("app.css", "text/css; charset=utf-8"))
	return e
}

func TestPanelAssetsServedWithStrictHeaders(t *testing.T) {
	e := panelEngine()
	for path, ctype := range map[string]string{
		"/admin":         "text/html",
		"/admin/app.js":  "text/javascript",
		"/admin/app.css": "text/css",
	} {
		w := httptest.NewRecorder()
		e.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s status = %d", path, w.Code)
		}
		if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, ctype) {
			t.Fatalf("%s content-type = %q", path, got)
		}
		csp := w.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
			t.Fatalf("%s weak CSP: %q", path, csp)
		}
		if w.Header().Get("X-Frame-Options") != "DENY" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s missing hardening headers", path)
		}
		if w.Body.Len() == 0 {
			t.Fatalf("%s empty body", path)
		}
	}
}

// The panel must not rely on inline script/style, which the CSP forbids.
func TestPanelHTMLHasNoInlineCode(t *testing.T) {
	b, err := panelFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := strings.ToLower(string(b))
	if strings.Contains(html, "<style") || strings.Contains(html, " style=") || strings.Contains(html, "onclick=") {
		t.Fatal("index.html contains inline style/script")
	}
	if strings.Count(html, "<script") != strings.Count(html, "<script src=") {
		t.Fatal("index.html contains an inline <script>")
	}
}

func TestTestChatAccess(t *testing.T) {
	cases := []struct {
		name, body string
		wantTok    string
		wantPool   bool
	}{
		{"no token uses pool", `{"model":"m","messages":[]}`, "", true},
		{"explicit token never uses pool", `{"model":"m","token":"sso=abcdefghijklmnopqrstuvwxyz"}`, "abcdefghijklmnopqrstuvwxyz", false},
		{"blank token uses pool", `{"model":"m","token":"   "}`, "", true},
		{"garbage body uses pool", `not json`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tok, pool := testChatAccess([]byte(tc.body))
			if tok != tc.wantTok || pool != tc.wantPool {
				t.Fatalf("got (%q,%v), want (%q,%v)", tok, pool, tc.wantTok, tc.wantPool)
			}
		})
	}
}

func TestTokensTestValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	s := &Server{}
	e.POST("/t", s.handleTokensTest)

	post := func(body string) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/t", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		e.ServeHTTP(w, r)
		return w.Code
	}
	if c := post(`{"tokens":[]}`); c != http.StatusBadRequest {
		t.Fatalf("empty list = %d, want 400", c)
	}
	if c := post(`{"tokens":["", "   "]}`); c != http.StatusBadRequest {
		t.Fatalf("blank tokens = %d, want 400", c)
	}
	var many []string
	for i := 0; i < maxTokenTestBatch+1; i++ {
		many = append(many, strings.Repeat("a", 30)+string(rune('A'+i%26))+strings.Repeat("b", i))
	}
	body := `{"tokens":["` + strings.Join(many, `","`) + `"]}`
	if c := post(body); c != http.StatusBadRequest {
		t.Fatalf("oversize batch = %d, want 400", c)
	}
}
