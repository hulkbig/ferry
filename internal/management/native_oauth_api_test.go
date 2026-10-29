package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ferry/internal/config"
	"ferry/internal/controlstore"
	"ferry/internal/nativeauth"
	"ferry/internal/secretstore"
)

type offlineNativeDriver struct {
	fields            map[string]string
	callback          string
	begins, completes int
	pending           bool
}

func (d *offlineNativeDriver) Info() nativeauth.Info {
	return nativeauth.Info{ID: "offline", Mode: "browser"}
}
func (d *offlineNativeDriver) Begin(_ context.Context, _ *http.Client, o nativeauth.LoginOptions) (nativeauth.Session, error) {
	d.begins++
	d.fields = o.Fields
	return nativeauth.Session{State: o.State, Verifier: "private-verifier", DeviceCode: "private-device", URL: "https://example.invalid/login", UserCode: "user-code", ExpiresAt: time.Now().Add(time.Hour)}, nil
}
func (d *offlineNativeDriver) Complete(_ context.Context, _ *http.Client, _ nativeauth.Session, callback string) (nativeauth.Token, error) {
	d.completes++
	d.callback = callback
	if d.pending {
		return nativeauth.Token{}, nativeauth.ErrPending
	}
	return nativeauth.Token{AccessToken: "private-access", RefreshToken: "private-refresh"}, nil
}
func (d *offlineNativeDriver) Refresh(context.Context, *http.Client, nativeauth.Token) (nativeauth.Token, error) {
	panic("no provider refresh allowed")
}

func nativeCall(h http.Handler, method, path, body, bearer string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

const nativeAccountPath = "/api/oauth/native/offline/accounts/acct"

func TestNativeOAuthLifecycleBindingAndSafeView(t *testing.T) {
	store, err := secretstore.OpenSQLite(filepath.Join(t.TempDir(), "secrets.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	driver := &offlineNativeDriver{}
	manager := nativeauth.NewManager(store, driver)
	cfg := &config.File{}
	refreshes := 0
	failRefresh := false
	s := New(Deps{NativeOAuth: manager, Config: cfg, RefreshRuntime: func(f *config.File) error {
		if f != cfg {
			t.Fatal("refresh did not use active config")
		}
		refreshes++
		if failRefresh {
			return errors.New("private-runtime-error")
		}
		return nil
	}})
	h := s.Routes()
	check := func(method, path, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		w := nativeCall(h, method, path, body, "")
		if w.Code != want {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body)
		}
		for _, secret := range []string{"private-verifier", "private-device", "private-access", "private-refresh", "private-callback", "private-runtime-error"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatalf("response exposed %s", secret)
			}
		}
		return w
	}
	check("GET", "/api/oauth/native/drivers", "", 200)
	check("POST", nativeAccountPath+"/login", `{"fields":{"project":"example"}}`, 200)
	if driver.fields["project"] != "example" {
		t.Fatal("fields not bound")
	}
	ref := nativeauth.Ref("offline", "acct")
	before, _ := store.Get(ref)
	check("GET", nativeAccountPath+"/status", "", 200)
	after, _ := store.Get(ref)
	if before != after || driver.completes != 0 || refreshes != 0 {
		t.Fatal("status mutated or polled")
	}
	driver.pending = true
	check("POST", nativeAccountPath+"/complete", `{"callback":"private-callback"}`, 200)
	if refreshes != 0 {
		t.Fatal("pending completion refreshed runtime")
	}
	driver.pending = false
	check("POST", nativeAccountPath+"/complete", `{"callback":"private-callback"}`, 200)
	if driver.callback != "private-callback" || refreshes != 1 {
		t.Fatal("completion binding/reconciliation failed")
	}
	// A second pending login must not make DELETE preserve the old credential.
	check("POST", nativeAccountPath+"/login", "", 200)
	check("DELETE", nativeAccountPath+"/login", "", 200)
	if _, ok := store.Get(ref); ok || refreshes != 2 {
		t.Fatal("disconnect did not clear entire account and reconcile")
	}
	check("POST", nativeAccountPath+"/login", `{"fields":"invalid"}`, 400)
	check("POST", nativeAccountPath+"/complete", `{"callback":123}`, 400)
	check("GET", "/api/oauth/native/unknown/accounts/acct/status", "", 400)
	check("POST", nativeAccountPath+"/login", `{}`, 200)
	failRefresh = true
	check("POST", nativeAccountPath+"/complete", `{}`, 503)
	view, err := manager.Status("offline", "acct")
	if err != nil || view.Status != "connected" {
		t.Fatal("lost saved credential after refresh failure")
	}
	check("DELETE", nativeAccountPath+"/login", "", 503)
	if _, ok := store.Get(ref); ok {
		t.Fatal("disconnect reverted after refresh failure")
	}
}

func TestNativeOAuthUnavailableAndAccess(t *testing.T) {
	routes := []struct{ method, suffix, body string }{
		{"GET", "/status", ""}, {"POST", "/login", `{}`}, {"POST", "/complete", `{}`}, {"DELETE", "/login", ""},
	}
	for _, route := range routes {
		w := nativeCall(New(Deps{}).Routes(), route.method, nativeAccountPath+route.suffix, route.body, "")
		if w.Code != 503 {
			t.Fatalf("unavailable %s: %d", route.suffix, w.Code)
		}
	}
	security, err := controlstore.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer security.Close()
	if err := security.InitializeManagementAdministrator("admin", "unused-password-hash", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	_, low, err := security.CreateManagementToken("low", "", nil, false, "admin", "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, high, err := security.CreateManagementToken("high", "", nil, true, "admin", "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := secretstore.OpenSQLite(filepath.Join(t.TempDir(), "secrets.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer secrets.Close()
	driver := &offlineNativeDriver{}
	manager := nativeauth.NewManager(secrets, driver)
	s := New(Deps{Security: security, NativeOAuth: manager, Config: &config.File{}, RefreshRuntime: func(*config.File) error { return nil }})
	h := s.Routes()
	for _, route := range routes {
		want := 403
		if route.method == "GET" {
			want = 200
		}
		w := nativeCall(h, route.method, nativeAccountPath+route.suffix, route.body, low)
		if w.Code != want {
			t.Fatalf("low token %s: %d %s", route.method, w.Code, w.Body)
		}
	}
	if driver.begins != 0 || driver.completes != 0 {
		t.Fatal("denied writes reached driver")
	}
	if w := nativeCall(h, "GET", "/api/oauth/native/drivers", "", low); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w := nativeCall(h, "GET", nativeAccountPath+"/status", "", ""); w.Code != 401 {
		t.Fatal("missing authentication accepted")
	}
	session := httptest.NewRecorder()
	csrf, err := s.createManagementSession(session, "admin", "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range routes[1:] {
		r := httptest.NewRequest(route.method, nativeAccountPath+route.suffix, strings.NewReader(route.body))
		for _, c := range session.Result().Cookies() {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 403 || !strings.Contains(w.Body.String(), "CSRF_REJECTED") {
			t.Fatalf("missing CSRF: %d %s", w.Code, w.Body)
		}
	}
	r := httptest.NewRequest("POST", nativeAccountPath+"/login", strings.NewReader(`{}`))
	for _, c := range session.Result().Cookies() {
		r.AddCookie(c)
	}
	r.Header.Set("X-CSRF-Token", csrf)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	for _, route := range []struct{ method, suffix, body string }{{"POST", "/complete", `{"callback":"private-callback"}`}, {"DELETE", "/login", ""}} {
		w := nativeCall(h, route.method, nativeAccountPath+route.suffix, route.body, high)
		if w.Code != 200 {
			t.Fatalf("high token: %d %s", w.Code, w.Body)
		}
	}
	events, err := security.ManagementSecurityEvents(100)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(events)
	for _, secret := range []string{"private-callback", "private-access", "private-refresh", "private-verifier"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("audit leaked secret")
		}
	}
	seen := map[string]bool{}
	for _, e := range events {
		seen[e.EventType] = true
	}
	for _, event := range []string{"native_oauth_begin", "native_oauth_complete", "native_oauth_disconnect"} {
		if !seen[event] {
			t.Fatalf("missing %s audit", event)
		}
	}
	// Missing runtime wiring blocks mutation before provider completion or deletion.
	noRuntime := New(Deps{NativeOAuth: manager}).Routes()
	for _, route := range routes[2:] {
		if w := nativeCall(noRuntime, route.method, nativeAccountPath+route.suffix, route.body, ""); w.Code != 503 {
			t.Fatal(w.Body)
		}
	}
}
