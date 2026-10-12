package api

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

const (
	testHost   = "admin.example.com"
	testOrigin = "https://admin.example.com"
	testAppKey = "a-long-custom-admin-key-for-tests"
	testUser   = "boss"
	testPass   = "correct horse battery"
)

type authHarness struct {
	t      *testing.T
	srv    *Server
	engine *gin.Engine
	cookie string
}

func newAuthHarness(t *testing.T) *authHarness {
	t.Helper()
	useConfig(t, "[app]\napp_key = \""+testAppKey+"\"\napi_key = \"k\"\n")
	gin.SetMode(gin.TestMode)
	s := NewServerFromDeps(Deps{
		DataDir:         t.TempDir(),
		BcryptCost:      bcrypt.MinCost,
		AdminFailMax:    50,
		AdminFailWindow: time.Minute,
	})
	t.Cleanup(func() { _ = s.Close() })
	return &authHarness{t: t, srv: s, engine: s.Router()}
}

// do emulates a browser behind a local TLS-terminating reverse proxy.
func (h *authHarness) do(method, path string, body any, opts ...func(*http.Request)) *httptest.ResponseRecorder {
	h.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	r := httptest.NewRequest(method, path, rd)
	r.Host = testHost
	r.RemoteAddr = "127.0.0.1:50000"
	r.Header.Set("X-Forwarded-For", "198.51.100.9")
	r.Header.Set("X-Forwarded-Proto", "https")
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet {
		r.Header.Set("Origin", testOrigin)
	}
	if h.cookie != "" {
		r.Header.Set("Cookie", sessionCookieName+"="+h.cookie)
	}
	for _, o := range opts {
		o(r)
	}
	w := httptest.NewRecorder()
	h.engine.ServeHTTP(w, r)
	return w
}

func (h *authHarness) takeCookie(w *httptest.ResponseRecorder) string {
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookieName {
			return c.Value
		}
	}
	return ""
}

func (h *authHarness) setup() {
	h.t.Helper()
	w := h.do(http.MethodPost, "/admin/auth/setup", gin.H{"app_key": testAppKey, "username": testUser, "password": testPass})
	if w.Code != http.StatusOK {
		h.t.Fatalf("setup = %d %s", w.Code, w.Body.String())
	}
	h.cookie = h.takeCookie(w)
	if h.cookie == "" {
		h.t.Fatal("setup did not set a session cookie")
	}
}

func TestSetupRequiresAppKeyAndStrongPassword(t *testing.T) {
	h := newAuthHarness(t)
	// Status before setup.
	var st map[string]any
	_ = json.Unmarshal(h.do(http.MethodGet, "/admin/auth/status", nil).Body.Bytes(), &st)
	if st["setup_required"] != true || st["authenticated"] != false {
		t.Fatalf("status = %v", st)
	}
	cases := []struct {
		name string
		body gin.H
		want int
	}{
		{"wrong app key", gin.H{"app_key": "nope", "username": testUser, "password": testPass}, http.StatusUnauthorized},
		{"default key not accepted when not set", gin.H{"app_key": "", "username": testUser, "password": testPass}, http.StatusUnauthorized},
		{"bad username", gin.H{"app_key": testAppKey, "username": "a b", "password": testPass}, http.StatusBadRequest},
		{"short password", gin.H{"app_key": testAppKey, "username": testUser, "password": "short"}, http.StatusBadRequest},
		{"password equals username", gin.H{"app_key": testAppKey, "username": "boss-boss-boss", "password": "boss-boss-boss"}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		if w := h.do(http.MethodPost, "/admin/auth/setup", tc.body); w.Code != tc.want {
			t.Fatalf("%s = %d, want %d (%s)", tc.name, w.Code, tc.want, w.Body.String())
		}
	}
	h.setup()
	// A second setup must not overwrite the account.
	w := h.do(http.MethodPost, "/admin/auth/setup", gin.H{"app_key": testAppKey, "username": "other", "password": "another long password"})
	if w.Code != http.StatusConflict {
		t.Fatalf("second setup = %d, want 409", w.Code)
	}
	// Cookie flags.
	w = h.do(http.MethodPost, "/admin/auth/login", gin.H{"username": testUser, "password": testPass})
	var ck *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookieName {
			ck = c
		}
	}
	if ck == nil || !ck.HttpOnly || !ck.Secure || ck.SameSite != http.SameSiteStrictMode || ck.Path != "/admin" {
		t.Fatalf("weak session cookie: %+v", ck)
	}
}

func TestPasswordLoginSessionAndCSRF(t *testing.T) {
	h := newAuthHarness(t)
	h.setup()
	h.cookie = ""

	// Not signed in → admin API is closed.
	if w := h.do(http.MethodGet, "/admin/api/keys", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous admin API = %d, want 401", w.Code)
	}
	if w := h.do(http.MethodPost, "/admin/auth/login", gin.H{"username": testUser, "password": "wrong password!!"}); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d", w.Code)
	}
	if w := h.do(http.MethodPost, "/admin/auth/login", gin.H{"username": "nobody", "password": testPass}); w.Code != http.StatusUnauthorized {
		t.Fatalf("unknown user = %d", w.Code)
	}
	w := h.do(http.MethodPost, "/admin/auth/login", gin.H{"username": testUser, "password": testPass})
	if w.Code != http.StatusOK {
		t.Fatalf("login = %d %s", w.Code, w.Body.String())
	}
	h.cookie = h.takeCookie(w)

	// Safe request with the cookie works.
	if w := h.do(http.MethodGet, "/admin/api/keys", nil); w.Code != http.StatusOK {
		t.Fatalf("session GET = %d", w.Code)
	}
	// Unsafe request: no Origin → rejected; foreign Origin → rejected; own Origin → ok.
	noOrigin := func(r *http.Request) { r.Header.Del("Origin") }
	if w := h.do(http.MethodPost, "/admin/api/keys", gin.H{"label": "x"}, noOrigin); w.Code != http.StatusForbidden {
		t.Fatalf("POST without Origin = %d, want 403", w.Code)
	}
	evil := func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }
	if w := h.do(http.MethodPost, "/admin/api/keys", gin.H{"label": "x"}, evil); w.Code != http.StatusForbidden {
		t.Fatalf("POST from foreign Origin = %d, want 403", w.Code)
	}
	if w := h.do(http.MethodPost, "/admin/api/keys", gin.H{"label": "x"}); w.Code != http.StatusOK {
		t.Fatalf("POST from own Origin = %d (%s)", w.Code, w.Body.String())
	}
	// Logout invalidates the session server-side.
	if w := h.do(http.MethodPost, "/admin/auth/logout", gin.H{}); w.Code != http.StatusOK {
		t.Fatalf("logout = %d", w.Code)
	}
	if w := h.do(http.MethodGet, "/admin/api/keys", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("after logout = %d, want 401", w.Code)
	}
}

func TestPasswordChangeNeedsCurrentPasswordAndSignsOut(t *testing.T) {
	h := newAuthHarness(t)
	h.setup()

	w := h.do(http.MethodPost, "/admin/api/auth/account", gin.H{"current_password": "not the password", "new_password": "a brand new password"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("wrong current password = %d, want 403", w.Code)
	}
	w = h.do(http.MethodPost, "/admin/api/auth/account", gin.H{"current_password": testPass, "new_password": "short"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("weak new password = %d, want 400", w.Code)
	}
	w = h.do(http.MethodPost, "/admin/api/auth/account", gin.H{"current_password": testPass, "new_password": "a brand new password"})
	if w.Code != http.StatusOK {
		t.Fatalf("change = %d %s", w.Code, w.Body.String())
	}
	if w := h.do(http.MethodGet, "/admin/api/keys", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("old session after password change = %d, want 401", w.Code)
	}
	h.cookie = ""
	if w := h.do(http.MethodPost, "/admin/auth/login", gin.H{"username": testUser, "password": testPass}); w.Code != http.StatusUnauthorized {
		t.Fatalf("old password still works: %d", w.Code)
	}
	if w := h.do(http.MethodPost, "/admin/auth/login", gin.H{"username": testUser, "password": "a brand new password"}); w.Code != http.StatusOK {
		t.Fatalf("new password rejected: %d", w.Code)
	}
}

func TestAppKeyBearerCanRecoverPassword(t *testing.T) {
	h := newAuthHarness(t)
	h.setup()
	h.cookie = ""
	bearer := func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+testAppKey) }
	w := h.do(http.MethodPost, "/admin/api/auth/account", gin.H{"new_password": "recovered password 1"}, bearer)
	if w.Code != http.StatusOK {
		t.Fatalf("recovery via app_key = %d %s", w.Code, w.Body.String())
	}
	if w := h.do(http.MethodPost, "/admin/auth/login", gin.H{"username": testUser, "password": "recovered password 1"}); w.Code != http.StatusOK {
		t.Fatalf("login after recovery = %d", w.Code)
	}
}

// ---- software authenticator -------------------------------------------------

type softAuthenticator struct {
	key      *ecdsa.PrivateKey
	credID   []byte
	userID   []byte
	signCnt  uint32
	rpIDHash [32]byte
}

func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func newSoftAuthenticator(rpID string) *softAuthenticator {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	id := make([]byte, 32)
	_, _ = rand.Read(id)
	return &softAuthenticator{key: k, credID: id, rpIDHash: sha256.Sum256([]byte(rpID))}
}

func (a *softAuthenticator) register(t *testing.T, options map[string]any) map[string]any {
	t.Helper()
	pk := options["publicKey"].(map[string]any)
	a.userID, _ = base64.RawURLEncoding.DecodeString(pk["user"].(map[string]any)["id"].(string))
	cd, _ := json.Marshal(map[string]any{"type": "webauthn.create", "challenge": pk["challenge"], "origin": testOrigin, "crossOrigin": false})

	enc, err := cbor.CTAP2EncOptions().EncMode()
	if err != nil {
		t.Fatal(err)
	}
	x := a.key.PublicKey.X.FillBytes(make([]byte, 32))
	y := a.key.PublicKey.Y.FillBytes(make([]byte, 32))
	cose, err := enc.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: x, -3: y})
	if err != nil {
		t.Fatal(err)
	}
	var ad bytes.Buffer
	ad.Write(a.rpIDHash[:])
	ad.WriteByte(0x45) // UP | UV | AT
	ad.Write([]byte{0, 0, 0, 0})
	ad.Write(make([]byte, 16)) // AAGUID
	ad.Write([]byte{byte(len(a.credID) >> 8), byte(len(a.credID))})
	ad.Write(a.credID)
	ad.Write(cose)
	att, err := enc.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": ad.Bytes()})
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"id": b64u(a.credID), "rawId": b64u(a.credID), "type": "public-key",
		"response":               map[string]any{"clientDataJSON": b64u(cd), "attestationObject": b64u(att)},
		"clientExtensionResults": map[string]any{},
	}
}

func (a *softAuthenticator) assert(t *testing.T, options map[string]any, counter uint32) map[string]any {
	t.Helper()
	pk := options["publicKey"].(map[string]any)
	cd, _ := json.Marshal(map[string]any{"type": "webauthn.get", "challenge": pk["challenge"], "origin": testOrigin, "crossOrigin": false})
	var ad bytes.Buffer
	ad.Write(a.rpIDHash[:])
	ad.WriteByte(0x05) // UP | UV
	ad.Write([]byte{byte(counter >> 24), byte(counter >> 16), byte(counter >> 8), byte(counter)})
	h := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte{}, ad.Bytes()...), h[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"id": b64u(a.credID), "rawId": b64u(a.credID), "type": "public-key",
		"response": map[string]any{
			"clientDataJSON": b64u(cd), "authenticatorData": b64u(ad.Bytes()),
			"signature": b64u(sig), "userHandle": b64u(a.userID),
		},
		"clientExtensionResults": map[string]any{},
	}
}

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("bad json (%d): %s", w.Code, w.Body.String())
	}
	return m
}

func TestPasskeyRegisterLoginAndCloneDetection(t *testing.T) {
	h := newAuthHarness(t)
	h.setup()
	auth := newSoftAuthenticator(testHost)

	// Register.
	w := h.do(http.MethodPost, "/admin/api/auth/passkey/register/begin", gin.H{})
	if w.Code != http.StatusOK {
		t.Fatalf("register begin = %d %s", w.Code, w.Body.String())
	}
	begin := decode(t, w)
	opts := begin["options"].(map[string]any)
	sel := opts["publicKey"].(map[string]any)["authenticatorSelection"].(map[string]any)
	if sel["residentKey"] != "required" {
		t.Fatalf("passkeys must be discoverable, got %v", sel)
	}
	cred := auth.register(t, opts)
	w = h.do(http.MethodPost, "/admin/api/auth/passkey/register/finish?label=MacBook&challenge_id="+begin["challenge_id"].(string), cred)
	if w.Code != http.StatusOK {
		t.Fatalf("register finish = %d %s", w.Code, w.Body.String())
	}
	// A challenge is single-use.
	w = h.do(http.MethodPost, "/admin/api/auth/passkey/register/finish?label=x&challenge_id="+begin["challenge_id"].(string), cred)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("replayed registration = %d, want 400", w.Code)
	}
	acct := decode(t, h.do(http.MethodGet, "/admin/api/auth/account", nil))
	if keys := acct["passkeys"].([]any); len(keys) != 1 || keys[0].(map[string]any)["label"] != "MacBook" {
		t.Fatalf("account passkeys = %v", acct["passkeys"])
	}

	// Sign out, then log in with the passkey alone (no username).
	h.do(http.MethodPost, "/admin/auth/logout", gin.H{})
	h.cookie = ""
	login := func(counter uint32) *httptest.ResponseRecorder {
		w := h.do(http.MethodPost, "/admin/auth/passkey/login/begin", gin.H{})
		if w.Code != http.StatusOK {
			t.Fatalf("login begin = %d %s", w.Code, w.Body.String())
		}
		b := decode(t, w)
		return h.do(http.MethodPost, "/admin/auth/passkey/login/finish?challenge_id="+b["challenge_id"].(string),
			auth.assert(t, b["options"].(map[string]any), counter))
	}
	w = login(1)
	if w.Code != http.StatusOK {
		t.Fatalf("passkey login = %d %s", w.Code, w.Body.String())
	}
	h.cookie = h.takeCookie(w)
	if w := h.do(http.MethodGet, "/admin/api/keys", nil); w.Code != http.StatusOK {
		t.Fatalf("session from passkey = %d", w.Code)
	}
	// Counter going backwards means a cloned authenticator → refused.
	h.cookie = ""
	login(2) // legitimate use, counter 2
	if w := login(1); w.Code != http.StatusUnauthorized {
		t.Fatalf("replayed/cloned counter = %d, want 401", w.Code)
	}

	// A signature from a different key is refused.
	other := newSoftAuthenticator(testHost)
	other.credID, other.userID = auth.credID, auth.userID
	wb := h.do(http.MethodPost, "/admin/auth/passkey/login/begin", gin.H{})
	bb := decode(t, wb)
	w = h.do(http.MethodPost, "/admin/auth/passkey/login/finish?challenge_id="+bb["challenge_id"].(string),
		other.assert(t, bb["options"].(map[string]any), 9))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("forged signature = %d, want 401", w.Code)
	}
}

func TestPasskeyNeedsDomainNotIP(t *testing.T) {
	h := newAuthHarness(t)
	h.setup()
	w := h.do(http.MethodPost, "/admin/api/auth/passkey/register/begin", gin.H{}, func(r *http.Request) {
		r.Host = "203.0.113.7:8000"
		r.Header.Set("Origin", "https://203.0.113.7:8000")
	})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "域名") {
		t.Fatalf("IP host = %d %s", w.Code, w.Body.String())
	}
}
