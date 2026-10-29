package management

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"ferry/internal/config"
	"ferry/internal/controlstore"
	"ferry/internal/domain"
	"ferry/internal/oauth"
	"ferry/internal/secretstore"
)

func managementJWT(claims map[string]any) string {
	payload, _ := json.Marshal(claims)
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." +
		base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

// oauthAPITest builds a fixture wired to a real OAuth manager (over a temp
// secret store) whose token endpoint is a fake, so the full login lifecycle
// runs without touching auth.openai.com.
func oauthAPITest(t *testing.T) *mgmtFixture {
	t.Helper()
	f := secretsAPITest(t)
	st, err := secretstore.Open(t.TempDir() + "/secrets.json")
	if err != nil {
		t.Fatalf("secret store: %v", err)
	}
	control, err := controlstore.Open(t.TempDir() + "/control.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close() })
	oauthMgr := oauth.NewManager(st)
	f.srv = New(Deps{
		Control:        control,
		Scheduler:      f.sch,
		Manager:        f.mgr,
		Ledger:         f.led,
		Config:         f.cfg,
		Compiler:       nil,
		Store:          st,
		OAuth:          oauthMgr,
		Publish:        f.srv.deps.Publish,
		RefreshRuntime: f.srv.deps.Publish,
		Timezone:       nil,
	})
	return f
}

// withCodexChannel adds a cliproxy-codex channel whose expanded targets use
// an oauth: credential ref.
func withCodexChannel(f *mgmtFixture) {
	f.cfg.Channels = []domain.Channel{{
		ID: "work", Type: domain.ChannelCliproxyCodex, PlanID: "free", Pools: []string{"p1"},
		Models: []domain.ChannelModel{{
			Model: "gpt-5.5", Protocols: []domain.Protocol{domain.ProtocolOpenAIResponses},
			Capabilities: domain.Capabilities{Text: true, Stream: true},
		}},
	}}
}

// startAndCompleteLogin drives one full login against the fake token server
// and fails the test if any step errors.
func startAndCompleteLogin(t *testing.T, f *mgmtFixture) {
	t.Helper()
	rec, out := f.do(http.MethodPost, "/api/channels/work/oauth/login", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("start code=%d body=%v", rec.Code, out)
	}
	u, err := url.Parse(out["authorize_url"].(string))
	if err != nil {
		t.Fatalf("authorize_url: %v", out["authorize_url"])
	}
	rec, out = f.do(http.MethodPost, "/api/channels/work/oauth/callback",
		map[string]string{"code": "the-code", "state": u.Query().Get("state")})
	if rec.Code != http.StatusOK {
		t.Fatalf("callback code=%d body=%v", rec.Code, out)
	}
}

func TestOAuthRoutesDisabledWithoutManager(t *testing.T) {
	f := newMgmtFixture(t) // Deps.OAuth nil
	rec, out := f.do(http.MethodGet, "/api/oauth/accounts", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("accounts code=%d body=%v, want 503", rec.Code, out)
	}
	rec, _ = f.do(http.MethodPost, "/api/channels/x/oauth/login", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("login code=%d, want 503", rec.Code)
	}
	rec, _ = f.do(http.MethodDelete, "/api/oauth/accounts/work", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("delete code=%d, want 503", rec.Code)
	}
}

func TestChannelMustBeOAuthManaged(t *testing.T) {
	restore := fakeTokenEndpoint(t)
	defer restore()
	f := oauthAPITest(t)
	f.cfg.Channels = []domain.Channel{{ID: "plain", Type: domain.ChannelCustom,
		BaseURL: "http://127.0.0.1:9",
		Models:  []domain.ChannelModel{{Model: "m", Protocols: []domain.Protocol{domain.ProtocolOpenAIChat}}}}}

	rec, out := f.do(http.MethodPost, "/api/channels/plain/oauth/login", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("custom channel login code=%d body=%v, want 400", rec.Code, out)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "not an oauth-managed channel") {
		t.Fatalf("error = %v", out)
	}
	rec, _ = f.do(http.MethodPost, "/api/channels/ghost/oauth/login", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown channel code=%d, want 404", rec.Code)
	}
}

func TestLoginLifecycleAndRepublish(t *testing.T) {
	restore := fakeTokenEndpoint(t)
	defer restore()
	restoreCatalog := fakeModelCatalogEndpoint(t)
	defer restoreCatalog()
	f := oauthAPITest(t)
	withCodexChannel(f)

	// start: returns an authorize URL carrying state + PKCE challenge
	rec, out := f.do(http.MethodPost, "/api/channels/work/oauth/login", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("start code=%d body=%v", rec.Code, out)
	}
	authURL, _ := out["authorize_url"].(string)
	if !strings.HasPrefix(authURL, oauth.AuthURL+"?") ||
		!strings.Contains(authURL, "code_challenge_method=S256") ||
		!strings.Contains(authURL, "state=") {
		t.Fatalf("authorize_url = %q", authURL)
	}
	u, _ := url.Parse(authURL)
	state := u.Query().Get("state")

	// pending logins are visible without exposing state or the PKCE verifier
	rec, out = f.do(http.MethodGet, "/api/oauth/accounts", nil)
	if items := out["accounts"].([]any); len(items) != 1 ||
		items[0].(map[string]any)["status"] != "pending" {
		t.Fatalf("pre-login accounts = %v", items)
	}

	// complete from the pasted redirect URL; publish must fire
	before := f.publishCount()
	rec, out = f.do(http.MethodPost, "/api/channels/work/oauth/callback",
		map[string]string{"callback_url": "http://localhost:1455/auth/callback?code=the-code&state=" + state})
	if rec.Code != http.StatusOK || out["status"] != "active" {
		t.Fatalf("callback code=%d body=%v", rec.Code, out)
	}
	if after := f.publishCount(); after != before+1 {
		t.Fatalf("republish not triggered: %d -> %d", before, after)
	}

	// account now listed, redacted: identity yes, tokens never
	rec, out = f.do(http.MethodGet, "/api/oauth/accounts", nil)
	body := rec.Body.String()
	for _, leak := range []string{"at-123", "rt-456"} {
		if strings.Contains(body, leak) {
			t.Fatalf("token material %q leaked via /api/oauth/accounts", leak)
		}
	}
	items := out["accounts"].([]any)
	if len(items) != 1 {
		t.Fatalf("accounts = %v", items)
	}
	acct := items[0].(map[string]any)
	if acct["name"] != "work" || acct["email"] != "user@x.com" ||
		acct["plan_type"] != "pro" || acct["expired"] != false {
		t.Fatalf("account view = %v", acct)
	}
	if chans, _ := acct["channels"].([]any); len(chans) != 1 || chans[0] != "work" {
		t.Fatalf("channels = %v", chans)
	}
	rec, out = f.do(http.MethodPost, "/api/providers/discover-models", map[string]any{
		"type": "cliproxy-codex", "credential_ref": domain.OAuthRef("work"),
	})
	if rec.Code != http.StatusOK || out["source"] != "oauth-catalog" || out["plan_type"] != "pro" {
		t.Fatalf("live OAuth model discovery=%d %v", rec.Code, out)
	}
	foundSpark, foundNew := false, false
	for _, model := range out["models"].([]any) {
		foundSpark = foundSpark || model == "gpt-5.3-codex-spark"
		foundNew = foundNew || model == "gpt-brand-new"
	}
	if !foundSpark || !foundNew {
		t.Fatalf("live model catalog=%v", out["models"])
	}

	// bad state rejected
	rec, out = f.do(http.MethodPost, "/api/channels/work/oauth/callback",
		map[string]string{"code": "c2", "state": "wrong"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad-state callback code=%d body=%v, want 400", rec.Code, out)
	}

	// logout republishes and empties the listing
	before = f.publishCount()
	rec, out = f.do(http.MethodDelete, "/api/oauth/accounts/work", nil)
	if rec.Code != http.StatusOK || out["deleted"] != "work" {
		t.Fatalf("delete code=%d body=%v", rec.Code, out)
	}
	if after := f.publishCount(); after != before+1 {
		t.Fatalf("logout did not republish (%d -> %d)", before, after)
	}
	rec, out = f.do(http.MethodGet, "/api/oauth/accounts", nil)
	if items := out["accounts"].([]any); len(items) != 0 {
		t.Fatalf("post-logout accounts = %v", items)
	}
}

func TestDeviceLoginStatusAndCancel(t *testing.T) {
	restoreToken := fakeTokenEndpoint(t)
	defer restoreToken()
	mux := http.NewServeMux()
	mux.HandleFunc("/usercode", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"device_auth_id":"device-1","user_code":"ABCD-EFGH","interval":1}`))
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"authorization_code":"the-code","code_verifier":"device-verifier","code_challenge":"device-challenge"}`))
	})
	deviceServer := httptest.NewServer(mux)
	defer deviceServer.Close()
	restoreDevice := oauth.SetDeviceEndpoints(deviceServer.URL+"/usercode", deviceServer.URL+"/token")
	defer restoreDevice()

	f := oauthAPITest(t)
	withCodexChannel(f)
	before := f.publishCount()
	rec, out := f.do(http.MethodPost, "/api/channels/work/oauth/login", map[string]any{"mode": "device"})
	if rec.Code != http.StatusOK || out["mode"] != "device" || out["user_code"] != "ABCD-EFGH" {
		t.Fatalf("device start code=%d body=%v", rec.Code, out)
	}
	deadline := time.Now().Add(time.Second)
	for f.publishCount() == before && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if after := f.publishCount(); after != before+1 {
		t.Fatalf("device completion did not refresh runtime: %d -> %d", before, after)
	}
	rec, _ = f.do(http.MethodGet, "/api/oauth/accounts/work/login", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("completed device login status code=%d", rec.Code)
	}

	// A new browser flow can be inspected and cancelled independently of the
	// already active account token.
	rec, out = f.do(http.MethodPost, "/api/channels/work/oauth/login", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("browser restart=%d %v", rec.Code, out)
	}
	rec, out = f.do(http.MethodGet, "/api/oauth/accounts/work/login", nil)
	if rec.Code != http.StatusOK || out["mode"] != "browser" || out["status"] != "pending" {
		t.Fatalf("login status=%d %v", rec.Code, out)
	}
	rec, out = f.do(http.MethodDelete, "/api/oauth/accounts/work/login", nil)
	if rec.Code != http.StatusOK || out["status"] != "cancelled" {
		t.Fatalf("cancel=%d %v", rec.Code, out)
	}
}

func TestV2ProviderConnectionOAuthLifecycle(t *testing.T) {
	restore := fakeTokenEndpoint(t)
	defer restore()
	f := oauthAPITest(t)
	f.cfg.V2DomainSource = config.V2DomainSourceNative
	f.cfg.Providers = []domain.Provider{{
		ID: "openai", Name: "OpenAI", Type: domain.ProviderCliproxyCodex,
		BaseURL: "https://example.test", Enabled: true,
		Connections: []domain.ConnectionRef{{ID: "work"}},
	}}
	f.cfg.Connections = []domain.Connection{{
		ID: "work", ProviderID: "openai", Enabled: true,
		Credential: domain.CredentialRef{SecretRef: domain.OAuthRef("work-account")},
		Models: []domain.ConnectionModel{{
			UpstreamModel: "gpt-5.5", Protocols: []domain.Protocol{domain.ProtocolOpenAIResponses},
			Capabilities: domain.Capabilities{Text: true},
		}},
	}}
	f.cfg.LogicalModels = []domain.LogicalModel{{
		ID: "smart", Enabled: true,
		DefaultRoute: domain.RouteSpec{Steps: []domain.RouteStep{{ProviderID: "openai", UpstreamModel: "gpt-5.5"}}},
	}}
	rec, out := f.do(http.MethodPost, "/api/providers/openai/connections/work/oauth/login", nil)
	if rec.Code != http.StatusOK || out["connection_id"] != "work" || out["account"] != "work-account" {
		t.Fatalf("V2 oauth login code=%d body=%v", rec.Code, out)
	}
	rec, out = f.do(http.MethodGet, "/api/providers/openai/connections/work/oauth/status", nil)
	if rec.Code != http.StatusOK || out["status"] != "pending" || out["login"] == nil {
		t.Fatalf("V2 oauth pending status code=%d body=%v", rec.Code, out)
	}
	rec, out = f.do(http.MethodDelete, "/api/providers/openai/connections/work/oauth/login", nil)
	if rec.Code != http.StatusOK || out["status"] != "cancelled" {
		t.Fatalf("V2 oauth cancel code=%d body=%v", rec.Code, out)
	}
	rec, out = f.do(http.MethodPost, "/api/providers/openai/connections/work/oauth/login", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("V2 oauth restart code=%d body=%v", rec.Code, out)
	}
	authorizeURL, err := url.Parse(out["authorize_url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	before := f.publishCount()
	rec, out = f.do(http.MethodPost, "/api/providers/openai/connections/work/oauth/callback", map[string]string{
		"code": "the-code", "state": authorizeURL.Query().Get("state"),
	})
	if rec.Code != http.StatusOK || out["status"] != "active" || out["connection_id"] != "work" {
		t.Fatalf("V2 oauth callback code=%d body=%v", rec.Code, out)
	}
	if after := f.publishCount(); after != before+1 {
		t.Fatalf("V2 oauth callback did not refresh runtime: %d -> %d", before, after)
	}
	rec, out = f.do(http.MethodGet, "/api/providers/openai/connections/work/oauth/status", nil)
	if rec.Code != http.StatusOK || out["status"] != "active" || out["credential"] == nil {
		t.Fatalf("V2 oauth active status code=%d body=%v", rec.Code, out)
	}
	before = f.publishCount()
	rec, out = f.do(http.MethodDelete, "/api/providers/openai/connections/work/oauth", nil)
	if rec.Code != http.StatusOK || out["status"] != "disconnected" {
		t.Fatalf("V2 oauth disconnect code=%d body=%v", rec.Code, out)
	}
	if after := f.publishCount(); after != before+1 {
		t.Fatalf("V2 oauth disconnect did not refresh runtime: %d -> %d", before, after)
	}
}

func TestV2ProviderConnectionAuthJSONImportAndUsage(t *testing.T) {
	f := oauthAPITest(t)
	f.cfg.V2DomainSource = config.V2DomainSourceNative
	f.cfg.Providers = []domain.Provider{{
		ID: "openai", Name: "OpenAI", Type: domain.ProviderCliproxyCodex,
		BaseURL: domain.CodexBaseURL, Enabled: true,
		Connections: []domain.ConnectionRef{{ID: "work"}},
	}}
	f.cfg.Connections = []domain.Connection{{
		ID: "work", ProviderID: "openai", Identity: "Work account", Enabled: true,
		Credential: domain.CredentialRef{SecretRef: domain.OAuthRef("work-account")},
		Models: []domain.ConnectionModel{{
			UpstreamModel: "gpt-5.6-sol", Protocols: []domain.Protocol{domain.ProtocolOpenAIResponses},
			Capabilities: domain.Capabilities{Text: true, Stream: true},
		}},
	}}
	f.cfg.LogicalModels = []domain.LogicalModel{{
		ID: "smart", Enabled: true,
		DefaultRoute: domain.RouteSpec{Steps: []domain.RouteStep{{ProviderID: "openai", UpstreamModel: "gpt-5.6-sol"}}},
	}}

	accessToken := managementJWT(map[string]any{"exp": time.Now().Add(time.Hour).Unix()})
	authJSON := map[string]any{
		"auth_mode":      "chatgpt",
		"OPENAI_API_KEY": nil,
		"tokens": map[string]any{
			"id_token": mgmtTokenIDToken, "access_token": accessToken,
			"refresh_token": "imported-refresh", "account_id": "acct-imported",
		},
		"last_refresh": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
	}
	before := f.publishCount()
	rec, out := f.do(http.MethodPost, "/api/providers/openai/connections/work/oauth/import", map[string]any{"auth_json": authJSON})
	if rec.Code != http.StatusOK || out["status"] != "active" {
		t.Fatalf("auth.json import code=%d body=%v", rec.Code, out)
	}
	if after := f.publishCount(); after != before+1 {
		t.Fatalf("auth.json import did not refresh runtime: %d -> %d", before, after)
	}
	if strings.Contains(rec.Body.String(), accessToken) || strings.Contains(rec.Body.String(), "imported-refresh") {
		t.Fatal("auth.json import response leaked token material")
	}
	account := out["account"].(map[string]any)
	if account["account_id"] != "acct-imported" || account["email"] != "user@x.com" || account["plan_type"] != "pro" {
		t.Fatalf("redacted imported account=%v", account)
	}

	var gotAuthorization, gotAccount string
	usageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		gotAccount = r.Header.Get("ChatGPT-Account-Id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan_type":"pro","rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":18,"limit_window_seconds":18000,"reset_after_seconds":600,"reset_at":1788052500}},"credits":{"has_credits":true,"unlimited":false,"balance":"25"},"additional_rate_limits":[]}`))
	}))
	defer usageServer.Close()
	originalUsageURL := oauth.CurrentUsageURL()
	oauth.SetUsageURL(usageServer.URL)
	defer oauth.SetUsageURL(originalUsageURL)

	rec, out = f.do(http.MethodGet, "/api/providers/openai/connections/work/oauth/usage?refresh=false", nil)
	if rec.Code != http.StatusOK || out["usage"] != nil || gotAuthorization != "" {
		t.Fatalf("initial cached usage code=%d body=%v authorization=%q", rec.Code, out, gotAuthorization)
	}

	rec, out = f.do(http.MethodGet, "/api/providers/openai/connections/work/oauth/usage", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("usage code=%d body=%v", rec.Code, out)
	}
	if gotAuthorization != "Bearer "+accessToken || gotAccount != "acct-imported" {
		t.Fatalf("usage headers authorization=%q account=%q", gotAuthorization, gotAccount)
	}
	usage := out["usage"].(map[string]any)
	if usage["plan_type"] != "pro" || usage["observed_at"] == nil {
		t.Fatalf("usage response=%v", usage)
	}
	if strings.Contains(rec.Body.String(), accessToken) || strings.Contains(rec.Body.String(), "imported-refresh") {
		t.Fatal("usage response leaked token material")
	}

	gotAuthorization = ""
	if len(f.mgr.ConnectionStates(time.Now())["work"].QuotaWindows) == 0 {
		t.Fatal("fresh upstream usage did not update scheduler")
	}
	if err := f.mgr.ResetCooldowns(nil); err != nil {
		t.Fatal(err)
	}
	rec, out = f.do(http.MethodGet, "/api/providers/openai/connections/work/oauth/usage?refresh=false", nil)
	if rec.Code != http.StatusOK || out["usage"].(map[string]any)["plan_type"] != "pro" || gotAuthorization != "" {
		t.Fatalf("cached usage code=%d body=%v unexpectedly queried upstream=%q", rec.Code, out, gotAuthorization)
	}
	if len(f.mgr.ConnectionStates(time.Now())["work"].QuotaWindows) != 0 {
		t.Fatal("cached display read undid runtime reset")
	}

	rec, _ = f.do(http.MethodPost, "/api/providers/openai/connections/work/oauth/import", map[string]any{
		"auth_json": map[string]any{"OPENAI_API_KEY": "sk-not-oauth"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("API-key-only auth.json code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCallbackWithoutPendingLoginFails(t *testing.T) {
	restore := fakeTokenEndpoint(t)
	defer restore()
	f := oauthAPITest(t)
	withCodexChannel(f)
	rec, out := f.do(http.MethodPost, "/api/channels/work/oauth/callback",
		map[string]string{"code": "x", "state": "y"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%v, want 400", rec.Code, out)
	}
}

func TestCodexTargetCredentialTracksLoginState(t *testing.T) {
	restore := fakeTokenEndpoint(t)
	defer restore()
	f := oauthAPITest(t)
	withCodexChannel(f)

	// The compiler expands channel models into targets carrying an oauth:
	// ref; mirror that shape by hand for the credential-status endpoint.
	f.cfg.Targets = append(f.cfg.Targets, domain.ResourceTarget{
		ID:           "work-gpt-5-5",
		EndpointID:   "ep1",
		Model:        "gpt-5.5",
		PlanID:       "free",
		Capabilities: domain.Capabilities{Text: true, Stream: true},
		Credential:   domain.CredentialRef{SecretRef: domain.OAuthRef("work")},
	})
	f.cfg.Pools[0].Targets = append(f.cfg.Pools[0].Targets, "work-gpt-5-5")

	// not logged in yet → resolvable false; secret entry never applies
	rec, out := f.do(http.MethodGet, "/api/targets/work-gpt-5-5/credential", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("credential status code=%d body=%s", rec.Code, rec.Body.String())
	}
	if out["resolvable"] != false || out["provisionable_here"] != false {
		t.Fatalf("pre-login credential = %v", out)
	}

	startAndCompleteLogin(t, f)

	rec, out = f.do(http.MethodGet, "/api/targets/work-gpt-5-5/credential", nil)
	if out["resolvable"] != true {
		t.Fatalf("post-login credential = %v", out)
	}
	if name, _ := out["secret_name"].(string); name != "" {
		t.Fatalf("oauth ref got a store name: %v", out)
	}
	if strings.Contains(rec.Body.String(), "at-123") {
		t.Fatal("access token leaked via credential status")
	}

	// set-secret on an oauth target must refuse with the login guidance
	rec, out = f.do(http.MethodPost, "/api/targets/work-gpt-5-5/set-secret",
		map[string]string{"value": "sk-manual"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("set-secret on oauth target code=%d body=%v, want 400", rec.Code, out)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "browser login") {
		t.Fatalf("error = %v", out)
	}
}

func TestSecretListSkipsOAuthEntries(t *testing.T) {
	restore := fakeTokenEndpoint(t)
	defer restore()
	f := oauthAPITest(t)
	withCodexChannel(f)
	startAndCompleteLogin(t, f)

	// the persisted token lives under oauth:codex:work in the shared store;
	// the secrets listing must not surface it as an operator-editable key
	rec, out := f.do(http.MethodGet, "/api/secrets", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list code=%d", rec.Code)
	}
	items := out["secrets"].([]any)
	for _, it := range items {
		name := it.(map[string]any)["name"].(string)
		if strings.Contains(name, "oauth:") {
			t.Fatalf("oauth token entry surfaced as a secret: %v", name)
		}
	}
}

// mgmtTokenRespJSON mirrors the oauth package's canned exchange response; the
// id_token decodes to email user@x.com, account acct-9, plan pro.
const mgmtTokenIDToken = "eyJhbGci" + "OiJub25l" + "In0" +
	"." +
	"eyJlbWFpbCI6InVzZXJAeC5jb20iLCJodHRwczovL2FwaS5vcGVuYWkuY29tL2F1dGgiOnsiY2hhdGdwdF9hY2NvdW50X2lkIjoiYWNjdC05IiwiY2hhdGdwdF9wbGFuX3R5cGUiOiJwcm8ifX0" +
	"."

const mgmtTokenRespJSON = `{
  "access_token": "at-123",
  "refresh_token": "rt-456",
  "id_token": "` + mgmtTokenIDToken + `",
  "token_type": "bearer",
  "expires_in": 3600
}`

// fakeTokenEndpoint points the oauth package at a local fake token server
// answering with the standard canned response. The returned func restores.
func fakeTokenEndpoint(t *testing.T) func() {
	t.Helper()
	srv := newMgmtFakeTokenServer(t, http.StatusOK, mgmtTokenRespJSON)
	orig := oauth.CurrentTokenURL()
	oauth.SetTokenURL(srv.URL + "/oauth/token")
	return func() {
		oauth.SetTokenURL(orig)
		srv.Close()
	}
}

// fakeModelCatalogEndpoint points the oauth package at a local fake Codex
// model catalog including one model absent from the built-in tables, proving
// discovery reads the account-scoped remote catalog instead of static lists.
func fakeModelCatalogEndpoint(t *testing.T) func() {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/backend-api/codex/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[
			{"slug":"gpt-5.3-codex-spark","visibility":"list"},
			{"slug":"gpt-brand-new","visibility":"list","context_window":372000,
			 "input_modalities":["text","image"]}
		]}`))
	})
	srv := httptest.NewServer(mux)
	orig := oauth.CurrentModelCatalogURL()
	oauth.SetModelCatalogURL(srv.URL + "/backend-api/codex/models")
	return func() {
		oauth.SetModelCatalogURL(orig)
		srv.Close()
	}
}

// newMgmtFakeTokenServer stands in for auth.openai.com's token endpoint
// (mirrors the oauth package's test double; separate copy so the two test
// packages stay independent).
func newMgmtFakeTokenServer(t *testing.T, status int, respBody string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	})
	return httptest.NewServer(mux)
}
