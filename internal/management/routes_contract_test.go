package management

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLegacyManagementRoutesAreOffline(t *testing.T) {
	handler := New(Deps{}).Routes()
	retired := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/state"},
		{http.MethodGet, "/api/dashboard"},
		{http.MethodGet, "/api/targets/target-id/usage"},
		{http.MethodGet, "/api/targets/target-id/credential"},
		{http.MethodPost, "/api/targets/target-id/set-secret"},
		{http.MethodPost, "/api/targets/target-id/clear-cooldown"},
		{http.MethodPost, "/api/targets/target-id/set-quota"},
		{http.MethodGet, "/api/consumers"},
		{http.MethodPost, "/api/consumers"},
		{http.MethodGet, "/api/consumers/consumer/key"},
		{http.MethodGet, "/api/consumers/consumer/analytics"},
		{http.MethodPost, "/api/consumers/consumer/rotate"},
		{http.MethodPatch, "/api/consumers/consumer"},
		{http.MethodDelete, "/api/consumers/consumer"},
		{http.MethodGet, "/api/policy/versions"},
		{http.MethodGet, "/api/policies"},
		{http.MethodPost, "/api/policies"},
		{http.MethodPost, "/api/policies/reorder"},
		{http.MethodGet, "/api/policies/policy-id"},
		{http.MethodPatch, "/api/policies/policy-id"},
		{http.MethodDelete, "/api/policies/policy-id"},
		{http.MethodPatch, "/api/policies/policy-id/enabled"},
		{http.MethodGet, "/api/policies/policy-id/revisions"},
		{http.MethodPost, "/api/policies/policy-id/rollback"},
		{http.MethodGet, "/api/config"},
		{http.MethodGet, "/api/config/raw"},
		{http.MethodPost, "/api/policy/validate"},
		{http.MethodPost, "/api/policy/dry-run"},
		{http.MethodPost, "/api/policy/publish"},
		{http.MethodPost, "/api/policy/rollback"},
		{http.MethodGet, "/api/channels"},
		{http.MethodPost, "/api/channels"},
		{http.MethodGet, "/api/channels/channel-id"},
		{http.MethodPatch, "/api/channels/channel-id"},
		{http.MethodDelete, "/api/channels/channel-id"},
		{http.MethodPost, "/api/channels/channel-id/test"},
		{http.MethodPost, "/api/channels/channel-id/oauth/login"},
		{http.MethodPost, "/api/channels/channel-id/oauth/callback"},
		{http.MethodGet, "/api/oauth/accounts"},
		{http.MethodGet, "/api/oauth/accounts/account/login"},
		{http.MethodDelete, "/api/oauth/accounts/account/login"},
		{http.MethodDelete, "/api/oauth/accounts/account"},
		{http.MethodGet, "/api/auth/security-events"},
	}
	for _, route := range retired {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			req := httptest.NewRequest(route.method, route.path, strings.NewReader(`{}`))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("retired route status=%d want=404 body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestVersionedManagementRoutesAreOffline(t *testing.T) {
	for _, path := range []string{"/api/v2/access", "/api/v2/models", "/api/v2/providers/provider-id"} {
		rec := httptest.NewRecorder()
		New(Deps{}).Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("retired versioned route %s status=%d want=404 body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestManagementAccessRouteIsRegistered(t *testing.T) {
	rec := httptest.NewRecorder()
	New(Deps{}).Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/access", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("management access status=%d body=%s", rec.Code, rec.Body.String())
	}
}
