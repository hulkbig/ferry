package nativeauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Protocol reference: CLIProxyAPI d946c4f; see the integration notes for provenance.
const claudeExchangeEndpoint = "https://platform.claude.com/v1/oauth/token"
const claudeClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
const claudeRedirectURI = "http://localhost:54545/callback"
const claudeScope = "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"

type claudeDriver struct{}

func NewClaudeDriver() Driver { return &claudeDriver{} }
func (*claudeDriver) Info() Info {
	return Info{ID: "claude", Name: "Claude", Mode: "browser", BaseURL: "https://api.anthropic.com/v1", Protocol: "anthropic"}
}

func claudeVerifier(v string) bool {
	if len(v) < 43 || len(v) > 128 {
		return false
	}
	for _, r := range v {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-._~", r)) {
			return false
		}
	}
	return true
}

func (*claudeDriver) Begin(ctx context.Context, _ *http.Client, opts LoginOptions) (Session, error) {
	if ctx == nil || !ckText(opts.State) || !claudeVerifier(opts.Verifier) {
		return Session{}, ckInvalid
	}
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}
	challenge := sha256.Sum256([]byte(opts.Verifier))
	q := url.Values{"code": {"true"}, "client_id": {claudeClientID}, "response_type": {"code"}, "redirect_uri": {claudeRedirectURI}, "scope": {claudeScope}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"}, "state": {opts.State}}
	return Session{State: opts.State, Verifier: opts.Verifier, URL: "https://claude.ai/oauth/authorize?" + q.Encode(), ExpiresAt: time.Now().Add(15 * time.Minute)}, nil
}

// Accept the fixed callback URL, its query, or the provider's code#state form.
// A bare code cannot prove callback state and is deliberately rejected.
func claudeCallback(raw, expected string) (string, error) {
	if len(raw) > 32768 || !ckText(expected) {
		return "", ckInvalid
	}
	var code, state string
	raw = strings.TrimSpace(raw)
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "http" || u.Host != "localhost:54545" || u.Path != "/callback" || u.User != nil || u.Fragment != "" {
			return "", ckInvalid
		}
		raw = u.RawQuery
	}
	if strings.Contains(raw, "#") && !strings.Contains(raw, "&") && !strings.Contains(raw, "=") {
		parts := strings.Split(raw, "#")
		if len(parts) != 2 {
			return "", ckInvalid
		}
		code, state = parts[0], parts[1]
	} else {
		q, err := url.ParseQuery(strings.TrimPrefix(raw, "?"))
		if err != nil || len(q["code"]) != 1 || len(q["state"]) != 1 || q.Has("error") {
			return "", ckInvalid
		}
		code, state = q.Get("code"), q.Get("state")
	}
	if !ckText(code) || subtle.ConstantTimeCompare([]byte(state), []byte(expected)) != 1 {
		return "", ckInvalid
	}
	return code, nil
}

func (d *claudeDriver) Complete(ctx context.Context, c *http.Client, s Session, callback string) (Token, error) {
	if !s.ExpiresAt.After(time.Now()) || !claudeVerifier(s.Verifier) {
		return Token{}, ErrLoginRequired
	}
	code, err := claudeCallback(callback, s.State)
	if err != nil {
		return Token{}, err
	}
	return d.exchange(ctx, c, map[string]string{"grant_type": "authorization_code", "code": code, "redirect_uri": claudeRedirectURI, "client_id": claudeClientID, "code_verifier": s.Verifier, "state": s.State}, Token{})
}

func (d *claudeDriver) Refresh(ctx context.Context, c *http.Client, old Token) (Token, error) {
	if !ckText(old.RefreshToken) {
		return Token{}, ErrLoginRequired
	}
	return d.exchange(ctx, c, map[string]string{"grant_type": "refresh_token", "refresh_token": old.RefreshToken, "client_id": claudeClientID, "scope": claudeScope}, old)
}

func (*claudeDriver) exchange(ctx context.Context, c *http.Client, fields map[string]string, old Token) (Token, error) {
	body, _ := json.Marshal(fields)
	var r struct {
		AccessToken  string  `json:"access_token"`
		RefreshToken string  `json:"refresh_token"`
		ExpiresIn    float64 `json:"expires_in"`
		TokenType    string  `json:"token_type"`
		Account      struct {
			UUID  string `json:"uuid"`
			Email string `json:"email_address"`
		} `json:"account"`
		Organization struct {
			UUID string `json:"uuid"`
		} `json:"organization"`
	}
	if err := ckPost(ctx, c, claudeExchangeEndpoint, "application/json", string(body), nil, &r); err != nil {
		return Token{}, err
	}
	if !ckText(r.AccessToken) || r.RefreshToken != "" && !ckText(r.RefreshToken) || r.TokenType != "" && !strings.EqualFold(r.TokenType, "bearer") {
		return Token{}, ckInvalid
	}
	expires, err := ckExpiry(r.ExpiresIn)
	if err != nil {
		return Token{}, err
	}
	t := Token{AccessToken: r.AccessToken, RefreshToken: r.RefreshToken, ExpiresAt: expires, Identity: old.Identity, Extra: ckCopyExtra(old.Extra)}
	if t.RefreshToken == "" {
		t.RefreshToken = old.RefreshToken
	}
	if r.Account.Email != "" {
		t.Identity = r.Account.Email
	} else if r.Account.UUID != "" {
		t.Identity = r.Account.UUID
	}
	if r.Account.UUID != "" {
		t.Extra["account_uuid"] = r.Account.UUID
	}
	if r.Organization.UUID != "" {
		t.Extra["organization_uuid"] = r.Organization.UUID
	}
	t.Extra["token_type"] = "Bearer"
	return t, nil
}
