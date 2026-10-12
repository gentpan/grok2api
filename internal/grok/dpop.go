package grok

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/gentpan/grok2api/internal/platform"
)

// DPoP (RFC 9449) for console.x.ai: the SSO cookie is exchanged for an access
// token bound to a throwaway P-256 key; every Console request then carries
// "Authorization: DPoP <token>" plus a proof JWT signed with that key.

const (
	dpopRefreshSkew      = 20 * time.Second
	maxDPoPTokenLifetime = time.Hour
	dpopTokenBodyLimit   = 1 << 20
)

type dpopJWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

type dpopSession struct {
	accessToken string
	privateKey  *ecdsa.PrivateKey
	publicJWK   dpopJWK
	expiresAt   time.Time
}

func dpopSessionKey(baseURL, ssoToken string) string {
	digest := sha256.Sum256([]byte(platform.SanitizeToken(ssoToken)))
	return strings.TrimRight(baseURL, "/") + "|" + base64.RawURLEncoding.EncodeToString(digest[:])
}

// getDPoPSession returns a cached session for (baseURL, ssoToken) or exchanges
// the SSO token for a new one. Concurrent callers share a single exchange.
func (t *Transport) getDPoPSession(ctx context.Context, ssoToken, baseURL string, profile proxyProfile) (dpopSession, error) {
	return t.dpop.get(ctx, dpopSessionKey(baseURL, ssoToken), func(ctx context.Context) (dpopSession, error) {
		return t.fetchDPoPSession(ctx, ssoToken, baseURL, profile)
	})
}

func (t *Transport) invalidateDPoPSession(ssoToken, baseURL, accessToken string) {
	t.dpop.invalidate(dpopSessionKey(baseURL, ssoToken), accessToken)
}

// fetchDPoPSession performs the token exchange against {baseURL}/v1/dpop/token.
func (t *Transport) fetchDPoPSession(ctx context.Context, ssoToken, baseURL string, profile proxyProfile) (dpopSession, error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return dpopSession{}, fmt.Errorf("generate Console DPoP key: %w", err)
	}
	publicJWK := publicDPoPJWK(&privateKey.PublicKey)
	payload, err := json.Marshal(map[string]any{"jwk": publicJWK})
	if err != nil {
		return dpopSession{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, consoleV1Endpoint(baseURL, "/dpop/token"), bytes.NewReader(payload))
	if err != nil {
		return dpopSession{}, platform.UpstreamError("build Console DPoP token request failed: "+err.Error(), 502, "")
	}
	req.Header = profile.consoleHeaders(ssoToken, defaultJSONType)
	req.Header.Del("x-cluster")

	resp, err := t.pool.do(ctx, req)
	if err != nil {
		return dpopSession{}, prefixError("Console DPoP token request failed", err)
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, dpopTokenBodyLimit+1))
	if readErr != nil {
		return dpopSession{}, platform.UpstreamError("read Console DPoP token response: "+readErr.Error(), resp.StatusCode, "")
	}
	if len(body) > dpopTokenBodyLimit {
		return dpopSession{}, upstreamErr("Console DPoP token response exceeds 1 MiB")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return dpopSession{}, platform.UpstreamError(
			fmt.Sprintf("Console DPoP token endpoint returned %d", resp.StatusCode), resp.StatusCode, truncBody(body, 400))
	}
	return buildDPoPSession(body, privateKey, publicJWK, time.Now().UTC())
}

// buildDPoPSession validates a token response and binds it to the local key.
// Every failure is an upstream fault (a malformed or foreign token), so it
// surfaces as a 502 rather than an internal error.
func buildDPoPSession(body []byte, key *ecdsa.PrivateKey, jwk dpopJWK, now time.Time) (dpopSession, error) {
	var tokenResponse struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResponse); err != nil {
		return dpopSession{}, upstreamErr("decode Console DPoP token: %v", err)
	}
	if strings.TrimSpace(tokenResponse.AccessToken) == "" || !strings.EqualFold(strings.TrimSpace(tokenResponse.TokenType), "DPoP") {
		return dpopSession{}, upstreamErr("invalid Console DPoP token response")
	}
	if tokenResponse.ExpiresIn <= 0 || time.Duration(tokenResponse.ExpiresIn)*time.Second > maxDPoPTokenLifetime {
		return dpopSession{}, upstreamErr("invalid Console DPoP token lifetime")
	}
	thumbprint, err := dpopJWKThumbprint(jwk)
	if err != nil {
		return dpopSession{}, err
	}
	tokenExpiry, tokenThumbprint, err := parseDPoPAccessToken(tokenResponse.AccessToken)
	if err != nil {
		return dpopSession{}, upstreamErr("%v", err)
	}
	if tokenThumbprint != thumbprint {
		return dpopSession{}, upstreamErr("Console DPoP token is not bound to the local key")
	}
	expiresAt := now.Add(time.Duration(tokenResponse.ExpiresIn) * time.Second)
	if tokenExpiry.Before(expiresAt) {
		expiresAt = tokenExpiry
	}
	if !expiresAt.After(now.Add(dpopRefreshSkew)) {
		return dpopSession{}, upstreamErr("Console DPoP token is expired or near expiry")
	}
	return dpopSession{accessToken: tokenResponse.AccessToken, privateKey: key, publicJWK: jwk, expiresAt: expiresAt}, nil
}

func publicDPoPJWK(key *ecdsa.PublicKey) dpopJWK {
	return dpopJWK{
		Kty: "EC",
		Crv: "P-256",
		X:   base64.RawURLEncoding.EncodeToString(key.X.FillBytes(make([]byte, 32))),
		Y:   base64.RawURLEncoding.EncodeToString(key.Y.FillBytes(make([]byte, 32))),
	}
}

func dpopJWKThumbprint(jwk dpopJWK) (string, error) {
	canonical := struct {
		Crv string `json:"crv"`
		Kty string `json:"kty"`
		X   string `json:"x"`
		Y   string `json:"y"`
	}{Crv: jwk.Crv, Kty: jwk.Kty, X: jwk.X, Y: jwk.Y}
	data, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return base64.RawURLEncoding.EncodeToString(digest[:]), nil
}

func parseDPoPAccessToken(value string) (time.Time, string, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return time.Time{}, "", fmt.Errorf("invalid Console DPoP access token format")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, "", fmt.Errorf("invalid Console DPoP access token payload")
	}
	var claims struct {
		ExpiresAt int64 `json:"exp"`
		CNF       struct {
			JKT string `json:"jkt"`
		} `json:"cnf"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.ExpiresAt <= 0 || strings.TrimSpace(claims.CNF.JKT) == "" {
		return time.Time{}, "", fmt.Errorf("invalid Console DPoP access token claims")
	}
	return time.Unix(claims.ExpiresAt, 0).UTC(), claims.CNF.JKT, nil
}

// applyDPoPAuthorization signs a DPoP proof for req and sets the
// Authorization and DPoP headers.
func applyDPoPAuthorization(req *http.Request, session dpopSession) error {
	if req == nil || req.URL == nil || session.privateKey == nil || strings.TrimSpace(session.accessToken) == "" {
		return fmt.Errorf("invalid Console DPoP request parameters")
	}
	digest := sha256.Sum256([]byte(session.accessToken))
	header := map[string]any{"alg": "ES256", "typ": "dpop+jwt", "jwk": session.publicJWK}
	claims := map[string]any{
		"jti": uuid.NewString(),
		"htm": strings.ToUpper(req.Method),
		"htu": dpopHTU(req),
		"iat": time.Now().UTC().Unix(),
		"ath": base64.RawURLEncoding.EncodeToString(digest[:]),
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return err
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return err
	}
	unsigned := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	hash := sha256.Sum256([]byte(unsigned))
	r, s, err := ecdsa.Sign(rand.Reader, session.privateKey, hash[:])
	if err != nil {
		return fmt.Errorf("sign Console DPoP proof: %w", err)
	}
	signature := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	proof := unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
	req.Header.Set("Authorization", "DPoP "+session.accessToken)
	req.Header.Set("DPoP", proof)
	return nil
}

func dpopHTU(req *http.Request) string {
	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	return req.URL.Scheme + "://" + req.URL.Host + path
}

func consoleV1Endpoint(baseURL, path string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	path = "/" + strings.TrimLeft(strings.TrimSpace(path), "/")
	if strings.HasSuffix(baseURL, "/v1") {
		return baseURL + path
	}
	return baseURL + "/v1" + path
}

// consoleRequestBase returns scheme://host of rawURL, or ConsoleBase when it
// has none; DPoP sessions are cached per base.
func consoleRequestBase(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ConsoleBase
	}
	return u.Scheme + "://" + u.Host
}
