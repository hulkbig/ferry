package nativeauth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type agTestRT func(*http.Request) (*http.Response, error)

func (f agTestRT) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func agTestResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}
func agTestSession(t *testing.T) (Driver, Session) {
	t.Helper()
	d := NewAntigravityDriver()
	s, err := d.Begin(context.Background(), nil, LoginOptions{State: "bound-state", Verifier: strings.Repeat("v", 43), Fields: map[string]string{"client_id": "operator-client", "client_secret": "operator-secret"}})
	if err != nil {
		t.Fatal(err)
	}
	return d, s
}
func TestAntigravityPKCEProjectAndRefresh(t *testing.T) {
	d, s := agTestSession(t)
	u, _ := url.Parse(s.URL)
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("state") != s.State || q.Get("redirect_uri") != agRedirect || strings.Contains(s.URL, "operator-secret") || strings.Contains(s.URL, s.Verifier) {
		t.Fatal("unsafe authorization URL")
	}
	calls := 0
	c := &http.Client{Transport: agTestRT(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("User-Agent") != "Ferry/native-antigravity" {
			t.Fatal("impersonation")
		}
		switch r.URL.String() {
		case agExchangeEndpoint:
			b, _ := io.ReadAll(r.Body)
			v, _ := url.ParseQuery(string(b))
			if v.Get("client_id") != "operator-client" || v.Get("client_secret") != "operator-secret" {
				t.Fatal("client binding missing")
			}
			if v.Get("grant_type") == "authorization_code" {
				if v.Get("code_verifier") != s.Verifier || v.Get("code") != "bound-code" || v.Get("redirect_uri") != agRedirect {
					t.Fatal("exchange binding missing")
				}
				return agTestResponse(200, `{"access_token":"access","refresh_token":"refresh","expires_in":3600,"token_type":"Bearer"}`), nil
			}
			if v.Get("refresh_token") != "refresh" {
				t.Fatal("refresh not bound")
			}
			return agTestResponse(200, `{"access_token":"next","expires_in":1800,"token_type":"Bearer"}`), nil
		case agRoot + "/v1internal:loadCodeAssist":
			if r.Header.Get("Authorization") != "Bearer access" {
				t.Fatal("account token missing")
			}
			return agTestResponse(200, `{"cloudaicompanionProject":{"id":"project-123"}}`), nil
		default:
			t.Fatalf("unexpected endpoint %s", r.URL)
			return nil, nil
		}
	})}
	token, err := d.Complete(context.Background(), c, s, agRedirect+"?code=bound-code&state=bound-state")
	if err != nil {
		t.Fatal(err)
	}
	if token.Extra["project_id"] != "project-123" || time.Until(token.ExpiresAt) < 59*time.Minute {
		t.Fatal("project/expiry missing")
	}
	refreshed, err := d.Refresh(context.Background(), c, token)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.AccessToken != "next" || refreshed.RefreshToken != "refresh" || refreshed.Extra["project_id"] != "project-123" || calls != 3 {
		t.Fatal("refresh lost binding")
	}
	refreshed.Extra["project_id"] = "other"
	if token.Extra["project_id"] != "project-123" {
		t.Fatal("refresh mutated old metadata")
	}
}
func TestAntigravityInvalidCallbackNeverExchanges(t *testing.T) {
	d, s := agTestSession(t)
	c := &http.Client{Transport: agTestRT(func(*http.Request) (*http.Response, error) { t.Fatal("invalid callback made request"); return nil, nil })}
	for _, cb := range []string{"bare-code", agRedirect + "?code=x&state=wrong", agRedirect + "?code=x&state=bound-state&state=bound-state", agRedirect + "?code=x&state=bound-state&error=denied", "http://evil.test/oauth-callback?code=x&state=bound-state", agRedirect + "?code=x&state=bound-state#fragment", "http://127.0.0.1:51121/%6fauth-callback?code=x&state=bound-state"} {
		if _, err := d.Complete(context.Background(), c, s, cb); err == nil {
			t.Fatalf("accepted %s", cb)
		}
	}
	s.ExpiresAt = time.Now().Add(-time.Second)
	if _, err := d.Complete(context.Background(), c, s, agRedirect+"?code=x&state=bound-state"); err == nil {
		t.Fatal("expired accepted")
	}
	if _, err := d.Begin(context.Background(), nil, LoginOptions{State: "state", Verifier: strings.Repeat("v", 43)}); err == nil {
		t.Fatal("missing operator credentials accepted")
	}
}
func TestAntigravityEntitlementOnboarding(t *testing.T) {
	for _, tc := range []struct {
		name, load, onboard string
		wantErr             bool
	}{
		{"onboard", `{"allowedTiers":[{"id":"free-tier","isDefault":true}]}`, `{"done":true,"response":{"cloudaicompanionProject":{"id":"assigned-project"}}}`, false},
		{"no-entitlement", `{"allowedTiers":[]}`, "", true},
		{"operation-error", `{"allowedTiers":[{"id":"free-tier","isDefault":true}]}`, `{"error":{"message":"private-secret"}}`, true},
		{"bad-project", `{"cloudaicompanionProject":"../other"}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &http.Client{Transport: agTestRT(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, ":loadCodeAssist") {
					return agTestResponse(200, tc.load), nil
				}
				if !strings.HasSuffix(r.URL.Path, ":onboardUser") {
					t.Fatal("unexpected endpoint")
				}
				var body map[string]any
				json.NewDecoder(r.Body).Decode(&body)
				if body["tier_id"] != "free-tier" {
					t.Fatal("default tier not used")
				}
				return agTestResponse(200, tc.onboard), nil
			})}
			project, err := agProject(context.Background(), c, "token")
			if (err != nil) != tc.wantErr {
				t.Fatalf("project=%s err=%v", project, err)
			}
			if err != nil && strings.Contains(err.Error(), "private-secret") {
				t.Fatal("raw upstream error leaked")
			}
			if !tc.wantErr && project != "assigned-project" {
				t.Fatal(project)
			}
		})
	}
}
func TestAntigravityRedirectAndTokenValidation(t *testing.T) {
	d, s := agTestSession(t)
	for _, tc := range []struct {
		status int
		body   string
	}{
		{302, `private-secret`}, {403, `private-secret`}, {200, `{"access_token":"access","expires_in":3600,"token_type":"Bearer"}`}, {200, `{"access_token":"access","refresh_token":"refresh","expires_in":-1,"token_type":"Bearer"}`},
	} {
		calls := 0
		c := &http.Client{Transport: agTestRT(func(*http.Request) (*http.Response, error) {
			calls++
			r := agTestResponse(tc.status, tc.body)
			r.Header.Set("Location", "https://evil.test")
			return r, nil
		})}
		_, err := d.Complete(context.Background(), c, s, agRedirect+"?code=x&state=bound-state")
		if err == nil || strings.Contains(err.Error(), "private-secret") || calls != 1 {
			t.Fatalf("unsafe error/redirect: %v calls=%d", err, calls)
		}
	}
}

func TestAntigravityOnboardingCancellationAndRefreshRotation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	c := &http.Client{Transport: agTestRT(func(r *http.Request) (*http.Response, error) {
		calls++
		if strings.HasSuffix(r.URL.Path, ":loadCodeAssist") {
			return agTestResponse(200, `{"allowedTiers":[{"id":"free-tier","isDefault":true}]}`), nil
		}
		cancel()
		return agTestResponse(200, `{"done":false}`), nil
	})}
	if _, err := agProject(ctx, c, "access"); err == nil || calls != 2 {
		t.Fatalf("cancellation err=%v calls=%d", err, calls)
	}
	d := NewAntigravityDriver()
	old := Token{RefreshToken: "old-refresh", Extra: map[string]string{"client_id": "operator-client", "client_secret": "operator-secret", "project_id": "project-one"}}
	c = &http.Client{Transport: agTestRT(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != agExchangeEndpoint {
			t.Fatal("refresh tried onboarding")
		}
		return agTestResponse(200, `{"access_token":"new-access","refresh_token":"rotated-refresh","expires_in":3600,"token_type":"Bearer"}`), nil
	})}
	next, err := d.Refresh(context.Background(), c, old)
	if err != nil || next.RefreshToken != "rotated-refresh" || next.Extra["project_id"] != "project-one" {
		t.Fatalf("rotation failed: %v", err)
	}
}
