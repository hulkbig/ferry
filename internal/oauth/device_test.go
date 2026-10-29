package oauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ferry/internal/secretstore"
)

func deviceAuthFixture(t *testing.T, pollStatus int) (*httptest.Server, func()) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/device/usercode", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"device_auth_id":"device-1","user_code":"ABCD-EFGH","interval":"1"}`))
	})
	mux.HandleFunc("/device/token", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(pollStatus)
		if pollStatus >= 200 && pollStatus < 300 {
			_, _ = w.Write([]byte(`{"authorization_code":"device-code","code_verifier":"device-verifier","code_challenge":"device-challenge"}`))
		}
	})
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("redirect_uri") != DeviceRedirectURI || r.Form.Get("code_verifier") != "device-verifier" {
			http.Error(w, "wrong device exchange", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(tokenRespJSON))
	})
	server := httptest.NewServer(mux)
	restoreDevice := SetDeviceEndpoints(server.URL+"/device/usercode", server.URL+"/device/token")
	oldTokenURL := tokenURL
	tokenURL = server.URL + "/oauth/token"
	return server, func() {
		tokenURL = oldTokenURL
		restoreDevice()
		server.Close()
	}
}

func TestDeviceLoginCompletesAsynchronously(t *testing.T) {
	_, cleanup := deviceAuthFixture(t, http.StatusOK)
	defer cleanup()
	store, err := secretstore.Open(t.TempDir() + "/secrets.json")
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store)
	var changes atomic.Int32
	manager.SetChangeHandler(func(string) { changes.Add(1) })
	status, err := manager.StartDeviceLogin(context.Background(), "work")
	if err != nil {
		t.Fatal(err)
	}
	if status.Mode != loginModeDevice || status.UserCode != "ABCD-EFGH" || status.VerificationURL != DeviceVerificationURL {
		t.Fatalf("status=%+v", status)
	}
	waitForOAuth(t, time.Second, func() bool {
		account, ok := manager.Account("work")
		return ok && account.PlanType == "pro"
	})
	if changes.Load() != 1 {
		t.Fatalf("change notifications=%d", changes.Load())
	}
	if _, ok := manager.LoginStatus("work"); ok {
		t.Fatal("completed device login remained pending")
	}
}

func TestDeviceLoginCanBeCancelled(t *testing.T) {
	_, cleanup := deviceAuthFixture(t, http.StatusForbidden)
	defer cleanup()
	store, err := secretstore.Open(t.TempDir() + "/secrets.json")
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store)
	if _, err := manager.StartDeviceLogin(context.Background(), "work"); err != nil {
		t.Fatal(err)
	}
	if err := manager.CancelLogin("work"); err != nil {
		t.Fatal(err)
	}
	if _, ok := manager.LoginStatus("work"); ok {
		t.Fatal("cancelled login remained visible")
	}
	if _, ok := store.Get(pendingStoreKey("work")); ok {
		t.Fatal("cancelled login remained persisted")
	}
}

func TestPersistedDeviceLoginResumes(t *testing.T) {
	_, cleanup := deviceAuthFixture(t, http.StatusOK)
	defer cleanup()
	store, err := secretstore.Open(t.TempDir() + "/secrets.json")
	if err != nil {
		t.Fatal(err)
	}
	pending := `{"mode":"device","device_auth_id":"device-1","user_code":"ABCD-EFGH","verification_url":"` +
		DeviceVerificationURL + `","interval_seconds":1,"status":"polling","started":"` + time.Now().Format(time.RFC3339Nano) + `"}`
	if err := store.Set(pendingStoreKey("work"), pending); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store)
	waitForOAuth(t, time.Second, func() bool {
		_, ok := manager.Account("work")
		return ok
	})
}

func TestLoopbackCallbackCompletesMatchingState(t *testing.T) {
	fake, done := newFakeTokenServer(t, http.StatusOK, tokenRespJSON)
	defer done()
	oldTokenURL := tokenURL
	tokenURL = fake.srv.URL + "/oauth/token"
	defer func() { tokenURL = oldTokenURL }()
	store, err := secretstore.Open(t.TempDir() + "/secrets.json")
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store)
	authorizeURL, err := manager.StartLogin("work")
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(authorizeURL)
	request := httptest.NewRequest(http.MethodGet, "/auth/callback?code=the-code&state="+url.QueryEscape(parsed.Query().Get("state")), nil)
	recorder := httptest.NewRecorder()
	manager.handleLoopbackCallback(recorder, request)
	if recorder.Code != http.StatusFound || !strings.Contains(recorder.Header().Get("Location"), "/success") {
		t.Fatalf("callback=%d headers=%v body=%s", recorder.Code, recorder.Header(), recorder.Body.String())
	}
	if _, ok := manager.Account("work"); !ok {
		t.Fatal("loopback callback did not activate account")
	}
}

func waitForOAuth(t *testing.T, timeout time.Duration, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for OAuth state")
}
