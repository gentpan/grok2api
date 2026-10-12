//go:build e2e

// These tests talk to the real grok.com with cookies from data/config.toml and
// are excluded from the default run. Use: go test -tags e2e ./internal/grok/

package grok

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gentpan/grok2api/internal/config"
	"github.com/gentpan/grok2api/internal/grok/statsig"
	"github.com/gentpan/grok2api/internal/grok/statsig/svgfingerprint"
	tlsclient "github.com/gentpan/grok2api/internal/tlsclient"
)

func truncForTest(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// realCookie and realUA are defined in statsig_e2e_helpers.go.

// TestE2E_ChatWithDynamicStatsig_RealCF uses the cf_clearance from
// data/config.toml and a live seed captured in this session. Sends a POST to
// /rest/app-chat/conversations/new via tls_client with our dynamic statsig.
func TestE2E_ChatWithDynamicStatsig_RealCF(t *testing.T) {
	seedB64 := "t2ODAFY4ozXd0K2Y8MdI2XfxTDiJoakZPuoaKfcQn8VuasZMcKliyhA1pJ+o1oMf"
	cookies := realCookie(t)
	client := tlsclient.New()

	// 1. Compute HEX
	hex, err := svgfingerprint.ComputeHEXForSeedB64(seedB64)
	if err != nil {
		t.Fatalf("ComputeHEXForSeedB64: %v", err)
	}
	t.Logf("hex  = %s (len=%d)", hex, len(hex))

	// 2. Set pair and generate statsig
	if err := statsig.SetPair(seedB64, hex); err != nil {
		t.Fatalf("SetPair: %v", err)
	}
	sid, err := statsig.Generate("/rest/app-chat/conversations/new", "POST", time.Now().Unix())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	t.Logf("statsig = %s (len=%d)", sid, len(sid))

	// 3. POST to /rest/app-chat/conversations/new
	req, err := http.NewRequest("POST", "https://grok.com/rest/app-chat/conversations/new",
		strings.NewReader(`{"temporary":false,"modelName":"grok-3","message":"hi"}`))
	if err != nil {
		t.Fatalf("NewRequest POST: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", realUA())
	req.Header.Set("Cookie", cookies)
	req.Header.Set("x-statsig-id", sid)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST chat: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	body := string(respBody)

	t.Logf("HTTP %d — %s", resp.StatusCode, truncForTest(body, 400))

	if resp.StatusCode == 403 && strings.Contains(body, `<title>Just a moment`) {
		t.Fatalf("cf_clearance invalid/expired (got Cloudflare challenge)")
	}
	if resp.StatusCode == 403 && strings.Contains(body, `"code":7`) {
		t.Fatalf("code:7 anti-bot rejected! body: %s", truncForTest(body, 500))
	}
	if resp.StatusCode == 200 {
		t.Logf("✅ ANTI-BOT PASSED — grok accepted the dynamically-computed statsig")
	} else if resp.StatusCode >= 400 {
		t.Logf("HTTP %d — not a code:7 anti-bot rejection", resp.StatusCode)
	}
}

// TestE2E_ChatWithRateLimit_RealCF probes /rest/rate-limits with cf_clearance
// from config. This is the lightest endpoint that exercises the statsig header.
func TestE2E_ChatWithRateLimit_RealCF(t *testing.T) {
	cookies := realCookie(t)
	client := tlsclient.New()
	ua := realUA()

	sid, err := statsig.Generate("/rest/rate-limits", "POST", time.Now().Unix())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	t.Logf("statsig = %s (len=%d)", sid, len(sid))

	req, err := http.NewRequest("POST", "https://grok.com/rest/rate-limits", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Cookie", cookies)
	req.Header.Set("x-statsig-id", sid)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST rate-limits: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	body := string(respBody)

	t.Logf("HTTP %d — %s", resp.StatusCode, truncForTest(body, 400))

	if resp.StatusCode == 403 && strings.Contains(body, `<title>Just a moment`) {
		t.Fatalf("cf_clearance invalid/expired (got Cloudflare challenge)")
	}
	if resp.StatusCode == 403 && strings.Contains(body, `"code":7`) {
		t.Fatalf("code:7 anti-bot rejected! body: %s", truncForTest(body, 500))
	}
	switch resp.StatusCode {
	case 200:
		t.Logf("✅ ANTI-BOT + statsig ACCEPTED — grok returned 200 OK")
	case 404:
		t.Logf("✅ ANTI-BOT PASSED — grok reached the API (404 = route exists, request body wrong)")
	case 401:
		t.Logf("⚠️ HTTP 401 — sso token / x-challenge / x-signature expired or invalid")
	default:
		t.Logf("HTTP %d — not a code:7 anti-bot rejection", resp.StatusCode)
	}
}

// TestE2E_ChatWithRealBody sends a minimal real chat body to verify the
// full anti-bot gate is accepted with our auto-generated statsig.
func TestE2E_ChatWithRealBody(t *testing.T) {
	cookies := realCookie(t)
	client := tlsclient.New()
	ua := realUA()

	// Try the lightest possible "real" chat probe: /rest/app-chat/check
	sid, err := statsig.Generate("/rest/app-chat/check", "POST", time.Now().Unix())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	bodyJSON := `{"modelName":"grok-3"}`
	req, err := http.NewRequest("POST", "https://grok.com/rest/app-chat/check", strings.NewReader(bodyJSON))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Cookie", cookies)
	req.Header.Set("x-statsig-id", sid)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST check: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	body := string(respBody)

	t.Logf("HTTP %d — %s", resp.StatusCode, truncForTest(body, 400))

	if resp.StatusCode == 403 && strings.Contains(body, `<title>Just a moment`) {
		t.Fatalf("cf_clearance invalid/expired")
	}
	if resp.StatusCode == 403 && strings.Contains(body, `"code":7`) {
		t.Fatalf("code:7 anti-bot rejected")
	}
	if resp.StatusCode == 200 {
		t.Logf("✅ ANTI-BOT + statsig ACCEPTED — got 200 OK")
	} else if resp.StatusCode == 404 || resp.StatusCode == 405 {
		t.Logf("✅ ANTI-BOT PASSED — reached API (HTTP %d, endpoint may be different)", resp.StatusCode)
	}
}

// TestE2E_WithBuildHTTPHeaders sends via the exact same code path as the
// server: buildHTTPHeaders → Transport.do → tlsclient.Do.
// WITHOUT WithExtraHeaders — exactly how the server calls it.
func TestE2E_WithBuildHTTPHeaders(t *testing.T) {
	config.SetPaths(
		"D:/Go/grok2api/config.defaults.toml",
		"D:/Go/grok2api/data/config.toml",
	)
	if err := config.Load(); err != nil {
		t.Skipf("config load: %v", err)
	}
	sso := os.Getenv("GROK_SSO")
	if sso == "" {
		t.Skip("GROK_SSO not set")
	}

	tr, err := NewTransport()
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	result, err := tr.PostJSON(context.Background(), "https://grok.com/rest/rate-limits", sso, []byte(`{}`))
	if err != nil {
		errStr := err.Error()
		t.Logf("PostJSON error: %s", truncForTest(errStr, 500))
		if strings.Contains(errStr, "code\":7") || strings.Contains(errStr, "code:7") {
			t.Fatalf("code:7 anti-bot rejected")
		}
		if strings.Contains(errStr, "Just a moment") {
			t.Fatalf("CF challenge")
		}
		// 404 = anti-bot passed, route wrong
		if strings.Contains(errStr, "404") {
			t.Logf("✅ Anti-bot PASSED (404 = route not found)")
			return
		}
		return
	}
	t.Logf("Result: %v", result)
	t.Logf("✅ Anti-bot passed")
}
