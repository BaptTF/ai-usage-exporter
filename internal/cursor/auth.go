package cursor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Cursor auth — two modes, adapted to secret managers:
//
// 1) User API key (canonical): CURSOR_API_KEY (crsr_…).
//    POST https://api2.cursor.sh/auth/exchange_user_api_key
//    Authorization: Bearer <crsr_…>, body {}
//    -> {accessToken, refreshToken}. The access JWT (~1h) is used as Bearer
//    for DashboardService. The returned refreshToken is NOT renewable via
//    /oauth/token: the key is exchanged again on expiry or 401.
//
// 2) OAuth refresh (historical): CURSOR_REFRESH_TOKEN.
//    POST https://api2.cursor.sh/oauth/token
//    {"grant_type":"refresh_token","client_id":"…","refresh_token":"…"}
//
// Safety net: a CURSOR_REFRESH_TOKEN starting with "crsr_" is treated as an
// API key, for a deployment that overwrote the refresh secret.
//
// Priority: CURSOR_API_KEY > CURSOR_REFRESH_TOKEN.
// The access JWT lives in process memory only and is never logged.

const (
	defaultOAuthURL    = "https://api2.cursor.sh/oauth/token"
	defaultExchangeURL = "https://api2.cursor.sh/auth/exchange_user_api_key"
	// oauthClientID is Cursor's public app client_id.
	oauthClientID = "KbZUR41cY7W6zRSdpSUJ7I7mLYBKOCmB"
	// refreshSkew renews the access JWT when it expires in less than 3 min,
	// or immediately on 401.
	refreshSkew = 3 * time.Minute

	apiKeyPrefix = "crsr_"
)

type authMode int

const (
	authModeNone authMode = iota
	authModeAPIKey
	authModeRefresh
)

func (m authMode) String() string {
	switch m {
	case authModeAPIKey:
		return "api_key"
	case authModeRefresh:
		return "oauth_refresh"
	default:
		return "none"
	}
}

// AuthError signals a revoked or invalid credential. The exporter keeps
// running and surfaces the failure through collection metrics.
type AuthError struct {
	Msg string
}

func (e *AuthError) Error() string { return e.Msg }

// IsAuthError reports whether err is (or wraps) an AuthError.
func IsAuthError(err error) bool {
	_, ok := err.(*AuthError)
	return ok
}

type tokenBundle struct {
	access  string
	refresh string // OAuth only; API-key mode re-exchanges the durable key
	exp     time.Time
}

// authenticator obtains an access JWT (key exchange or OAuth) in memory.
type authenticator struct {
	http        *http.Client
	oauthURL    string
	exchangeURL string
	clientID    string
	mode        authMode
	secretVar   string
	apiKey      string
	mu          sync.Mutex
	bundle      tokenBundle
	now         func() time.Time
}

func newAuthenticator(httpClient *http.Client, cfg Config) *authenticator {
	oauthURL := cfg.OAuthURL
	if oauthURL == "" {
		oauthURL = defaultOAuthURL
	}
	exchangeURL := cfg.ExchangeURL
	if exchangeURL == "" {
		exchangeURL = defaultExchangeURL
	}

	mode, secret, secretVar := cfg.resolveAuth()
	a := &authenticator{
		http:        httpClient,
		oauthURL:    oauthURL,
		exchangeURL: exchangeURL,
		clientID:    oauthClientID,
		mode:        mode,
		secretVar:   secretVar,
		now:         time.Now,
	}
	switch mode {
	case authModeAPIKey:
		a.apiKey = secret
	case authModeRefresh:
		a.bundle.refresh = secret
	}
	return a
}

// accessToken returns a valid access JWT, renewing when needed.
func (a *authenticator) accessToken(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	switch a.mode {
	case authModeNone:
		return "", &AuthError{Msg: "no Cursor credential (CURSOR_API_KEY or CURSOR_REFRESH_TOKEN)"}
	case authModeAPIKey:
		if a.apiKey == "" {
			return "", &AuthError{Msg: a.secretVar + " missing"}
		}
	case authModeRefresh:
		if a.bundle.refresh == "" {
			return "", &AuthError{Msg: a.secretVar + " missing"}
		}
	}

	if a.bundle.access != "" && a.now().Add(refreshSkew).Before(a.bundle.exp) {
		return a.bundle.access, nil
	}
	return a.renewLocked(ctx)
}

// invalidateAccess forces a renewal on the next call (for example after 401).
func (a *authenticator) invalidateAccess() {
	a.mu.Lock()
	a.bundle.access = ""
	a.bundle.exp = time.Time{}
	a.mu.Unlock()
}

func (a *authenticator) renewLocked(ctx context.Context) (string, error) {
	switch a.mode {
	case authModeAPIKey:
		return a.exchangeLocked(ctx)
	case authModeRefresh:
		return a.refreshLocked(ctx)
	default:
		return "", &AuthError{Msg: "no Cursor credential (CURSOR_API_KEY or CURSOR_REFRESH_TOKEN)"}
	}
}

type oauthResponse struct {
	AccessToken       string `json:"access_token"`
	IDToken           string `json:"id_token"`
	RefreshToken      string `json:"refresh_token"`
	ShouldLogout      *bool  `json:"shouldLogout"`
	ShouldLogoutSnake *bool  `json:"should_logout"`
	Error             string `json:"error"`
	ErrorDescription  string `json:"error_description"`
}

type exchangeResponse struct {
	AccessToken       string `json:"accessToken"`
	RefreshToken      string `json:"refreshToken"`
	AccessTokenSnake  string `json:"access_token"`
	RefreshTokenSnake string `json:"refresh_token"`
	ShouldLogout      *bool  `json:"shouldLogout"`
	ShouldLogoutSnake *bool  `json:"should_logout"`
	Error             string `json:"error"`
	ErrorDescription  string `json:"error_description"`
}

func (e exchangeResponse) access() string {
	if v := strings.TrimSpace(e.AccessToken); v != "" {
		return v
	}
	return strings.TrimSpace(e.AccessTokenSnake)
}

func (a *authenticator) exchangeLocked(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.exchangeURL, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+a.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ai-usage-exporter/1.0")

	resp, err := a.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("network error (exchange_user_api_key): %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read error (exchange_user_api_key): %w", err)
	}

	var er exchangeResponse
	if err := json.Unmarshal(raw, &er); err != nil {
		return "", fmt.Errorf("JSON parse error (exchange_user_api_key): %w", err)
	}

	if logoutFlag(er.ShouldLogout, er.ShouldLogoutSnake) {
		return "", &AuthError{
			Msg: fmt.Sprintf("Cursor authentication revoked (shouldLogout): renew %s in Cursor Dashboard > API Keys", a.secretVar),
		}
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", &AuthError{
			Msg: fmt.Sprintf("Cursor API key rejected (HTTP %d): %s is invalid or revoked — regenerate it in Cursor Dashboard > API Keys", resp.StatusCode, a.secretVar),
		}
	}
	if resp.StatusCode != http.StatusOK {
		detail := er.Error
		if er.ErrorDescription != "" {
			detail = er.Error + ": " + er.ErrorDescription
		}
		if detail == "" {
			detail = truncate(string(raw), 200)
		}
		return "", fmt.Errorf("exchange_user_api_key HTTP %d: %s", resp.StatusCode, detail)
	}

	access := er.access()
	if access == "" {
		return "", &AuthError{Msg: fmt.Sprintf("exchange_user_api_key: accessToken missing — check %s", a.secretVar)}
	}

	// The refreshToken returned by the exchange is NOT usable via /oauth/token:
	// it is deliberately ignored and the durable key is exchanged again.
	exp, ok := jwtExp(access)
	if !ok {
		exp = a.now().Add(refreshSkew)
	}

	a.bundle.access = access
	a.bundle.exp = exp
	return access, nil
}

func (a *authenticator) refreshLocked(ctx context.Context) (string, error) {
	body, err := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     a.clientID,
		"refresh_token": a.bundle.refresh,
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.oauthURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ai-usage-exporter/1.0")

	resp, err := a.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("network error (oauth/token): %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read error (oauth/token): %w", err)
	}

	var or oauthResponse
	if err := json.Unmarshal(raw, &or); err != nil {
		return "", fmt.Errorf("JSON parse error (oauth/token): %w", err)
	}

	if logoutFlag(or.ShouldLogout, or.ShouldLogoutSnake) {
		return "", &AuthError{
			Msg: fmt.Sprintf("Cursor authentication revoked (shouldLogout): renew %s and update the secret", a.secretVar),
		}
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", &AuthError{
			Msg: fmt.Sprintf("Cursor token rejected (HTTP %d): refresh expired or revoked — check %s", resp.StatusCode, a.secretVar),
		}
	}
	if resp.StatusCode != http.StatusOK {
		detail := or.Error
		if or.ErrorDescription != "" {
			detail = or.Error + ": " + or.ErrorDescription
		}
		if detail == "" {
			detail = truncate(string(raw), 200)
		}
		return "", fmt.Errorf("oauth/token HTTP %d: %s", resp.StatusCode, detail)
	}

	access := strings.TrimSpace(or.AccessToken)
	if access == "" {
		return "", &AuthError{Msg: fmt.Sprintf("oauth/token: access_token missing — renew %s", a.secretVar)}
	}

	exp, ok := jwtExp(access)
	if !ok {
		// Without a readable exp, accept the token but treat it as expiring
		// soon so the next cycle refreshes it.
		exp = a.now().Add(refreshSkew)
	}

	if rotated := strings.TrimSpace(or.RefreshToken); rotated != "" && rotated != a.bundle.refresh {
		a.bundle.refresh = rotated
	}

	a.bundle.access = access
	a.bundle.exp = exp
	return access, nil
}

func logoutFlag(camel, snake *bool) bool {
	if camel != nil && *camel {
		return true
	}
	if snake != nil && *snake {
		return true
	}
	return false
}

// jwtExp reads the exp claim without verifying the signature: the API decides
// validity, this only schedules the refresh.
func jwtExp(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return time.Time{}, false
		}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0).UTC(), true
}

// jwtSub extracts the subject from the JWT (WorkosCursorSessionToken cookie).
func jwtSub(token string) (string, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return "", false
		}
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", false
	}
	sub := strings.TrimSpace(claims.Sub)
	if sub == "" {
		return "", false
	}
	// Frequent WorkOS form: "auth0|user_xxx" -> session id is the second part.
	if _, id, ok := strings.Cut(sub, "|"); ok && strings.TrimSpace(id) != "" {
		return strings.TrimSpace(id), true
	}
	return sub, true
}
