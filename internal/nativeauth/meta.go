package nativeauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const metaClientID = "1031625952748946"
const metaDeviceURL = "https://auth.meta.com/oidc/device/authorization/"
const metaExchangeEndpoint = "https://auth.meta.com/oidc/device/token/"
const metaMintURL = "https://api.meta.ai/muse-code/key"
const metaBaseURL = "https://api.meta.ai/v1"

type metaDriver struct{}

func NewMetaDriver() Driver { return metaDriver{} }
func (metaDriver) Info() Info {
	return Info{ID: "meta", Name: "Meta", Mode: "device", BaseURL: metaBaseURL, Protocol: "openai-responses"}
}
func (metaDriver) Begin(ctx context.Context, c *http.Client, o LoginOptions) (Session, error) {
	var d xmDevice
	if err := xmForm(ctx, c, metaDeviceURL, url.Values{"client_id": {metaClientID}}, &d); err != nil {
		return Session{}, err
	}
	return xmSession(o, d, 900, "auth.meta.com", "accounts.meta.com", "www.meta.com", "meta.com")
}
func (m metaDriver) Complete(ctx context.Context, c *http.Client, s Session, _ string) (Token, error) {
	if err := xmCheckSession(s); err != nil {
		return Token{}, err
	}
	var p struct {
		Access    string `json:"access_token"`
		Type      string `json:"token_type"`
		Expires   int64  `json:"expires_in"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := xmForm(ctx, c, metaExchangeEndpoint, url.Values{"client_id": {metaClientID}, "grant_type": {xmDeviceGrant}, "device_code": {s.DeviceCode}}, &p); err != nil {
		return Token{}, err
	}
	if strings.TrimSpace(p.Access) == "" || (p.Type != "" && !strings.EqualFold(p.Type, "Bearer")) {
		return Token{}, errors.New("invalid Meta token response")
	}
	expiry, err := xmExpiry(p.Expires)
	if err != nil {
		return Token{}, err
	}
	if p.ExpiresAt < 0 || p.ExpiresAt > 253402300799 {
		return Token{}, errors.New("invalid Meta token lifetime")
	}
	if p.ExpiresAt > 0 && (expiry.IsZero() || time.Unix(p.ExpiresAt, 0).Before(expiry)) {
		expiry = time.Unix(p.ExpiresAt, 0)
	}
	// RefreshToken is the secret DCA minting credential, not an OAuth refresh token.
	t := Token{RefreshToken: p.Access, Extra: map[string]string{}}
	if !expiry.IsZero() {
		t.Extra["dca_expires_at"] = expiry.UTC().Format(time.RFC3339)
	}
	next, err := m.Refresh(ctx, c, t)
	if err != nil {
		return t, err
	} // Preserve the consumed device grant for a deliberate mint retry.
	return next, nil
}
func (metaDriver) Refresh(ctx context.Context, c *http.Client, t Token) (Token, error) {
	if strings.TrimSpace(t.RefreshToken) == "" {
		return Token{}, ErrLoginRequired
	}
	if raw := t.Extra["dca_expires_at"]; raw != "" {
		expiry, err := time.Parse(time.RFC3339, raw)
		if err != nil || !time.Now().Before(expiry) {
			return Token{}, ErrLoginRequired
		}
	}
	body, _ := json.Marshal(map[string]string{"dca_token": t.RefreshToken})
	var p struct {
		Key     string `json:"api_key"`
		BaseURL string `json:"base_url"`
		Email   string `json:"user_email"`
	}
	if err := xmJSON(ctx, c, metaMintURL, "application/json", string(body), t.RefreshToken, &p); err != nil {
		return Token{}, err
	}
	if strings.TrimSpace(p.Key) == "" || strings.HasPrefix(p.Key, "dca:") {
		return Token{}, errors.New("invalid Meta minted key")
	}
	if p.BaseURL != "" && p.BaseURL != metaBaseURL && p.BaseURL != metaBaseURL+"/" {
		return Token{}, errors.New("untrusted Meta inference base URL")
	}
	extra := map[string]string{"base_url": metaBaseURL}
	if expiry := t.Extra["dca_expires_at"]; expiry != "" {
		extra["dca_expires_at"] = expiry
	}
	// Minted keys have no advertised expiry; DCA expiry must not expire the key.
	return Token{AccessToken: p.Key, RefreshToken: t.RefreshToken, Identity: p.Email, Extra: extra}, nil
}
