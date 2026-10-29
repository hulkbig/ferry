package management

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ferry/internal/config"
	"ferry/internal/controlstore"
	"ferry/internal/domain"
	"ferry/internal/oauth"
	"ferry/internal/secretstore"
)

type resetAPIRoundTrip func(*http.Request) (*http.Response, error)

func (f resetAPIRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestUsageResetAPISessionBoundaryAndExactTarget(t *testing.T) {
	security, err := controlstore.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer security.Close()
	secrets, err := secretstore.OpenSQLite(filepath.Join(t.TempDir(), "secrets.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer secrets.Close()
	raw, _ := json.Marshal(oauth.Token{AccessToken: "offline-test-token", RefreshToken: "offline-refresh-token", AccountID: "account-a", ExpiresAt: time.Now().Add(time.Hour)})
	if err := secrets.Set("oauth:codex:local-a", string(raw)); err != nil {
		t.Fatal(err)
	}
	// All outbound HTTP is intercepted; unexpected requests fail without dialing.
	oldTransport := http.DefaultTransport
	oldUsage := oauth.CurrentUsageURL()
	posts := 0
	http.DefaultTransport = resetAPIRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "reset-api-fixture.invalid" || r.Header.Get("ChatGPT-Account-Id") != "account-a" || r.Header.Get("Authorization") != "Bearer offline-test-token" {
			return nil, errors.New("unexpected destination/identity; network disabled")
		}
		var body string
		switch r.Method + " " + r.URL.Path {
		case "GET /usage":
			body = `{"account_id":"account-a","rate_limit":{"primary_window":{"limit_window_seconds":18000,"used_percent":95}}}`
		case "GET /rate-limit-reset-credits":
			body = `{"available_count":1,"credits":[{"id":"credit-a","reset_type":"codex_rate_limits","status":"available","expires_at":null}]}`
		case "POST /rate-limit-reset-credits/consume":
			posts++
			var payload map[string]string
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if payload["credit_id"] != "credit-a" || payload["redeem_request_id"] == "" {
				t.Error("wrong consumption target")
			}
			body = `{"code":"reset","windows_reset":1}`
		default:
			return nil, errors.New("unexpected path; network disabled")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	oauth.SetUsageURL("https://reset-api-fixture.invalid/usage")
	t.Cleanup(func() { http.DefaultTransport = oldTransport; oauth.SetUsageURL(oldUsage) })
	cfg := &config.File{Providers: []domain.Provider{{ID: "provider-a", Type: domain.ProviderCliproxyCodex}, {ID: "provider-b", Type: domain.ProviderCliproxyCodex}}, Connections: []domain.Connection{{ID: "connection-a", ProviderID: "provider-a", Credential: domain.CredentialRef{SecretRef: domain.OAuthRef("local-a")}}}}
	// Scheduler is deliberately absent: this path must not invoke cooldown,
	// quota-override, conversation, or other reset operations.
	server := New(Deps{Security: security, Control: security, Config: cfg, OAuth: oauth.NewManager(secrets)})
	handler := server.Routes()
	call := func(path, body string, cookies []*http.Cookie, csrf, bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "https://ferry.test"+path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		for _, cookie := range cookies {
			req.AddCookie(cookie)
		}
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	init := call("/api/auth/initialize", `{"username":"admin","password":"correct horse battery staple"}`, nil, "", "")
	if init.Code != 201 {
		t.Fatal(init.Body.String())
	}
	cookies := init.Result().Cookies()
	csrf := ""
	for _, c := range cookies {
		if c.Name == managementCSRFCookie {
			csrf = c.Value
		}
	}
	tokenRec := call("/api/management-tokens", `{"name":"offline-agent","allow_high_risk":true}`, cookies, csrf, "")
	var token map[string]any
	_ = json.Unmarshal(tokenRec.Body.Bytes(), &token)
	bearer, _ := token["secret"].(string)
	if bearer == "" {
		t.Fatal(tokenRec.Body.String())
	}
	base := "/api/providers/provider-a/connections/connection-a/oauth/usage-reset-requests"
	prepared := call(base, `{"account_id":"account-a","credit_id":"credit-a"}`, nil, "", bearer)
	if prepared.Code != 200 {
		t.Fatal(prepared.Body.String())
	}
	var envelope struct {
		Request  oauth.UsageResetRequest `json:"request"`
		Required bool                    `json:"confirmation_required"`
		Path     string                  `json:"console_path"`
	}
	_ = json.Unmarshal(prepared.Body.Bytes(), &envelope)
	if !envelope.Required || envelope.Request.Status != "pending" || !strings.Contains(envelope.Path, envelope.Request.ID) || posts != 0 {
		t.Fatal("prepare crossed confirmation boundary")
	}
	confirm := base + "/" + envelope.Request.ID + "/confirm"
	body := `{"account_id":"account-a","credit_id":"credit-a","confirmation":"consume_one_codex_usage_reset"}`
	for _, test := range []struct {
		name, path, body string
		cookies          []*http.Cookie
		csrf, bearer     string
		want             int
	}{
		{"no session", confirm, body, nil, "", "", 401},
		{"privileged token", confirm, body, nil, "", bearer, 403},
		{"missing CSRF", confirm, body, cookies, "", "", 403},
		{"missing explicit confirmation", confirm, `{"account_id":"account-a","credit_id":"credit-a"}`, cookies, csrf, "", 400},
		{"wrong credit", confirm, strings.Replace(body, "credit-a", "credit-b", 1), cookies, csrf, "", 409},
		{"wrong upstream account", confirm, strings.Replace(body, "account-a", "account-b", 1), cookies, csrf, "", 409},
		{"wrong provider", strings.Replace(confirm, "provider-a", "provider-b", 1), body, cookies, csrf, "", 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			rec := call(test.path, test.body, test.cookies, test.csrf, test.bearer)
			if rec.Code != test.want {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
			if posts != 0 {
				t.Fatal("rejected confirmation consumed credit")
			}
		})
	}
	if rec := call(confirm, body, cookies, csrf, ""); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	if posts != 1 {
		t.Fatalf("dispatch count %d", posts)
	}
	if rec := call(confirm, body, cookies, csrf, ""); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	if posts != 1 {
		t.Fatal("duplicate HTTP confirmation replayed consume")
	}
	binding := oauth.UsageResetBinding{ProviderID: "provider-a", ConnectionID: "connection-a", Account: "local-a"}
	cfg.Connections[0].Credential.SecretRef = domain.OAuthRef("other-account")
	if release, err := server.lockUsageResetTarget(binding); err == nil {
		release()
		t.Fatal("target guard accepted remapped account")
	}
	// Security-less development servers must also fail closed.
	handler = New(Deps{Config: cfg, OAuth: oauth.NewManager(secrets)}).Routes()
	if rec := call(confirm, body, nil, "", ""); rec.Code != 403 {
		t.Fatalf("security-less confirm=%d", rec.Code)
	}
}
