package management

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"ferry/internal/config"
	"ferry/internal/controlstore"
	"ferry/internal/domain"
	"ferry/internal/oauth"
	"ferry/internal/secretstore"
)

func TestAutomaticUsageResetSettings(t *testing.T) {
	f := newV2APIFixture(t)
	rec, settings := f.do(http.MethodGet, "/api/settings", nil)
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	policy := settings[automaticUsageResetKey].(map[string]any)
	if policy["enabled"] != true || policy["threshold_minutes"] != float64(20) {
		t.Fatal(policy)
	}
	for _, minutes := range []int{0, -1, 1441} {
		rec, _ = f.do(http.MethodPatch, "/api/settings", map[string]any{automaticUsageResetKey: map[string]any{"enabled": true, "threshold_minutes": minutes}})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("minutes=%d: %s", minutes, rec.Body.String())
		}
	}
	rec, _ = f.do(http.MethodPatch, "/api/settings", map[string]any{automaticUsageResetKey: map[string]any{"enabled": false, "threshold_minutes": 7}})
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	fresh := New(f.server.deps)
	saved, err := fresh.automaticUsageResetPolicy()
	if err != nil || saved.Enabled || saved.ThresholdMinutes != 7 {
		t.Fatalf("saved=%+v err=%v", saved, err)
	}
}

func TestAutomaticUsageResetBackgroundTargetsAndPolicy(t *testing.T) {
	control, err := controlstore.Open(t.TempDir() + "/control.db")
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	secrets, err := secretstore.OpenSQLite(t.TempDir() + "/secrets.db")
	if err != nil {
		t.Fatal(err)
	}
	defer secrets.Close()
	raw, _ := json.Marshal(oauth.Token{AccessToken: "offline-test-token", RefreshToken: "offline-refresh", AccountID: "account-a", ExpiresAt: time.Now().Add(time.Hour)})
	if err := secrets.Set("oauth:codex:local-a", string(raw)); err != nil {
		t.Fatal(err)
	}
	oldTransport, oldUsage := http.DefaultTransport, oauth.CurrentUsageURL()
	posts, reads := 0, 0
	http.DefaultTransport = resetAPIRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "reset-auto.invalid" {
			return nil, errors.New("network disabled")
		}
		body := ""
		switch r.Method + " " + r.URL.Path {
		case "GET /usage":
			body = `{"account_id":"account-a"}`
		case "GET /rate-limit-reset-credits":
			reads++
			body = `{"available_count":1,"credits":[{"id":"credit-a","reset_type":"codex_rate_limits","status":"available","expires_at":"` + time.Now().Add(10*time.Minute).UTC().Format(time.RFC3339) + `"}]}`
		case "POST /rate-limit-reset-credits/consume":
			posts++
			body = `{"code":"reset","windows_reset":1}`
		default:
			return nil, errors.New("unexpected path")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	oauth.SetUsageURL("https://reset-auto.invalid/usage")
	t.Cleanup(func() { http.DefaultTransport = oldTransport; oauth.SetUsageURL(oldUsage) })
	cfg := &config.File{Providers: []domain.Provider{{ID: "p", Type: domain.ProviderCliproxyCodex, Enabled: true}}, Connections: []domain.Connection{{ID: "c", ProviderID: "p", Enabled: true, Credential: domain.CredentialRef{SecretRef: domain.OAuthRef("local-a")}}}}
	s := New(Deps{Control: control, Config: cfg, OAuth: oauth.NewManager(secrets)})
	ctx := context.Background()
	cfg.Providers[0].Enabled = false
	s.runAutomaticUsageReset(ctx)
	cfg.Providers[0].Enabled = true
	cfg.Connections[0].Enabled = false
	s.runAutomaticUsageReset(ctx)
	cfg.Connections[0].Enabled = true
	if reads != 0 || posts != 0 {
		t.Fatal("disabled target contacted")
	}
	if err := s.saveAutomaticUsageResetPolicy(automaticUsageResetPolicy{Enabled: false, ThresholdMinutes: 20}); err != nil {
		t.Fatal(err)
	}
	s.runAutomaticUsageReset(ctx)
	if reads != 0 {
		t.Fatal("disabled schedule contacted upstream")
	}
	if err := s.saveAutomaticUsageResetPolicy(automaticUsageResetPolicy{Enabled: true, ThresholdMinutes: 5}); err != nil {
		t.Fatal(err)
	}
	s.runAutomaticUsageReset(ctx)
	if posts != 0 {
		t.Fatal("threshold ignored")
	}
	policy := automaticUsageResetPolicy{Enabled: true, ThresholdMinutes: 20}
	if err := s.saveAutomaticUsageResetPolicy(policy); err != nil {
		t.Fatal(err)
	}
	s.runAutomaticUsageReset(ctx)
	s.runAutomaticUsageReset(ctx)
	if posts != 1 {
		t.Fatalf("posts=%d", posts)
	}
	binding := oauth.UsageResetBinding{ProviderID: "p", ConnectionID: "c", Account: "local-a"}
	if err := s.saveAutomaticUsageResetPolicy(automaticUsageResetPolicy{Enabled: false, ThresholdMinutes: 20}); err != nil {
		t.Fatal(err)
	}
	if release, err := s.lockAutomaticUsageResetTarget(binding, policy); err == nil {
		release()
		t.Fatal("stale policy accepted")
	}
}

func TestAutomaticUsageResetCorruptPolicyFailsClosed(t *testing.T) {
	f := newV2APIFixture(t)
	for _, raw := range []string{"null", "{}", "invalid", `{"enabled":true,"threshold_minutes":0}`} {
		if err := f.control.SetRuntimeSetting(automaticUsageResetKey, raw); err != nil {
			t.Fatal(err)
		}
		if _, err := f.server.automaticUsageResetPolicy(); err == nil {
			t.Fatalf("invalid policy accepted: %s", raw)
		}
	}
}
