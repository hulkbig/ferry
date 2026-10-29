package nativeauth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

const xaiClientID = "b1a00492-073a-47ea-816f-4c329264a828"
const xaiDiscoveryURL = "https://auth.x.ai/.well-known/openid-configuration"
const xaiScope = "openid profile email offline_access grok-cli:access api:access"

type xaiDriver struct{}

func NewXAIDriver() Driver { return xaiDriver{} }
func (xaiDriver) Info() Info {
	return Info{ID: "xai", Name: "xAI", Mode: "device", BaseURL: "https://api.x.ai/v1", Protocol: "openai-responses"}
}

type xaiDiscovery struct {
	Issuer string `json:"issuer"`
	Device string `json:"device_authorization_endpoint"`
	Token  string `json:"token_endpoint"`
}

func xaiDiscover(ctx context.Context, c *http.Client) (xaiDiscovery, error) {
	var d xaiDiscovery
	if err := xmJSON(ctx, c, xaiDiscoveryURL, "", "", "", &d); err != nil {
		return d, err
	}
	if (d.Issuer != "" && d.Issuer != "https://auth.x.ai") || !xmURL(d.Device, false, "auth.x.ai") || !xmURL(d.Token, false, "auth.x.ai") {
		return xaiDiscovery{}, errors.New("untrusted xAI discovery endpoints")
	}
	return d, nil
}
func (xaiDriver) Begin(ctx context.Context, c *http.Client, o LoginOptions) (Session, error) {
	discovery, err := xaiDiscover(ctx, c)
	if err != nil {
		return Session{}, err
	}
	var d xmDevice
	if err = xmForm(ctx, c, discovery.Device, url.Values{"client_id": {xaiClientID}, "scope": {xaiScope}}, &d); err != nil {
		return Session{}, err
	}
	s, err := xmSession(o, d, 1800, "auth.x.ai", "accounts.x.ai", "x.ai")
	if err == nil {
		s.Extra = map[string]string{"token_endpoint": discovery.Token}
	}
	return s, err
}
func (xaiDriver) Complete(ctx context.Context, c *http.Client, s Session, _ string) (Token, error) {
	if err := xmCheckSession(s); err != nil {
		return Token{}, err
	}
	return xaiExchange(ctx, c, s.Extra["token_endpoint"], url.Values{"client_id": {xaiClientID}, "grant_type": {xmDeviceGrant}, "device_code": {s.DeviceCode}})
}
func (xaiDriver) Refresh(ctx context.Context, c *http.Client, t Token) (Token, error) {
	if strings.TrimSpace(t.RefreshToken) == "" {
		return Token{}, ErrLoginRequired
	}
	endpoint := t.Extra["token_endpoint"]
	if endpoint == "" {
		d, err := xaiDiscover(ctx, c)
		if err != nil {
			return Token{}, err
		}
		endpoint = d.Token
	}
	next, err := xaiExchange(ctx, c, endpoint, url.Values{"client_id": {xaiClientID}, "grant_type": {"refresh_token"}, "refresh_token": {t.RefreshToken}})
	if err != nil {
		return Token{}, err
	}
	if next.RefreshToken == "" {
		next.RefreshToken = t.RefreshToken
	}
	next.Identity = t.Identity
	return next, nil
}
func xaiExchange(ctx context.Context, c *http.Client, endpoint string, form url.Values) (Token, error) {
	if !xmURL(endpoint, false, "auth.x.ai") {
		return Token{}, errors.New("untrusted xAI token endpoint")
	}
	var p struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		Type    string `json:"token_type"`
		Expires int64  `json:"expires_in"`
	}
	if err := xmForm(ctx, c, endpoint, form, &p); err != nil {
		return Token{}, err
	}
	if strings.TrimSpace(p.Access) == "" || (p.Type != "" && !strings.EqualFold(p.Type, "Bearer")) {
		return Token{}, errors.New("invalid xAI token response")
	}
	expiry, err := xmExpiry(p.Expires)
	if err != nil {
		return Token{}, err
	}
	// ID tokens are neither validated nor used as identity assertions.
	return Token{AccessToken: p.Access, RefreshToken: p.Refresh, ExpiresAt: expiry, Extra: map[string]string{"token_endpoint": endpoint}}, nil
}
