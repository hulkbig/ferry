package nativeauth

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"time"
)

const kimiClientID = "17e5f671-d194-4dfb-9706-5516cb48c098"

type kimiDriver struct{ international bool }

func NewKimiDriver(international bool) Driver { return &kimiDriver{international: international} }
func (d *kimiDriver) domain() string {
	if d.international {
		return "kimi.ai"
	}
	return "kimi.com"
}
func (d *kimiDriver) Info() Info {
	id, name := "kimi", "Kimi"
	if d.international {
		id, name = "kimi-ai", "Kimi (international)"
	}
	return Info{ID: id, Name: name, Mode: "device", BaseURL: "https://api." + d.domain() + "/coding/v1", Protocol: "openai-chat"}
}
func (d *kimiDriver) endpoint(path string) string {
	return "https://auth." + d.domain() + "/api/oauth/" + path
}
func kimiHeaders(device string) map[string]string {
	return map[string]string{"X-Msh-Platform": "Ferry", "X-Msh-Version": "nativeauth", "X-Msh-Device-Name": "Ferry", "X-Msh-Device-Model": runtime.GOOS + " " + runtime.GOARCH, "X-Msh-Device-Id": device}
}
func (d *kimiDriver) validExtra(extra map[string]string) bool {
	return extra["domain"] == d.domain() && ckText(extra["device_id"])
}
func (d *kimiDriver) Begin(ctx context.Context, c *http.Client, opts LoginOptions) (Session, error) {
	if !ckText(opts.State) {
		return Session{}, ckInvalid
	}
	device := opts.Fields["device_id"]
	if device == "" {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return Session{}, ckRequestFailed
		}
		b[6] = b[6]&15 | 64
		b[8] = b[8]&63 | 128
		device = fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
	}
	if !ckText(device) || len(device) > 128 {
		return Session{}, ckInvalid
	}
	var r struct {
		DeviceCode  string `json:"device_code"`
		UserCode    string `json:"user_code"`
		URI         string `json:"verification_uri"`
		CompleteURI string `json:"verification_uri_complete"`
		ExpiresIn   int    `json:"expires_in"`
		Interval    int    `json:"interval"`
	}
	if err := ckPost(ctx, c, d.endpoint("device_authorization"), "application/x-www-form-urlencoded", url.Values{"client_id": {kimiClientID}}.Encode(), kimiHeaders(device), &r); err != nil {
		return Session{}, err
	}
	if !ckText(r.DeviceCode) || !ckText(r.UserCode) || r.ExpiresIn <= 0 || r.Interval < 0 || r.Interval > 900 {
		return Session{}, ckInvalid
	}
	link := r.CompleteURI
	if link == "" {
		link = r.URI
	}
	u, err := url.Parse(link)
	if err != nil || len(link) > 16384 || u.Scheme != "https" || u.User != nil || (u.Host != d.domain() && u.Host != "www."+d.domain() && u.Host != "auth."+d.domain()) || u.Fragment != "" {
		return Session{}, ckInvalid
	}
	if r.ExpiresIn > 900 {
		r.ExpiresIn = 900
	}
	if r.Interval < 5 {
		r.Interval = 5
	}
	return Session{State: opts.State, Verifier: opts.Verifier, DeviceCode: r.DeviceCode, UserCode: r.UserCode, URL: link, ExpiresAt: time.Now().Add(time.Duration(r.ExpiresIn) * time.Second), IntervalSeconds: r.Interval, Extra: map[string]string{"domain": d.domain(), "device_id": device}}, nil
}
func (d *kimiDriver) Complete(ctx context.Context, c *http.Client, s Session, _ string) (Token, error) {
	if !s.ExpiresAt.After(time.Now()) {
		return Token{}, ErrLoginRequired
	}
	if !ckText(s.DeviceCode) || !ckText(s.State) || !d.validExtra(s.Extra) {
		return Token{}, ckInvalid
	}
	return d.exchange(ctx, c, url.Values{"client_id": {kimiClientID}, "grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {s.DeviceCode}}, Token{Extra: s.Extra})
}
func (d *kimiDriver) Refresh(ctx context.Context, c *http.Client, old Token) (Token, error) {
	if !ckText(old.RefreshToken) {
		return Token{}, ErrLoginRequired
	}
	if !d.validExtra(old.Extra) {
		return Token{}, ckInvalid
	}
	return d.exchange(ctx, c, url.Values{"client_id": {kimiClientID}, "grant_type": {"refresh_token"}, "refresh_token": {old.RefreshToken}}, old)
}
func (d *kimiDriver) exchange(ctx context.Context, c *http.Client, form url.Values, old Token) (Token, error) {
	var r struct {
		AccessToken  string  `json:"access_token"`
		RefreshToken string  `json:"refresh_token"`
		TokenType    string  `json:"token_type"`
		ExpiresIn    float64 `json:"expires_in"`
		Scope        string  `json:"scope"`
	}
	if err := ckPost(ctx, c, d.endpoint("token"), "application/x-www-form-urlencoded", form.Encode(), kimiHeaders(old.Extra["device_id"]), &r); err != nil {
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
	t.Extra["token_type"] = "Bearer"
	if r.Scope != "" {
		t.Extra["scope"] = r.Scope
	}
	return t, nil
}
