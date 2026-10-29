// Portions reference/adapt CLIProxyAPI (MIT); see NOTICE and
// LICENSES/CLIProxyAPI/LICENSE for retained upstream attribution.
// Package oauth implements the built-in ChatGPT/Codex subscription login
// (Phase 4): PKCE authorization-code OAuth against auth.openai.com, token
// persistence through the local secret store (0600), and lazy refresh on use.
// Token material lives only in memory and the secret store file — it is never
// logged, never echoed by any API, and never written into configs.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// tokenURL is a var so tests can point the flow at a fake token endpoint.
var (
	tokenURL = TokenURL
)

// SetTokenURL overrides the token endpoint (test hook; restore the original
// value when done).
func SetTokenURL(u string) { tokenURL = u }

// CurrentTokenURL returns the active token endpoint.
func CurrentTokenURL() string { return tokenURL }

// OAuth endpoints and client identity of the Codex CLI flow.
const (
	AuthURL     = "https://auth.openai.com/oauth/authorize"
	TokenURL    = "https://auth.openai.com/oauth/token" // #nosec G101 -- public OAuth endpoint, not a credential
	ClientID    = "app_EMoamEEZ73f0CkXaXp7hrann"
	RedirectURI = "http://localhost:1455/auth/callback"
	// DeviceRedirectURI is returned by the Codex device authorization service
	// and must be used for the final authorization-code exchange.
	DeviceRedirectURI = "https://auth.openai.com/deviceauth/callback"

	authScope    = "openid email profile offline_access"
	refreshScope = "openid profile email"
)

// Token is one account's stored credential set.
type Token struct {
	// ImportedDormant suppresses proactive rotation until first on-demand refresh.
	ImportedDormant bool      `json:"imported_dormant,omitempty"`
	AccessToken     string    `json:"access_token"`
	RefreshToken    string    `json:"refresh_token"`
	IDToken         string    `json:"id_token,omitempty"`
	AccountID       string    `json:"account_id,omitempty"` // chatgpt_account_id (Chatgpt-Account-Id header)
	Email           string    `json:"email,omitempty"`
	PlanType        string    `json:"plan_type,omitempty"` // chatgpt_plan_type from the id_token
	ExpiresAt       time.Time `json:"expires_at"`
	// ObtainedAt anchors the token's lifetime so proactive refresh can aim at
	// a fraction of it instead of guessing. Absent on records written by
	// older builds, in which case only the absolute lead applies.
	ObtainedAt time.Time `json:"obtained_at,omitempty"`
}

// Lifetime reports how long this token was minted for, or zero when the
// issuing time was not recorded.
func (t *Token) Lifetime() time.Duration {
	if t == nil || t.ObtainedAt.IsZero() || !t.ExpiresAt.After(t.ObtainedAt) {
		return 0
	}
	return t.ExpiresAt.Sub(t.ObtainedAt)
}

// PKCE is one login attempt's proof-key pair (RFC 7636).
type PKCE struct {
	Verifier  string
	Challenge string
}

// NewPKCE generates a 96-byte verifier (128 base64url chars) and its S256
// challenge — the same shape the Codex CLI uses.
func NewPKCE() (*PKCE, error) {
	raw := make([]byte, 96)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generate pkce verifier: %w", err)
	}
	v := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(v))
	return &PKCE{Verifier: v, Challenge: base64.RawURLEncoding.EncodeToString(sum[:])}, nil
}

// LoginURL builds the authorization URL for one PKCE challenge and state.
func LoginURL(state string, challenge string) string {
	q := url.Values{
		"client_id":                  {ClientID},
		"response_type":              {"code"},
		"redirect_uri":               {RedirectURI},
		"scope":                      {authScope},
		"state":                      {state},
		"code_challenge":             {challenge},
		"code_challenge_method":      {"S256"},
		"prompt":                     {"login"},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
	}
	return AuthURL + "?" + q.Encode()
}

// tokenResponse mirrors the token endpoint's JSON for both grant types.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int    `json:"expires_in"`
}

// TokenEndpointError retains the provider's safe OAuth error classification
// so refresh policy can distinguish a transient outage from an invalid or
// already-rotated refresh token. It never includes submitted token material.
type TokenEndpointError struct {
	Status      int
	Code        string
	Description string
}

func (e *TokenEndpointError) Error() string {
	if e == nil {
		return "oauth token endpoint error"
	}
	detail := strings.TrimSpace(e.Code)
	if detail == "" {
		detail = http.StatusText(e.Status)
	}
	if desc := strings.TrimSpace(e.Description); desc != "" && !strings.EqualFold(desc, detail) {
		detail += ": " + desc
	}
	return fmt.Sprintf("token endpoint status %d: %s", e.Status, detail)
}

// NonRetryable reports OAuth grant failures for which replaying the same
// refresh token cannot succeed and can make token rotation races worse.
func (e *TokenEndpointError) NonRetryable() bool {
	if e == nil {
		return false
	}
	code := strings.ToLower(strings.TrimSpace(e.Code + " " + e.Description))
	if strings.Contains(code, "refresh_token_reused") || strings.Contains(code, "invalid_grant") ||
		strings.Contains(code, "invalid refresh token") || strings.Contains(code, "revoked") {
		return true
	}
	return e.Status >= 400 && e.Status < 500 && e.Status != http.StatusRequestTimeout &&
		e.Status != http.StatusTooManyRequests
}

// postToken sends one form-encoded grant request to the token endpoint.
func postToken(ctx context.Context, hc *http.Client, form url.Values) (*Token, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token endpoint: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, fmt.Errorf("read token response: %w", err)
	}
	var tr tokenResponse
	if resp.StatusCode != http.StatusOK {
		var providerErr struct {
			Error            json.RawMessage `json:"error"`
			Code             string          `json:"code"`
			ErrorDescription string          `json:"error_description"`
			Message          string          `json:"message"`
		}
		_ = json.Unmarshal(body, &providerErr)
		code := ""
		description := providerErr.ErrorDescription
		if len(providerErr.Error) > 0 {
			if json.Unmarshal(providerErr.Error, &code) != nil {
				var nested struct {
					Type    string `json:"type"`
					Code    string `json:"code"`
					Message string `json:"message"`
				}
				if json.Unmarshal(providerErr.Error, &nested) == nil {
					code = firstNonEmptyTokenError(nested.Type, nested.Code)
					if description == "" {
						description = nested.Message
					}
				}
			}
		}
		if providerErr.Code != "" {
			if code != "" && !strings.EqualFold(code, providerErr.Code) {
				code += "/" + providerErr.Code
			} else {
				code = providerErr.Code
			}
		}
		if description == "" {
			description = providerErr.Message
		}
		// Never include an opaque provider body in the error. OAuth errors are
		// frequently logged by callers and an unexpected body could echo a
		// submitted authorization code, verifier, or rotating refresh token.
		for _, key := range []string{"code", "code_verifier", "refresh_token"} {
			if secret := strings.TrimSpace(form.Get(key)); secret != "" {
				code = strings.ReplaceAll(code, secret, "[REDACTED]")
				description = strings.ReplaceAll(description, secret, "[REDACTED]")
			}
		}
		return nil, &TokenEndpointError{Status: resp.StatusCode, Code: code, Description: description}
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	if tr.AccessToken == "" {
		return nil, fmt.Errorf("token response missing access_token")
	}
	now := time.Now()
	tok := &Token{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		IDToken:      tr.IDToken,
		ObtainedAt:   now,
		ExpiresAt:    now.Add(time.Duration(tr.ExpiresIn) * time.Second),
	}
	applyIDTokenClaims(tok)
	return tok, nil
}

func firstNonEmptyTokenError(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

// Exchange swaps an authorization code (plus its PKCE verifier) for tokens.
func Exchange(ctx context.Context, hc *http.Client, code, verifier string) (*Token, error) {
	return ExchangeWithRedirect(ctx, hc, code, verifier, RedirectURI)
}

// ExchangeWithRedirect swaps an authorization code using the redirect URI
// belonging to the flow that produced it (browser loopback or device flow).
func ExchangeWithRedirect(ctx context.Context, hc *http.Client, code, verifier, redirectURI string) (*Token, error) {
	if strings.TrimSpace(redirectURI) == "" {
		return nil, fmt.Errorf("redirect URI required")
	}
	return postToken(ctx, hc, url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {ClientID},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	})
}

// Refresh exchanges a refresh token for a fresh token set.
func Refresh(ctx context.Context, hc *http.Client, refreshToken string) (*Token, error) {
	if refreshToken == "" {
		return nil, fmt.Errorf("refresh token required")
	}
	return postToken(ctx, hc, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {ClientID},
		"refresh_token": {refreshToken},
		"scope":         {refreshScope},
	})
}

// ---------------------------------------------------------------------------
// id_token claim extraction (no signature check: the token came straight from
// the TLS-verified token endpoint; claims are used for display/routing only)

type idClaims struct {
	Email string `json:"email"`
	Auth  struct {
		ChatgptAccountID string `json:"chatgpt_account_id"`
		ChatgptPlanType  string `json:"chatgpt_plan_type"`
	} `json:"https://api.openai.com/auth"`
}

// applyIDTokenClaims fills display/routing fields from the id_token payload.
func applyIDTokenClaims(tok *Token) {
	if tok.IDToken == "" {
		return
	}
	parts := strings.Split(tok.IDToken, ".")
	if len(parts) < 2 {
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return
	}
	var c idClaims
	if err := json.Unmarshal(raw, &c); err != nil {
		return
	}
	tok.AccountID = c.Auth.ChatgptAccountID
	tok.PlanType = c.Auth.ChatgptPlanType
	if tok.Email == "" {
		tok.Email = c.Email
	}
}

// Valid reports whether the access token is still fresh past the given time.
// A five-minute safety margin keeps in-flight calls from expiring mid-request.
func (t *Token) Valid(now time.Time) bool {
	return t != nil && t.AccessToken != "" && t.RefreshToken != "" && now.Before(t.ExpiresAt.Add(-5*time.Minute))
}

// parsePositiveSeconds accepts provider interval/reset values encoded as a
// JSON number or quoted decimal. It is shared by the device flow.
func parsePositiveSeconds(raw json.RawMessage, fallback int) int {
	if len(raw) == 0 {
		return fallback
	}
	var number int
	if json.Unmarshal(raw, &number) == nil && number > 0 {
		return number
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if parsed, err := strconv.Atoi(strings.TrimSpace(text)); err == nil && parsed > 0 {
			return parsed
		}
	}
	return fallback
}
