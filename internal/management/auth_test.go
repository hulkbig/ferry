package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"ferry/internal/controlstore"
)

func TestManagementInitializationSessionCSRFAndPasswordRevocation(t *testing.T) {
	store, err := controlstore.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	handler := New(Deps{Security: store, Control: store}).Routes()
	call := func(method, path, body string, cookies []*http.Cookie, csrf string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "https://ferry.test"+path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		for _, cookie := range cookies {
			req.AddCookie(cookie)
		}
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	if rec := call(http.MethodGet, "/api/security-events", "", nil, ""); rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("pre-init protected=%d %s", rec.Code, rec.Body.String())
	}
	init := call(http.MethodPost, "/api/auth/initialize", `{"username":"admin","password":"correct horse battery staple"}`, nil, "")
	if init.Code != http.StatusCreated {
		t.Fatalf("initialize=%d %s", init.Code, init.Body.String())
	}
	cookies := init.Result().Cookies()
	var csrf string
	for _, cookie := range cookies {
		if cookie.Name == managementSessionCookie && (!cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode) {
			t.Fatalf("session cookie flags: %+v", cookie)
		}
		if cookie.Name == managementCSRFCookie {
			csrf = cookie.Value
		}
	}
	if csrf == "" {
		t.Fatal("csrf cookie missing")
	}
	if rec := call(http.MethodGet, "/api/auth/session", "", cookies, ""); rec.Code != http.StatusOK {
		t.Fatalf("session=%d %s", rec.Code, rec.Body.String())
	}
	if rec := call(http.MethodPost, "/api/auth/logout", "{}", cookies, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("missing csrf=%d %s", rec.Code, rec.Body.String())
	}
	change := call(http.MethodPost, "/api/auth/password", `{"current_password":"correct horse battery staple","new_password":"another correct horse battery"}`, cookies, csrf)
	if change.Code != http.StatusOK {
		t.Fatalf("password change=%d %s", change.Code, change.Body.String())
	}
	if rec := call(http.MethodGet, "/api/auth/session", "", cookies, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session=%d %s", rec.Code, rec.Body.String())
	}
	admin, err := store.ManagementAdministrator()
	if err != nil || admin == nil || admin.PasswordHash == "correct horse battery staple" {
		t.Fatalf("administrator verifier: %+v %v", admin, err)
	}
	events, err := store.ManagementSecurityEvents(50)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, event := range events {
		seen[event.EventType] = true
	}
	for _, event := range []string{"administrator_initialized", "session_created", "password_changed"} {
		if !seen[event] {
			t.Fatalf("missing audit event %s: %+v", event, events)
		}
	}
}

func TestManagementTokenAuthenticationAndAdministratorControlledHighRiskGate(t *testing.T) {
	store, err := controlstore.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	handler := New(Deps{Security: store, Control: store}).Routes()
	call := func(method, path, body string, cookies []*http.Cookie, csrf, bearer string) (*httptest.ResponseRecorder, map[string]any) {
		req := httptest.NewRequest(method, "https://ferry.test"+path, bytes.NewBufferString(body))
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
		var response map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &response)
		return rec, response
	}

	init, _ := call(http.MethodPost, "/api/auth/initialize",
		`{"username":"admin","password":"correct horse battery staple"}`, nil, "", "")
	if init.Code != http.StatusCreated {
		t.Fatalf("initialize=%d %s", init.Code, init.Body.String())
	}
	cookies := init.Result().Cookies()
	var csrf string
	for _, cookie := range cookies {
		if cookie.Name == managementCSRFCookie {
			csrf = cookie.Value
		}
	}
	if csrf == "" {
		t.Fatal("csrf cookie missing")
	}

	if rec, _ := call(http.MethodPost, "/api/management-tokens", `{"name":"skill"}`, cookies, "", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("token create without csrf=%d %s", rec.Code, rec.Body.String())
	}
	createdRec, created := call(http.MethodPost, "/api/management-tokens",
		`{"name":"skill","description":"automation"}`, cookies, csrf, "")
	if createdRec.Code != http.StatusCreated {
		t.Fatalf("token create=%d %s", createdRec.Code, createdRec.Body.String())
	}
	secret, _ := created["secret"].(string)
	createdToken, _ := created["management_token"].(map[string]any)
	tokenID, _ := createdToken["id"].(string)
	if secret == "" || tokenID == "" || createdToken["allow_high_risk"] != false {
		t.Fatalf("created response=%v", created)
	}
	if rec, _ := call(http.MethodPatch, "/api/management-tokens/"+tokenID,
		`{"expires_at":"2027-09-02T00:00:00Z","clear_expiry":true}`, cookies, csrf, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("ambiguous expiry patch=%d %s", rec.Code, rec.Body.String())
	}

	accessRec, access := call(http.MethodGet, "/api/access", "", nil, "", secret)
	if accessRec.Code != http.StatusOK || access["auth_type"] != "management_token" || access["allow_high_risk"] != false {
		t.Fatalf("access=%d %v", accessRec.Code, access)
	}
	if rec, response := call(http.MethodGet, "/api/management-tokens", "", nil, "", secret); rec.Code != http.StatusForbidden || response["error"].(map[string]any)["code"] != "ADMIN_SESSION_REQUIRED" {
		t.Fatalf("bearer token management=%d %v", rec.Code, response)
	}
	blockedRec, blocked := call(http.MethodPatch, "/api/settings",
		`{"debug_request_response":true}`, nil, "", secret)
	if blockedRec.Code != http.StatusForbidden || blocked["error"].(map[string]any)["code"] != "HIGH_RISK_NOT_ALLOWED" {
		t.Fatalf("high risk gate=%d %v", blockedRec.Code, blocked)
	}
	blockedSyncRec, blockedSync := call(http.MethodPost, "/api/providers/provider-a/models/sync", `{}`, nil, "", secret)
	if blockedSyncRec.Code != http.StatusForbidden || blockedSync["error"].(map[string]any)["code"] != "HIGH_RISK_NOT_ALLOWED" {
		t.Fatalf("model sync high risk gate=%d %v", blockedSyncRec.Code, blockedSync)
	}

	for _, path := range []string{"/api/imports/preview", "/api/imports/apply"} {
		rec, body := call(http.MethodPost, path, `{}`, nil, "", secret)
		if rec.Code != http.StatusForbidden || body["error"].(map[string]any)["code"] != "HIGH_RISK_NOT_ALLOWED" {
			t.Fatalf("import high risk gate=%d", rec.Code)
		}
	}

	patchRec, patched := call(http.MethodPatch, "/api/management-tokens/"+tokenID,
		`{"allow_high_risk":true}`, cookies, csrf, "")
	if patchRec.Code != http.StatusOK {
		t.Fatalf("enable high risk=%d %s", patchRec.Code, patchRec.Body.String())
	}
	patchedToken, _ := patched["management_token"].(map[string]any)
	if patchedToken["allow_high_risk"] != true {
		t.Fatalf("patched token=%v", patchedToken)
	}
	if rec, _ := call(http.MethodPatch, "/api/settings", `{"debug_request_response":true}`, nil, "", secret); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("enabled high-risk token did not reach handler: %d %s", rec.Code, rec.Body.String())
	}
	if rec, _ := call(http.MethodPost, "/api/providers/provider-a/models/sync", `{}`, nil, "", secret); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("enabled high-risk token did not reach model sync handler: %d %s", rec.Code, rec.Body.String())
	}

	revokeRec, _ := call(http.MethodPost, "/api/management-tokens/"+tokenID+"/revoke", `{}`, cookies, csrf, "")
	if revokeRec.Code != http.StatusOK {
		t.Fatalf("revoke=%d %s", revokeRec.Code, revokeRec.Body.String())
	}
	invalidRec, invalid := call(http.MethodGet, "/api/access", "", nil, "", secret)
	if invalidRec.Code != http.StatusUnauthorized || invalid["error"].(map[string]any)["code"] != "INVALID_MANAGEMENT_TOKEN" {
		t.Fatalf("revoked bearer=%d %v", invalidRec.Code, invalid)
	}
	events, err := store.ManagementSecurityEvents(100)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, event := range events {
		seen[event.EventType] = true
	}
	for _, eventType := range []string{"management_token_scope_denied", "management_token_high_risk_denied", "management_token_rejected"} {
		if !seen[eventType] {
			t.Fatalf("missing %s audit event: %+v", eventType, events)
		}
	}
}
