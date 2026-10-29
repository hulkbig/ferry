package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ferry/internal/secretstore"
)

func testJWT(claims map[string]any) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, _ := json.Marshal(claims)
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestParseAuthJSONOfficialAndCLIProxyShapes(t *testing.T) {
	now := time.Date(2026, 8, 30, 1, 0, 0, 0, time.UTC)
	idToken := testJWT(map[string]any{
		"email": "official@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct-official", "chatgpt_plan_type": "pro",
		},
	})
	accessToken := testJWT(map[string]any{"exp": now.Add(time.Hour).Unix()})
	official, err := ParseAuthJSON([]byte(fmt.Sprintf(`{
		"auth_mode":"chatgpt",
		"OPENAI_API_KEY":null,
		"tokens":{"id_token":%q,"access_token":%q,"refresh_token":"refresh-official","account_id":"acct-selected"},
		"last_refresh":"2026-08-30T00:55:00Z"
	}`, idToken, accessToken)), now)
	if err != nil {
		t.Fatal(err)
	}
	if official.AccessToken != accessToken || official.RefreshToken != "refresh-official" ||
		official.AccountID != "acct-selected" || official.Email != "official@example.com" || official.PlanType != "pro" ||
		!official.ExpiresAt.Equal(now.Add(time.Hour)) || !official.ObtainedAt.Equal(now.Add(-5*time.Minute)) {
		t.Fatalf("official auth.json = %+v", official)
	}

	flat, err := ParseAuthJSON([]byte(fmt.Sprintf(`{
		"type":"codex","id_token":%q,"access_token":"opaque-access","refresh_token":"refresh-flat",
		"account_id":"acct-flat","email":"flat@example.com","plan_type":"plus","expired":"2026-08-30T02:00:00Z"
	}`, idToken)), now)
	if err != nil {
		t.Fatal(err)
	}
	if flat.AccessToken != "opaque-access" || flat.AccountID != "acct-flat" || flat.Email != "flat@example.com" ||
		flat.PlanType != "plus" || !flat.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("CLIProxyAPI auth JSON = %+v", flat)
	}
}

func TestParseAuthJSONRejectsNonCodexAndAPIKeyOnlyFiles(t *testing.T) {
	for _, raw := range []string{
		`{"OPENAI_API_KEY":"sk-secret"}`,
		`{"type":"gemini","access_token":"a","refresh_token":"r"}`,
		`{"tokens":{"access_token":"a"}}`,
	} {
		if _, err := ParseAuthJSON([]byte(raw), time.Now()); err == nil {
			t.Fatalf("ParseAuthJSON(%s) succeeded", raw)
		}
	}
}

func TestImportAuthJSONPersistsOnlyNormalizedToken(t *testing.T) {
	store, err := secretstore.Open(t.TempDir() + "/secrets.json")
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store)
	now := time.Date(2026, 8, 30, 1, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	accessToken := testJWT(map[string]any{"exp": now.Add(time.Hour).Unix()})
	raw := fmt.Sprintf(`{"tokens":{"access_token":%q,"refresh_token":"refresh","account_id":"acct"},"untrusted_extra":"must-not-persist"}`, accessToken)
	view, err := manager.ImportAuthJSON("work", []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if view.Name != "work" || view.AccountID != "acct" || view.Expired {
		t.Fatalf("import view = %+v", view)
	}
	stored, ok := store.Get(storeKey("work"))
	if !ok || !strings.Contains(stored, "refresh") || strings.Contains(stored, "untrusted_extra") {
		t.Fatalf("normalized persisted token = %q", stored)
	}
	if strings.Contains(fmt.Sprintf("%+v", view), accessToken) {
		t.Fatal("redacted import response leaked an access token")
	}
}

func TestUsageQueriesOfficialEndpointWithAccountIdentity(t *testing.T) {
	var gotAuthorization, gotAccount, gotUserAgent string
	usageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		gotAccount = r.Header.Get("ChatGPT-Account-Id")
		gotUserAgent = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"plan_type":"pro",
			"rate_limit":{"allowed":true,"limit_reached":false,
				"primary_window":{"used_percent":37,"limit_window_seconds":18000,"reset_after_seconds":900,"reset_at":1788052500},
				"secondary_window":{"used_percent":12,"limit_window_seconds":604800,"reset_after_seconds":86400,"reset_at":1788138000}},
			"credits":{"has_credits":true,"unlimited":false,"balance":"42.5"},
			"additional_rate_limits":[{"limit_name":"GPT-5.3-Codex-Spark","metered_feature":"spark","rate_limit":{"allowed":true,"limit_reached":false}}],
			"rate_limit_reset_credits":{"available_count":2}
		}`))
	}))
	defer usageServer.Close()
	originalUsageURL := CurrentUsageURL()
	SetUsageURL(usageServer.URL)
	defer SetUsageURL(originalUsageURL)

	store, err := secretstore.Open(t.TempDir() + "/secrets.json")
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store)
	now := time.Now()
	accessToken := testJWT(map[string]any{"exp": now.Add(time.Hour).Unix()})
	_, err = manager.ImportAuthJSON("work", []byte(fmt.Sprintf(
		`{"tokens":{"access_token":%q,"refresh_token":"refresh","account_id":"acct-usage"}}`, accessToken)))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Usage(context.Background(), "work")
	if err != nil {
		t.Fatal(err)
	}
	if gotAuthorization != "Bearer "+accessToken || gotAccount != "acct-usage" || gotUserAgent != "codex-cli" {
		t.Fatalf("usage headers: authorization=%q account=%q user-agent=%q", gotAuthorization, gotAccount, gotUserAgent)
	}
	if snapshot.PlanType != "pro" || snapshot.RateLimit == nil || snapshot.RateLimit.PrimaryWindow == nil ||
		snapshot.RateLimit.PrimaryWindow.UsedPercent != 37 || snapshot.Credits == nil || snapshot.Credits.Balance != "42.5" ||
		len(snapshot.AdditionalRateLimits) != 1 || snapshot.RateLimitResetCredits == nil || snapshot.RateLimitResetCredits.AvailableCount != 2 {
		t.Fatalf("usage snapshot = %+v", snapshot)
	}
	cached, err := manager.CachedUsage("work")
	if err != nil || cached == nil || cached.ObservedAt.IsZero() || cached.PlanType != "pro" {
		t.Fatalf("cached usage snapshot = %+v, err=%v", cached, err)
	}
	reloaded := NewManager(store)
	cached, err = reloaded.CachedUsage("work")
	if err != nil || cached == nil || cached.ObservedAt.IsZero() || cached.PlanType != "pro" {
		t.Fatalf("persisted usage snapshot = %+v, err=%v", cached, err)
	}
}

func TestCachedUsageIsEmptyUntilSuccessfulQueryAndClearedOnImport(t *testing.T) {
	store, err := secretstore.Open(t.TempDir() + "/secrets.json")
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store)
	if cached, err := manager.CachedUsage("work"); err != nil || cached != nil {
		t.Fatalf("initial cached usage = %+v, err=%v", cached, err)
	}
	stale := &UsageSnapshot{PlanType: "pro", ObservedAt: time.Now()}
	manager.usageCache["work"] = stale
	encoded, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(usageStoreKey("work"), string(encoded)); err != nil {
		t.Fatal(err)
	}
	accessToken := testJWT(map[string]any{"exp": time.Now().Add(time.Hour).Unix()})
	if _, err := manager.ImportAuthJSON("work", []byte(fmt.Sprintf(
		`{"tokens":{"access_token":%q,"refresh_token":"refresh","account_id":"acct"}}`, accessToken))); err != nil {
		t.Fatal(err)
	}
	if cached, err := manager.CachedUsage("work"); err != nil || cached != nil {
		t.Fatalf("import did not clear stale usage: %+v, err=%v", cached, err)
	}
	reloaded := NewManager(store)
	if cached, err := reloaded.CachedUsage("work"); err != nil || cached != nil {
		t.Fatalf("import did not clear persisted stale usage: %+v, err=%v", cached, err)
	}
}

func TestUsageRefreshesOnceAfterUnauthorized(t *testing.T) {
	manager, tokenServer, cleanupTokenServer := newTestManager(t,
		`{"access_token":"refreshed-access","refresh_token":"rotated-refresh","expires_in":3600}`,
		http.StatusOK)
	defer cleanupTokenServer()

	now := time.Now()
	oldAccess := testJWT(map[string]any{"exp": now.Add(time.Hour).Unix()})
	if _, err := manager.ImportAuthJSON("work", []byte(fmt.Sprintf(
		`{"tokens":{"access_token":%q,"refresh_token":"original-refresh","account_id":"acct-refresh"}}`, oldAccess))); err != nil {
		t.Fatal(err)
	}

	var authorizations []string
	usageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		if len(authorizations) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan_type":"pro","additional_rate_limits":[]}`))
	}))
	defer usageServer.Close()
	originalUsageURL := CurrentUsageURL()
	SetUsageURL(usageServer.URL)
	defer SetUsageURL(originalUsageURL)

	if _, err := manager.Usage(context.Background(), "work"); err != nil {
		t.Fatal(err)
	}
	if tokenServer.callCount() != 1 {
		t.Fatalf("token refresh calls = %d, want 1", tokenServer.callCount())
	}
	want := []string{"Bearer " + oldAccess, "Bearer refreshed-access"}
	if fmt.Sprint(authorizations) != fmt.Sprint(want) {
		t.Fatalf("usage authorization sequence = %v, want %v", authorizations, want)
	}
}

func TestParseAuthJSONCLIProxyV8CredentialSelection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields string
		wantOK bool
	}{
		{"legacy", ``, true},
		{"inactive SIWC", `,"use_siwc":false,"siwc":{"access_token":"siwc-secret","client_id":"dynamic"}`, true},
		{"inactive SIWC string", `,"use_siwc":"false"`, true},
		{"active SIWC", `,"use_siwc":true`, false},
		{"active SIWC string", `,"use_siwc":"true"`, false},
		{"active SIWC numeric string", `,"use_siwc":"1"`, false},
		{"invalid selection", `,"use_siwc":{}`, false},
		{"invalid selection string", `,"use_siwc":"unknown"`, false},
		{"legacy client", `,"client_id":"` + ClientID + `"`, true},
		{"dynamic client", `,"client_id":"dynamic-secret"`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"type":"codex","access_token":"legacy-access","refresh_token":"legacy-refresh"` + tc.fields + `}`
			token, err := ParseAuthJSON([]byte(raw), time.Now())
			if (err == nil) != tc.wantOK {
				t.Fatalf("accepted=%v, want %v; error=%v", err == nil, tc.wantOK, err)
			}
			if err != nil {
				for _, secret := range []string{"legacy-access", "legacy-refresh", "siwc-secret", "dynamic-secret"} {
					if strings.Contains(err.Error(), secret) {
						t.Fatal("import error disclosed credential material")
					}
				}
				return
			}
			encoded, err := json.Marshal(token)
			if err != nil {
				t.Fatal(err)
			}
			if token.AccessToken != "legacy-access" || strings.Contains(string(encoded), "siwc-secret") {
				t.Fatal("legacy import retained the wrong credential partition")
			}
		})
	}
}

func TestImportSIWCDoesNotReplaceExistingAccount(t *testing.T) {
	store, err := secretstore.Open(t.TempDir() + "/secrets.db")
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store)
	defer manager.Close(context.Background())
	if _, err := manager.ImportAuthJSON("work", []byte(`{"access_token":"existing","refresh_token":"refresh"}`)); err != nil {
		t.Fatal(err)
	}
	before, _ := store.Get(storeKey("work"))
	_, err = manager.ImportAuthJSON("work", []byte(`{"type":"codex","use_siwc":true,"access_token":"other-legacy","refresh_token":"other-refresh","siwc":{"access_token":"selected"}}`))
	if err == nil {
		t.Fatal("SIWC import succeeded")
	}
	after, _ := store.Get(storeKey("work"))
	if before != after || manager.tokens["work"].AccessToken != "existing" {
		t.Fatal("rejected import replaced the existing credential")
	}
}
