package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ferry/internal/domain"
)

func TestNewPKCEShape(t *testing.T) {
	p, err := NewPKCE()
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Verifier) != 128 {
		t.Fatalf("verifier length = %d, want 128", len(p.Verifier))
	}
	if strings.ContainsAny(p.Verifier, "+/= \n") || strings.ContainsAny(p.Challenge, "+/= \n") {
		t.Fatal("pkce values must be base64url without padding")
	}
	if p.Verifier == p.Challenge {
		t.Fatal("challenge must not equal the verifier")
	}
}

func TestLoginURLParams(t *testing.T) {
	u := LoginURL("state-xyz", "chal-abc")
	for _, want := range []string{
		"client_id=" + ClientID,
		"response_type=code",
		"code_challenge=chal-abc",
		"code_challenge_method=S256",
		"state=state-xyz",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("LoginURL missing %s in %s", want, u)
		}
	}
}

// fakeTokenServer stands in for auth.openai.com and records the form fields.
type fakeTokenServer struct {
	srv    *httptest.Server
	form   map[string]string
	status int
	resp   string
}

func newFakeTokenServer(t *testing.T, status int, respBody string) (*fakeTokenServer, func()) {
	f := &fakeTokenServer{status: status, resp: respBody, form: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		for k, v := range r.Form {
			f.form[k] = v[0]
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	})
	f.srv = httptest.NewServer(mux)
	return f, f.srv.Close
}

const testIDToken = "eyJhbGci" + "OiJub25l" + "In0" +
	"." +
	"eyJlbWFpbCI6InVzZXJAeC5jb20iLCJodHRwczovL2FwaS5vcGVuYWkuY29tL2F1dGgiOnsiY2hhdGdwdF9hY2NvdW50X2lkIjoiYWNjdC05IiwiY2hhdGdwdF9wbGFuX3R5cGUiOiJwcm8ifX0" +
	"."

const tokenRespJSON = `{
  "access_token": "at-123",
  "refresh_token": "rt-456",
  "id_token": "` + testIDToken + `",
  "token_type": "bearer",
  "expires_in": 3600
}`

// The id_token above decodes to claims:
// {"email":"user@x.com","https://api.openai.com/auth":{"chatgpt_account_id":"acct-9","chatgpt_plan_type":"pro"}}

func TestExchangeAndClaims(t *testing.T) {
	f, done := newFakeTokenServer(t, http.StatusOK, tokenRespJSON)
	defer done()
	orig := tokenURL
	tokenURL = f.srv.URL + "/oauth/token"
	defer func() { tokenURL = orig }()

	tok, err := Exchange(context.Background(), f.srv.Client(), "the-code", "the-verifier")
	if err != nil {
		t.Fatal(err)
	}
	if f.form["grant_type"] != "authorization_code" || f.form["code"] != "the-code" ||
		f.form["code_verifier"] != "the-verifier" || f.form["redirect_uri"] != RedirectURI ||
		f.form["client_id"] != ClientID {
		t.Fatalf("unexpected exchange form: %v", f.form)
	}
	if tok.AccessToken != "at-123" || tok.RefreshToken != "rt-456" {
		t.Fatalf("token = %+v", tok)
	}
	if tok.AccountID != "acct-9" || tok.PlanType != "pro" || tok.Email != "user@x.com" {
		t.Fatalf("claims not extracted: %+v", tok)
	}
}

func TestRefreshForm(t *testing.T) {
	f, done := newFakeTokenServer(t, http.StatusOK, tokenRespJSON)
	defer done()
	orig := tokenURL
	tokenURL = f.srv.URL + "/oauth/token"
	defer func() { tokenURL = orig }()

	tok, err := Refresh(context.Background(), f.srv.Client(), "rt-old")
	if err != nil {
		t.Fatal(err)
	}
	if f.form["grant_type"] != "refresh_token" || f.form["refresh_token"] != "rt-old" ||
		f.form["scope"] != refreshScope {
		t.Fatalf("unexpected refresh form: %v", f.form)
	}
	if tok.ExpiresAt.Before(time.Now()) {
		t.Fatal("expires_at should be ~now+expires_in")
	}
}

func TestRefreshErrorSurfaces(t *testing.T) {
	f, done := newFakeTokenServer(t, http.StatusBadRequest, `{"error":"invalid_grant","code":"refresh_token_reused"}`)
	defer done()
	orig := tokenURL
	tokenURL = f.srv.URL + "/oauth/token"
	defer func() { tokenURL = orig }()

	_, err := Refresh(context.Background(), f.srv.Client(), "rt-reused")
	if err == nil {
		t.Fatal("want error on non-200 token response")
	}
	var endpointErr *TokenEndpointError
	if !errors.As(err, &endpointErr) || !endpointErr.NonRetryable() ||
		!strings.Contains(endpointErr.Error(), "refresh_token_reused") {
		t.Fatalf("error=%T %v", err, err)
	}
}

func TestTokenEndpointErrorRedactsEchoedRefreshToken(t *testing.T) {
	const secret = "rt-sensitive-value"
	f, done := newFakeTokenServer(t, http.StatusBadRequest,
		`{"error":"invalid_grant","error_description":"refresh token rt-sensitive-value rejected"}`)
	defer done()
	orig := tokenURL
	tokenURL = f.srv.URL + "/oauth/token"
	defer func() { tokenURL = orig }()
	_, err := Refresh(context.Background(), f.srv.Client(), secret)
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("unsafe token endpoint error: %v", err)
	}
}

func TestExchangeWithDeviceRedirect(t *testing.T) {
	f, done := newFakeTokenServer(t, http.StatusOK, tokenRespJSON)
	defer done()
	orig := tokenURL
	tokenURL = f.srv.URL + "/oauth/token"
	defer func() { tokenURL = orig }()
	if _, err := ExchangeWithRedirect(context.Background(), f.srv.Client(), "device-code", "verifier", DeviceRedirectURI); err != nil {
		t.Fatal(err)
	}
	if got := f.form["redirect_uri"]; got != DeviceRedirectURI {
		t.Fatalf("redirect_uri=%q", got)
	}
}

func TestTokenValidWindow(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		tok  *Token
		want bool
	}{
		{"fresh", &Token{AccessToken: "a", RefreshToken: "r", ExpiresAt: now.Add(10 * time.Minute)}, true},
		{"inside-margin", &Token{AccessToken: "a", RefreshToken: "r", ExpiresAt: now.Add(2 * time.Minute)}, false},
		{"expired", &Token{AccessToken: "a", RefreshToken: "r", ExpiresAt: now.Add(-time.Minute)}, false},
		{"no-refresh", &Token{AccessToken: "a", ExpiresAt: now.Add(time.Hour)}, false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := c.tok.Valid(now); got != c.want {
			t.Errorf("%s: Valid = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRefRoundTripViaDomain(t *testing.T) {
	acct, ok := domain.OAuthAccountFromRef(domain.OAuthRef("work"))
	if !ok || acct != "work" {
		t.Fatalf("domain ref round trip failed: %q %v", acct, ok)
	}
	st, err := json.Marshal(domain.OAuthAccountStatus{Name: "work"})
	if err != nil || !strings.Contains(string(st), `"name":"work"`) {
		t.Fatalf("status marshal = %s, %v", st, err)
	}
}
