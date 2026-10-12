package api

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"golang.org/x/crypto/bcrypt"

	"github.com/gentpan/grok2api/internal/jsonstore"
	"github.com/gentpan/grok2api/internal/logger"
)

const (
	sessionCookieName = "g2a_session"
	sessionTTL        = 12 * time.Hour
	challengeTTL      = 5 * time.Minute
	minPasswordLen    = 10
	maxPasswordBytes  = 72 // bcrypt silently ignores anything past this
	maxSessions       = 64
	// maxPendingChallenges caps live WebAuthn challenges so unauthenticated
	// /passkey/login/begin calls cannot grow the table without bound.
	maxPendingChallenges = 256
	// maxChallengesPerIP keeps one peer from using up the whole table.
	maxChallengesPerIP = 8
)

// defaultBcryptCost is the production bcrypt work factor.
const defaultBcryptCost = 12

var usernameRE = regexp.MustCompile(`^[A-Za-z0-9_.@-]{3,64}$`)

// passkeyRecord is one registered passkey.
type passkeyRecord struct {
	Label      string              `json:"label"`
	CreatedAt  int64               `json:"created_at"`
	LastUsedAt int64               `json:"last_used_at"`
	Credential webauthn.Credential `json:"credential"`
}

// adminAccount is the single panel account. The zero value means "not set
// up yet"; the persisted file is this object, bare.
type adminAccount struct {
	Username     string          `json:"username"`
	PasswordHash string          `json:"password_hash"`
	UserHandle   []byte          `json:"user_handle"`
	Passkeys     []passkeyRecord `json:"passkeys"`
	UpdatedAt    int64           `json:"updated_at"`
}

func (a *adminAccount) configured() bool { return a.Username != "" && a.PasswordHash != "" }

type adminSession struct {
	expires time.Time
	via     string // "password" | "passkey" | "setup"
}

type pendingChallenge struct {
	data    webauthn.SessionData
	kind    string // "register" | "login"
	ip      string // peer that started it
	expires time.Time
}

// adminAuth stores the panel account (0600 JSON), live sessions and pending
// WebAuthn challenges. Sessions live in memory: a restart signs everyone out.
type adminAuth struct {
	store *jsonstore.File[adminAccount]
	cost  int // bcrypt work factor

	mu         sync.Mutex // guards sessions and challenges
	sessions   map[string]*adminSession
	challenges map[string]*pendingChallenge

	dummyOnce sync.Once
	dummyHash []byte
}

func newAdminAuth(path string, bcryptCost int) *adminAuth {
	if bcryptCost <= 0 {
		bcryptCost = defaultBcryptCost
	}
	f, err := jsonstore.Open(path, jsonstore.Config[adminAccount]{
		OnError: func(err error) { logger.Warnf("admin account persist failed: error=%v", err) },
		Normalize: func(a *adminAccount) {
			if !a.configured() {
				*a = adminAccount{}
			}
		},
	})
	if err != nil {
		logger.Warnf("admin account unreadable, setup required: error=%v", err)
	}
	return &adminAuth{
		store: f, cost: bcryptCost,
		sessions: map[string]*adminSession{}, challenges: map[string]*pendingChallenge{},
	}
}

// account returns a snapshot of the panel account.
func (a *adminAuth) account() (acct adminAccount, ok bool) {
	a.store.View(func(cur *adminAccount) {
		acct = *cur
		acct.UserHandle = append([]byte(nil), cur.UserHandle...)
		acct.Passkeys = append([]passkeyRecord(nil), cur.Passkeys...)
	})
	return acct, acct.configured()
}

// flush persists the account (it is written synchronously already; kept for
// the Server.Close contract).
func (a *adminAuth) flush() error {
	if a == nil {
		return nil
	}
	return a.store.Flush()
}

// ---------------------------------------------------------------- sessions

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func randomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func (a *adminAuth) newSession(via string) (string, error) {
	tok, err := randomToken(32)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for k, s := range a.sessions {
		if now.After(s.expires) {
			delete(a.sessions, k)
		}
	}
	if len(a.sessions) >= maxSessions { // drop the oldest expiry
		var oldest string
		var oldestT time.Time
		for k, s := range a.sessions {
			if oldest == "" || s.expires.Before(oldestT) {
				oldest, oldestT = k, s.expires
			}
		}
		delete(a.sessions, oldest)
	}
	a.sessions[hashToken(tok)] = &adminSession{expires: now.Add(sessionTTL), via: via}
	return tok, nil
}

// sessionFor returns the live session behind the request's cookie, if any.
func (a *adminAuth) sessionFor(r *http.Request) *adminSession {
	if a == nil {
		return nil
	}
	ck, err := r.Cookie(sessionCookieName)
	if err != nil || ck.Value == "" {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.sessions[hashToken(ck.Value)]
	if s == nil {
		return nil
	}
	if time.Now().After(s.expires) {
		delete(a.sessions, hashToken(ck.Value))
		return nil
	}
	return s
}

func (a *adminAuth) dropSession(r *http.Request) {
	if ck, err := r.Cookie(sessionCookieName); err == nil {
		a.mu.Lock()
		delete(a.sessions, hashToken(ck.Value))
		a.mu.Unlock()
	}
}

func (a *adminAuth) dropAllSessions() {
	a.mu.Lock()
	a.sessions = map[string]*adminSession{}
	a.mu.Unlock()
}

// ---------------------------------------------------------- request helpers

// peerIsLoopback reports whether the TCP peer is this machine (a local
// reverse proxy such as Caddy).
func peerIsLoopback(r *http.Request) bool {
	ip := net.ParseIP(remoteHost(r))
	return ip != nil && ip.IsLoopback()
}

// clientIP is the real client address: the TCP peer, or — only when the peer
// is a local reverse proxy — the last X-Forwarded-For entry it appended.
func clientIP(r *http.Request) string {
	peer := remoteHost(r)
	if peerIsLoopback(r) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); net.ParseIP(ip) != nil {
				return ip
			}
		}
	}
	return peer
}

func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return peerIsLoopback(r) && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// publicOrigin returns the origin and WebAuthn RP ID the browser sees.
// app.public_origin overrides it when set.
func (s *Server) publicOrigin(r *http.Request) (origin, rpID string, err error) {
	if v := strings.TrimSpace(s.config().GetStr("app.public_origin", "")); v != "" {
		u, perr := url.Parse(v)
		if perr != nil || u.Scheme == "" || u.Host == "" {
			return "", "", errors.New("app.public_origin is not a valid origin")
		}
		return u.Scheme + "://" + u.Host, u.Hostname(), nil
	}
	scheme := "http"
	if requestIsHTTPS(r) {
		scheme = "https"
	}
	host := r.Host
	if h, _, serr := net.SplitHostPort(host); serr == nil {
		host = h
	}
	return scheme + "://" + r.Host, host, nil
}

// sameOrigin is the CSRF guard for cookie-authenticated unsafe requests.
func (s *Server) sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return false
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" {
		return false
	}
	want, _, perr := s.publicOrigin(r)
	if perr != nil {
		return false
	}
	wu, err := url.Parse(want)
	return err == nil && strings.EqualFold(u.Host, wu.Host)
}

func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

func setSessionCookie(c *gin.Context, token string, maxAge int) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name: sessionCookieName, Value: token, Path: "/admin",
		MaxAge: maxAge, HttpOnly: true, Secure: requestIsHTTPS(c.Request), SameSite: http.SameSiteStrictMode,
	})
}

// ---------------------------------------------------------------- passwords

// spendPasswordTime burns a bcrypt comparison so that unknown users and wrong
// passwords take about the same time.
func (a *adminAuth) spendPasswordTime(pw string) {
	a.dummyOnce.Do(func() { a.dummyHash, _ = bcrypt.GenerateFromPassword([]byte("dummy-password"), a.cost) })
	_ = bcrypt.CompareHashAndPassword(a.dummyHash, []byte(pw))
}

func validatePassword(pw, username string) error {
	switch {
	case len([]rune(pw)) < minPasswordLen:
		return errors.New("密码至少 10 位")
	case len(pw) > maxPasswordBytes:
		return errors.New("密码不能超过 72 字节")
	case strings.EqualFold(pw, username):
		return errors.New("密码不能与用户名相同")
	}
	return nil
}

// ------------------------------------------------------------ WebAuthn glue

type waUser struct {
	name        string
	handle      []byte
	credentials []webauthn.Credential
}

func (u waUser) WebAuthnID() []byte                         { return u.handle }
func (u waUser) WebAuthnName() string                       { return u.name }
func (u waUser) WebAuthnDisplayName() string                { return u.name }
func (u waUser) WebAuthnCredentials() []webauthn.Credential { return u.credentials }

func waUserOf(acct adminAccount) waUser {
	u := waUser{name: acct.Username, handle: append([]byte(nil), acct.UserHandle...)}
	for _, p := range acct.Passkeys {
		u.credentials = append(u.credentials, p.Credential)
	}
	return u
}

func (s *Server) newWebAuthn(r *http.Request) (*webauthn.WebAuthn, error) {
	origin, rpID, err := s.publicOrigin(r)
	if err != nil {
		return nil, err
	}
	if net.ParseIP(rpID) != nil {
		return nil, errors.New("通行密钥需要通过域名访问（不能使用 IP 地址）")
	}
	return webauthn.New(&webauthn.Config{
		RPID:          rpID,
		RPDisplayName: "Grok2API",
		RPOrigins:     []string{origin},
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			RequireResidentKey: protocol.ResidentKeyRequired(),
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			UserVerification:   protocol.VerificationPreferred,
		},
	})
}

func (a *adminAuth) putChallenge(kind, ip string, sd *webauthn.SessionData) (string, error) {
	id, err := randomToken(18)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for k, c := range a.challenges {
		if now.After(c.expires) {
			delete(a.challenges, k)
		}
	}
	if len(a.challenges) > maxPendingChallenges {
		return "", errors.New("too many pending challenges")
	}
	fromIP := 0
	for _, c := range a.challenges {
		if c.ip == ip {
			fromIP++
		}
	}
	if fromIP >= maxChallengesPerIP {
		return "", errors.New("too many pending challenges from this address")
	}
	a.challenges[id] = &pendingChallenge{data: *sd, kind: kind, ip: ip, expires: now.Add(challengeTTL)}
	return id, nil
}

// takeChallenge consumes a challenge: each one is single-use.
func (a *adminAuth) takeChallenge(id, kind string) (*webauthn.SessionData, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.challenges[id]
	delete(a.challenges, id)
	if c == nil || c.kind != kind || time.Now().After(c.expires) {
		return nil, false
	}
	sd := c.data
	return &sd, true
}

// ---------------------------------------------------------------- handlers

// authFail is returned from jsonstore update callbacks to carry the HTTP
// answer out of the locked section; the store rolls its state back.
type authFail struct {
	status int
	msg    string
}

func (e *authFail) Error() string { return e.msg }

// writeAuthError answers with the panel's own error envelope for either an
// *authFail or an unexpected persistence error.
func writeAuthError(c *gin.Context, err error, persistMsg string) {
	var af *authFail
	if errors.As(err, &af) {
		authErr(c, af.status, af.msg)
		return
	}
	logger.Warnf("admin account persist failed: error=%v", err)
	authErr(c, http.StatusInternalServerError, persistMsg)
}

// handleAuthStatus is public: it only reveals whether setup is needed and
// whether the caller is signed in.
func (s *Server) handleAuthStatus(c *gin.Context) {
	a := s.authStore()
	acct, hasAcct := a.account()
	sess := a.sessionFor(c.Request)
	username := ""
	if sess != nil && hasAcct {
		username = acct.Username
	}
	_, rpID, _ := s.publicOrigin(c.Request)
	c.JSON(http.StatusOK, gin.H{
		"setup_required":    !hasAcct,
		"authenticated":     sess != nil,
		"username":          username,
		"passkey_available": hasAcct && len(acct.Passkeys) > 0,
		"passkey_supported": net.ParseIP(rpID) == nil,
	})
}

func (s *Server) appKeyMatches(candidate string) bool {
	key := strings.TrimSpace(s.config().GetStr("app.app_key", ""))
	return key != "" && subtle.ConstantTimeCompare([]byte(strings.TrimSpace(candidate)), []byte(key)) == 1
}

// handleAuthSetup creates the first account. It requires the app_key so that
// whoever reaches the page first cannot claim the panel.
func (s *Server) handleAuthSetup(c *gin.Context) {
	var body struct {
		AppKey   string `json:"app_key"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		authErr(c, http.StatusBadRequest, "请求格式不正确")
		return
	}
	ip := clientIP(c.Request)
	lim := s.adminLimiter()
	if lim.blocked(ip) {
		authErr(c, http.StatusTooManyRequests, "尝试次数过多，请稍后再试")
		return
	}
	if !s.appKeyMatches(body.AppKey) {
		lim.fail(ip)
		authErr(c, http.StatusUnauthorized, "管理密钥不正确")
		return
	}
	if strings.TrimSpace(s.config().GetStr("app.app_key", "")) == defaultAdminKey && !isLocalDirectRequest(c.Request) {
		authErr(c, http.StatusUnauthorized, "管理密钥仍是内置默认值，请先在配置文件中设置自定义 app_key")
		return
	}
	if !usernameRE.MatchString(body.Username) {
		authErr(c, http.StatusBadRequest, "用户名需为 3–64 位字母、数字或 _ . @ -")
		return
	}
	if err := validatePassword(body.Password, body.Username); err != nil {
		authErr(c, http.StatusBadRequest, err.Error())
		return
	}
	a := s.authStore()
	hash, err := bcrypt.GenerateFromPassword([]byte(body.Password), a.cost)
	if err != nil {
		authErr(c, http.StatusInternalServerError, "无法处理密码")
		return
	}
	handle := make([]byte, 32)
	if _, err := rand.Read(handle); err != nil {
		authErr(c, http.StatusInternalServerError, "无法生成账号标识")
		return
	}

	err = a.store.Update(func(acct *adminAccount) error {
		if acct.configured() {
			return &authFail{http.StatusConflict, "账号已经设置过了；忘记密码请用管理密钥登录后在「账户安全」里修改"}
		}
		*acct = adminAccount{Username: body.Username, PasswordHash: string(hash), UserHandle: handle, UpdatedAt: time.Now().UnixMilli()}
		return nil
	})
	if err != nil {
		writeAuthError(c, err, "保存失败")
		return
	}

	tok, err := a.newSession("setup")
	if err != nil {
		authErr(c, http.StatusInternalServerError, "无法创建会话")
		return
	}
	setSessionCookie(c, tok, int(sessionTTL.Seconds()))
	c.JSON(http.StatusOK, gin.H{"status": "success", "username": body.Username})
}

func (s *Server) handleAuthLogin(c *gin.Context) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		authErr(c, http.StatusBadRequest, "请求格式不正确")
		return
	}
	ip := clientIP(c.Request)
	lim := s.adminLimiter()
	if lim.blocked(ip) {
		authErr(c, http.StatusTooManyRequests, "尝试次数过多，请稍后再试")
		return
	}
	a := s.authStore()
	acct, configured := a.account()
	userOK := configured && subtle.ConstantTimeCompare([]byte(body.Username), []byte(acct.Username)) == 1

	ok := false
	if userOK && acct.PasswordHash != "" && len(body.Password) <= maxPasswordBytes {
		ok = bcrypt.CompareHashAndPassword([]byte(acct.PasswordHash), []byte(body.Password)) == nil
	} else {
		a.spendPasswordTime(body.Password)
	}
	if !ok {
		lim.fail(ip)
		authErr(c, http.StatusUnauthorized, "用户名或密码不正确")
		return
	}
	tok, err := a.newSession("password")
	if err != nil {
		authErr(c, http.StatusInternalServerError, "无法创建会话")
		return
	}
	setSessionCookie(c, tok, int(sessionTTL.Seconds()))
	c.JSON(http.StatusOK, gin.H{"status": "success"})
}

func (s *Server) handleAuthLogout(c *gin.Context) {
	a := s.authStore()
	if a.sessionFor(c.Request) != nil && !s.sameOrigin(c.Request) {
		authErr(c, http.StatusForbidden, "来源校验失败")
		return
	}
	a.dropSession(c.Request)
	setSessionCookie(c, "", -1)
	c.JSON(http.StatusOK, gin.H{"status": "success"})
}

// ---- passkey login (public) ----

func (s *Server) handlePasskeyLoginBegin(c *gin.Context) {
	a := s.authStore()
	if acct, ok := a.account(); !ok || len(acct.Passkeys) == 0 {
		authErr(c, http.StatusNotFound, "还没有注册通行密钥")
		return
	}
	wa, err := s.newWebAuthn(c.Request)
	if err != nil {
		authErr(c, http.StatusBadRequest, err.Error())
		return
	}
	assertion, sd, err := wa.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationPreferred))
	if err != nil {
		authErr(c, http.StatusInternalServerError, "无法发起通行密钥登录")
		return
	}
	id, err := a.putChallenge("login", clientIP(c.Request), sd)
	if err != nil {
		authErr(c, http.StatusTooManyRequests, "请求过于频繁")
		return
	}
	c.JSON(http.StatusOK, gin.H{"challenge_id": id, "options": assertion})
}

func (s *Server) handlePasskeyLoginFinish(c *gin.Context) {
	ip := clientIP(c.Request)
	lim := s.adminLimiter()
	if lim.blocked(ip) {
		authErr(c, http.StatusTooManyRequests, "尝试次数过多，请稍后再试")
		return
	}
	a := s.authStore()
	sd, ok := a.takeChallenge(c.Query("challenge_id"), "login")
	if !ok {
		authErr(c, http.StatusBadRequest, "登录请求已过期，请重试")
		return
	}
	wa, err := s.newWebAuthn(c.Request)
	if err != nil {
		authErr(c, http.StatusBadRequest, err.Error())
		return
	}
	var matched []byte
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		acct, configured := a.account()
		if !configured || subtle.ConstantTimeCompare(userHandle, acct.UserHandle) != 1 {
			return nil, errors.New("unknown user")
		}
		matched = append([]byte(nil), rawID...)
		return waUserOf(acct), nil
	}
	_, cred, err := wa.FinishPasskeyLogin(handler, *sd, c.Request)
	if err != nil || cred == nil {
		lim.fail(ip)
		authErr(c, http.StatusUnauthorized, "通行密钥校验失败")
		return
	}
	if cred.Authenticator.CloneWarning {
		lim.fail(ip)
		authErr(c, http.StatusUnauthorized, "检测到通行密钥可能被克隆，已拒绝登录")
		return
	}
	// Persist the new signature counter and last-used time.
	_ = a.store.Update(func(acct *adminAccount) error {
		for i := range acct.Passkeys {
			if subtle.ConstantTimeCompare(acct.Passkeys[i].Credential.ID, matched) == 1 {
				acct.Passkeys[i].Credential = *cred
				acct.Passkeys[i].LastUsedAt = time.Now().UnixMilli()
			}
		}
		return nil
	})

	tok, err := a.newSession("passkey")
	if err != nil {
		authErr(c, http.StatusInternalServerError, "无法创建会话")
		return
	}
	setSessionCookie(c, tok, int(sessionTTL.Seconds()))
	c.JSON(http.StatusOK, gin.H{"status": "success"})
}

// ---- authenticated account management (under /admin/api/auth) ----

func (s *Server) handleAccountGet(c *gin.Context) {
	acct, ok := s.authStore().account()
	if !ok {
		c.JSON(http.StatusOK, gin.H{"configured": false, "passkeys": []gin.H{}})
		return
	}
	keys := make([]gin.H, 0, len(acct.Passkeys))
	for _, p := range acct.Passkeys {
		keys = append(keys, gin.H{
			"id":           base64.RawURLEncoding.EncodeToString(p.Credential.ID),
			"label":        p.Label,
			"created_at":   p.CreatedAt,
			"last_used_at": p.LastUsedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{"configured": true, "username": acct.Username, "passkeys": keys})
}

// handleAccountUpdate changes username and/or password. A password session
// must prove the current password; the app_key (recovery) does not need to.
func (s *Server) handleAccountUpdate(c *gin.Context) {
	var body struct {
		CurrentPassword string `json:"current_password"`
		Username        string `json:"username"`
		NewPassword     string `json:"new_password"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		authErr(c, http.StatusBadRequest, "请求格式不正确")
		return
	}
	viaBearer := c.GetString(adminViaKey) == "key"
	a := s.authStore()
	err := a.store.Update(func(acct *adminAccount) error {
		if !acct.configured() {
			return &authFail{http.StatusConflict, "还没有设置账号，请先在登录页完成首次设置"}
		}
		if !viaBearer {
			if body.CurrentPassword == "" || len(body.CurrentPassword) > maxPasswordBytes ||
				bcrypt.CompareHashAndPassword([]byte(acct.PasswordHash), []byte(body.CurrentPassword)) != nil {
				return &authFail{http.StatusForbidden, "当前密码不正确"}
			}
		}
		newName := acct.Username
		if body.Username != "" {
			if !usernameRE.MatchString(body.Username) {
				return &authFail{http.StatusBadRequest, "用户名需为 3–64 位字母、数字或 _ . @ -"}
			}
			newName = body.Username
		}
		newHash := acct.PasswordHash
		if body.NewPassword != "" {
			if err := validatePassword(body.NewPassword, newName); err != nil {
				return &authFail{http.StatusBadRequest, err.Error()}
			}
			h, err := bcrypt.GenerateFromPassword([]byte(body.NewPassword), a.cost)
			if err != nil {
				return &authFail{http.StatusInternalServerError, "无法处理密码"}
			}
			newHash = string(h)
		}
		acct.Username = newName
		acct.PasswordHash = newHash
		acct.UpdatedAt = time.Now().UnixMilli()
		return nil
	})
	if err != nil {
		writeAuthError(c, err, "保存失败")
		return
	}
	if body.NewPassword != "" {
		a.dropAllSessions() // a password change signs every device out
		setSessionCookie(c, "", -1)
	}
	c.JSON(http.StatusOK, gin.H{"status": "success", "relogin": body.NewPassword != ""})
}

func (s *Server) handlePasskeyRegisterBegin(c *gin.Context) {
	a := s.authStore()
	acct, ok := a.account()
	if !ok {
		authErr(c, http.StatusConflict, "请先设置用户名和密码")
		return
	}
	user := waUserOf(acct)

	wa, err := s.newWebAuthn(c.Request)
	if err != nil {
		authErr(c, http.StatusBadRequest, err.Error())
		return
	}
	var exclude []protocol.CredentialDescriptor
	for i := range user.credentials {
		exclude = append(exclude, user.credentials[i].Descriptor())
	}
	creation, sd, err := wa.BeginRegistration(user, webauthn.WithExclusions(exclude))
	if err != nil {
		authErr(c, http.StatusInternalServerError, "无法发起注册")
		return
	}
	id, err := a.putChallenge("register", clientIP(c.Request), sd)
	if err != nil {
		authErr(c, http.StatusTooManyRequests, "请求过于频繁")
		return
	}
	c.JSON(http.StatusOK, gin.H{"challenge_id": id, "options": creation})
}

func (s *Server) handlePasskeyRegisterFinish(c *gin.Context) {
	a := s.authStore()
	sd, ok := a.takeChallenge(c.Query("challenge_id"), "register")
	if !ok {
		authErr(c, http.StatusBadRequest, "注册请求已过期，请重试")
		return
	}
	label := strings.TrimSpace(c.Query("label"))
	if label == "" {
		label = "通行密钥"
	}
	if len([]rune(label)) > 40 {
		label = string([]rune(label)[:40])
	}
	acct, configured := a.account()
	if !configured {
		authErr(c, http.StatusConflict, "请先设置用户名和密码")
		return
	}
	user := waUserOf(acct)

	wa, err := s.newWebAuthn(c.Request)
	if err != nil {
		authErr(c, http.StatusBadRequest, err.Error())
		return
	}
	cred, err := wa.FinishRegistration(user, *sd, c.Request)
	if err != nil {
		authErr(c, http.StatusBadRequest, "通行密钥注册校验失败："+err.Error())
		return
	}
	err = a.store.Update(func(cur *adminAccount) error {
		if !cur.configured() {
			return &authFail{http.StatusConflict, "账号状态已变化，请重试"}
		}
		cur.Passkeys = append(cur.Passkeys, passkeyRecord{
			Label: label, CreatedAt: time.Now().UnixMilli(), Credential: *cred,
		})
		return nil
	})
	if err != nil {
		writeAuthError(c, err, "保存失败")
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "success", "id": base64.RawURLEncoding.EncodeToString(cred.ID), "label": label})
}

func (s *Server) handlePasskeyDelete(c *gin.Context) {
	id, err := base64.RawURLEncoding.DecodeString(c.Param("id"))
	if err != nil {
		authErr(c, http.StatusBadRequest, "无效的标识")
		return
	}
	err = s.authStore().store.Update(func(acct *adminAccount) error {
		if !acct.configured() {
			return &authFail{http.StatusNotFound, "没有账号"}
		}
		for i, p := range acct.Passkeys {
			if subtle.ConstantTimeCompare(p.Credential.ID, id) == 1 {
				acct.Passkeys = append(acct.Passkeys[:i:i], acct.Passkeys[i+1:]...)
				return nil
			}
		}
		return &authFail{http.StatusNotFound, "通行密钥不存在"}
	})
	if err != nil {
		writeAuthError(c, err, "保存失败")
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "success"})
}
