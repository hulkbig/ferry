package nativeauth

// Independently implemented from observed CLIProxyAPI d946c4f wire schemas.
// No embedded product credentials or client impersonation. See docs/reference/native/native-antigravity-notes.md.
import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const agRoot = "https://daily-cloudcode-pa.googleapis.com"
const agExchangeEndpoint = "https://oauth2.googleapis.com/token"
const agRedirect = "http://127.0.0.1:51121/oauth-callback"

var agIdentifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)
var agInvalid = errors.New("invalid Antigravity authorization; matching authorized OAuth client and account entitlement required")

type antigravityDriver struct{}

func NewAntigravityDriver() Driver { return &antigravityDriver{} }
func (*antigravityDriver) Info() Info {
	return Info{ID: "antigravity", Name: "Antigravity", Mode: "browser", BaseURL: agRoot, Protocol: "openai-chat"}
}
func agText(s string) bool {
	if s == "" || len(s) > 8192 {
		return false
	}
	for _, r := range s {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}
func agVerifier(s string) bool {
	if len(s) < 43 || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-._~", r)) {
			return false
		}
	}
	return true
}
func (*antigravityDriver) Begin(ctx context.Context, _ *http.Client, o LoginOptions) (Session, error) {
	if ctx == nil || !agText(o.State) || !agVerifier(o.Verifier) {
		return Session{}, agInvalid
	}
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}
	for k := range o.Fields {
		if k != "client_id" && k != "client_secret" {
			return Session{}, agInvalid
		}
	}
	if !agText(o.Fields["client_id"]) || !agText(o.Fields["client_secret"]) {
		return Session{}, agInvalid
	}
	h := sha256.Sum256([]byte(o.Verifier))
	scopes := []string{"cloud-platform", "userinfo.email", "userinfo.profile", "cclog", "experimentsandconfigs"}
	for i := range scopes {
		scopes[i] = "https://www.googleapis.com/auth/" + scopes[i]
	}
	q := url.Values{"client_id": {o.Fields["client_id"]}, "redirect_uri": {agRedirect}, "response_type": {"code"}, "scope": {strings.Join(scopes, " ")}, "access_type": {"offline"}, "prompt": {"consent"}, "state": {o.State}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(h[:])}}
	return Session{State: o.State, Verifier: o.Verifier, URL: "https://accounts.google.com/o/oauth2/v2/auth?" + q.Encode(), ExpiresAt: time.Now().Add(15 * time.Minute), Extra: map[string]string{"client_id": o.Fields["client_id"], "client_secret": o.Fields["client_secret"]}}, nil
}
func (*antigravityDriver) Complete(ctx context.Context, c *http.Client, s Session, callback string) (Token, error) {
	if !s.ExpiresAt.After(time.Now()) || !agVerifier(s.Verifier) || !agText(s.State) || len(callback) > 32768 {
		return Token{}, agInvalid
	}
	u, err := url.Parse(callback)
	if err != nil || u.Scheme != "http" || u.Host != "127.0.0.1:51121" || u.Path != "/oauth-callback" || u.RawPath != "" || u.User != nil || u.Fragment != "" {
		return Token{}, agInvalid
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(q["state"]) != 1 || len(q["code"]) != 1 || q.Has("error") || !agText(q.Get("code")) || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(s.State)) != 1 {
		return Token{}, agInvalid
	}
	t, err := agExchange(ctx, c, s.Extra, url.Values{"grant_type": {"authorization_code"}, "code": {q.Get("code")}, "redirect_uri": {agRedirect}, "code_verifier": {s.Verifier}}, Token{})
	if err != nil {
		return Token{}, err
	}
	project, err := agProject(ctx, c, t.AccessToken)
	if err != nil {
		return Token{}, err
	}
	t.Extra["project_id"] = project
	return t, nil
}
func (*antigravityDriver) Refresh(ctx context.Context, c *http.Client, old Token) (Token, error) {
	if !agText(old.RefreshToken) || !agIdentifier.MatchString(old.Extra["project_id"]) {
		return Token{}, ErrLoginRequired
	}
	return agExchange(ctx, c, old.Extra, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {old.RefreshToken}}, old)
}
func agExchange(ctx context.Context, c *http.Client, extra map[string]string, form url.Values, old Token) (Token, error) {
	if !agText(extra["client_id"]) || !agText(extra["client_secret"]) {
		return Token{}, agInvalid
	}
	form.Set("client_id", extra["client_id"])
	form.Set("client_secret", extra["client_secret"])
	var v struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		Expires int64  `json:"expires_in"`
		Type    string `json:"token_type"`
	}
	if err := agHTTP(ctx, c, agExchangeEndpoint, "application/x-www-form-urlencoded", form.Encode(), "", &v); err != nil {
		return Token{}, err
	}
	if !agText(v.Access) || v.Expires <= 0 || v.Expires > 86400*30 || !strings.EqualFold(v.Type, "Bearer") {
		return Token{}, agInvalid
	}
	if v.Refresh == "" {
		v.Refresh = old.RefreshToken
	}
	if !agText(v.Refresh) {
		return Token{}, agInvalid
	}
	t := Token{AccessToken: v.Access, RefreshToken: v.Refresh, ExpiresAt: time.Now().Add(time.Duration(v.Expires) * time.Second), Identity: old.Identity, Extra: map[string]string{}}
	for k, v := range extra {
		t.Extra[k] = v
	}
	return t, nil
}
func agHTTP(ctx context.Context, c *http.Client, endpoint, contentType, body, token string, out any) error {
	if ctx == nil || c == nil {
		return agInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(body))
	if err != nil {
		return agInvalid
	}
	r.Header.Set("Content-Type", contentType)
	r.Header.Set("User-Agent", "Ferry/native-antigravity")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	safe := *c
	safe.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := safe.Do(r)
	if err != nil {
		return errors.New("Antigravity authorization transport failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Antigravity authorization endpoint returned HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(b) > 1<<20 {
		return agInvalid
	}
	if json.Unmarshal(b, out) != nil {
		return agInvalid
	}
	return nil
}
func agProjectValue(v map[string]json.RawMessage) string {
	for _, key := range []string{"cloudaicompanionProject", "projectId", "project"} {
		var s string
		if json.Unmarshal(v[key], &s) != nil {
			var obj struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(v[key], &obj)
			s = obj.ID
		}
		if agIdentifier.MatchString(s) {
			return s
		}
	}
	return ""
}
func agProject(ctx context.Context, c *http.Client, token string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var load map[string]json.RawMessage
	if err := agHTTP(ctx, c, agRoot+"/v1internal:loadCodeAssist", "application/json", `{"metadata":{"ideType":"ANTIGRAVITY"}}`, token, &load); err != nil {
		return "", err
	}
	if p := agProjectValue(load); p != "" {
		return p, nil
	}
	var tiers []struct {
		ID      string `json:"id"`
		Default bool   `json:"isDefault"`
	}
	_ = json.Unmarshal(load["allowedTiers"], &tiers)
	tier := ""
	for _, v := range tiers {
		if v.Default && agIdentifier.MatchString(v.ID) {
			tier = v.ID
			break
		}
	}
	if tier == "" {
		return "", errors.New("Antigravity account has no project or default entitled tier")
	}
	b, _ := json.Marshal(map[string]any{"tier_id": tier, "metadata": map[string]string{"ide_type": "ANTIGRAVITY"}})
	for i := 0; i < 5; i++ {
		var op struct {
			Done     bool                       `json:"done"`
			Response map[string]json.RawMessage `json:"response"`
			Error    json.RawMessage            `json:"error"`
		}
		if err := agHTTP(ctx, c, agRoot+"/v1internal:onboardUser", "application/json", string(b), token, &op); err != nil {
			return "", err
		}
		if len(op.Error) > 0 && string(op.Error) != "null" {
			return "", errors.New("Antigravity onboarding failed")
		}
		if op.Done {
			if p := agProjectValue(op.Response); p != "" {
				return p, nil
			}
			return "", agInvalid
		}
		if i < 4 {
			timer := time.NewTimer(2 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return "", ctx.Err()
			case <-timer.C:
			}
		}
	}
	return "", errors.New("Antigravity onboarding did not complete; retry login after checking account entitlement")
}
