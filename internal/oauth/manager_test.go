package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"ferry/internal/secretstore"
)

// testManager builds a Manager over a fresh secret store with the token
// endpoint pointed at a fake server. Returns the manager, the fake, and a
// cleanup func. The fake's response body can be swapped between calls via
// f.setResp.
type fakeAuth struct {
	mu       sync.Mutex
	form     map[string]string
	respBody string
	status   int
	calls    int
	delay    time.Duration
	srv      *http.Server
	url      string
}

func newTestManager(t *testing.T, respBody string, status int) (*Manager, *fakeAuth, func()) {
	t.Helper()
	f := &fakeAuth{form: map[string]string{}, respBody: respBody, status: status}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		for k, v := range r.Form {
			f.form[k] = v[0]
		}
		body, status := f.respBody, f.status
		delay := f.delay
		f.calls++
		f.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	srv := &http.Server{Addr: "127.0.0.1:0", Handler: mux}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.url = "http://" + ln.Addr().String() + "/oauth/token"
	go func() { _ = srv.Serve(ln) }()

	store, err := secretstore.Open(t.TempDir() + "/secrets.json")
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(store)
	orig := tokenURL
	tokenURL = f.url
	return m, f, func() {
		tokenURL = orig
		_ = srv.Close()
	}
}

// swapResp replaces the fake's canned response (for expiry/refresh tests).
func (f *fakeAuth) swapResp(body string, status int) {
	f.mu.Lock()
	f.respBody, f.status = body, status
	f.mu.Unlock()
}

func (f *fakeAuth) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fullLogin drives StartLogin → CompleteLogin to a stored token.
func fullLogin(t *testing.T, m *Manager, account string) {
	t.Helper()
	loginURL, err := m.StartLogin(account)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(loginURL, "state=") {
		t.Fatalf("login URL missing state: %s", loginURL)
	}
	m.mu.Lock()
	p := m.pending[account]
	m.mu.Unlock()
	if p == nil {
		t.Fatal("pending login not recorded")
	}
	if err := m.CompleteLogin(account, p.State, "the-code"); err != nil {
		t.Fatal(err)
	}
}

func TestLoginLifecycle(t *testing.T) {
	m, f, done := newTestManager(t, tokenRespJSON, http.StatusOK)
	defer done()

	fullLogin(t, m, "work")
	if got := f.form["grant_type"]; got != "authorization_code" {
		t.Fatalf("grant_type = %q", got)
	}

	// token is usable immediately
	at, err := m.AccessToken(context.Background(), "work")
	if err != nil || at != "at-123" {
		t.Fatalf("AccessToken = %q, %v", at, err)
	}

	// redacted view: identity only, no token material
	accts := m.Accounts()
	if len(accts) != 1 || accts[0].Name != "work" || accts[0].PlanType != "pro" ||
		accts[0].Email != "user@x.com" || accts[0].AccountID != "acct-9" {
		t.Fatalf("Accounts = %+v", accts)
	}
	if accts[0].Expired {
		t.Fatal("fresh token must not be expired")
	}
	raw, _ := json.Marshal(accts)
	for _, leak := range []string{"at-123", "rt-456"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("account view leaks token material %s", leak)
		}
	}

	// persisted under the reserved store name
	if v, ok := m.store.Get(storeKey("work")); !ok || !strings.Contains(v, "at-123") {
		t.Fatal("token not persisted to store")
	}
}

func TestCompleteLoginRejects(t *testing.T) {
	m, _, done := newTestManager(t, tokenRespJSON, http.StatusOK)
	defer done()

	if err := m.CompleteLogin("work", "any", "code"); err == nil {
		t.Fatal("complete without a started login must fail")
	}

	if _, err := m.StartLogin("work"); err != nil {
		t.Fatal(err)
	}
	if err := m.CompleteLogin("work", "wrong-state", "code"); err == nil {
		t.Fatal("state mismatch must fail")
	}
	// the failed attempt must not have stored anything
	if _, ok := m.store.Get(storeKey("work")); ok {
		t.Fatal("rejected login persisted a token")
	}
}

func TestPendingLoginExpiry(t *testing.T) {
	m, _, done := newTestManager(t, tokenRespJSON, http.StatusOK)
	defer done()

	if _, err := m.StartLogin("work"); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	p := m.pending["work"]
	m.now = func() time.Time { return p.Started.Add(16 * time.Minute) }
	state := p.State
	m.mu.Unlock()

	if err := m.CompleteLogin("work", state, "code"); err == nil {
		t.Fatal("expired pending login must fail")
	}
	m.mu.Lock()
	_, still := m.pending["work"]
	m.now = time.Now
	m.mu.Unlock()
	if still {
		t.Fatal("expired pending login not cleaned up")
	}
}

func TestPersistenceReload(t *testing.T) {
	m, _, done := newTestManager(t, tokenRespJSON, http.StatusOK)
	defer done()
	fullLogin(t, m, "work")

	// a second manager over the same store sees the account
	m2 := NewManager(m.store)
	accts := m2.Accounts()
	if len(accts) != 1 || accts[0].Name != "work" {
		t.Fatalf("reloaded accounts = %+v", accts)
	}
	at, err := m2.AccessToken(context.Background(), "work")
	if err != nil || at != "at-123" {
		t.Fatalf("reloaded AccessToken = %q, %v", at, err)
	}
}

func TestLazyRefreshSingleFlight(t *testing.T) {
	m, f, done := newTestManager(t, tokenRespJSON, http.StatusOK)
	defer done()
	fullLogin(t, m, "work")

	// age the cached token into the refresh margin
	m.mu.Lock()
	old := *m.tokens["work"]
	old.ExpiresAt = m.now().Add(time.Minute)
	m.tokens["work"] = &old
	m.mu.Unlock()

	f.swapResp(`{"access_token":"at-new","refresh_token":"rt-new","expires_in":3600}`, http.StatusOK)
	f.mu.Lock()
	f.delay = 50 * time.Millisecond
	beforeCalls := f.calls
	f.mu.Unlock()

	var wg sync.WaitGroup
	results := make([]string, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			at, err := m.AccessToken(context.Background(), "work")
			if err != nil {
				t.Errorf("concurrent AccessToken: %v", err)
				return
			}
			results[i] = at
		}(i)
	}
	wg.Wait()
	if got := f.callCount() - beforeCalls; got != 1 {
		t.Fatalf("concurrent refresh made %d upstream calls, want 1", got)
	}

	for i, at := range results {
		if at != "at-new" {
			t.Fatalf("call %d got %q, want refreshed at-new", i, at)
		}
	}
	// display claims survive a refresh response that omits id_token
	accts := m.Accounts()
	if accts[0].PlanType != "pro" || accts[0].AccountID != "acct-9" {
		t.Fatalf("claims lost on refresh: %+v", accts[0])
	}
	// new refresh token persisted
	if v, _ := m.store.Get(storeKey("work")); !strings.Contains(v, "rt-new") {
		t.Fatal("refreshed token not persisted")
	}
}

func TestRefreshPreservesOmittedRefreshToken(t *testing.T) {
	m, f, done := newTestManager(t, tokenRespJSON, http.StatusOK)
	defer done()
	fullLogin(t, m, "work")
	m.mu.Lock()
	old := *m.tokens["work"]
	old.ExpiresAt = m.now().Add(time.Minute)
	m.tokens["work"] = &old
	m.mu.Unlock()
	f.swapResp(`{"access_token":"at-new","expires_in":3600}`, http.StatusOK)
	if token, err := m.AccessToken(context.Background(), "work"); err != nil || token != "at-new" {
		t.Fatalf("token=%q err=%v", token, err)
	}
	stored, _ := m.store.Get(storeKey("work"))
	if !strings.Contains(stored, `"refresh_token":"rt-456"`) {
		t.Fatalf("rotating response erased prior refresh token: %s", stored)
	}
}

func TestRefreshIdentityReusesConcurrentReplacement(t *testing.T) {
	m, f, done := newTestManager(t, tokenRespJSON, http.StatusOK)
	defer done()
	fullLogin(t, m, "work")
	f.swapResp(`{"access_token":"at-new","refresh_token":"rt-new","expires_in":3600}`, http.StatusOK)
	access, _, err := m.RefreshIdentity(context.Background(), "work", "at-123")
	if err != nil || access != "at-new" {
		t.Fatalf("first refresh=%q err=%v", access, err)
	}
	before := f.callCount()
	access, _, err = m.RefreshIdentity(context.Background(), "work", "at-123")
	if err != nil || access != "at-new" || f.callCount() != before {
		t.Fatalf("replacement not reused: access=%q err=%v calls=%d->%d", access, err, before, f.callCount())
	}
}

func TestRefreshFailureSurfaces(t *testing.T) {
	m, f, done := newTestManager(t, tokenRespJSON, http.StatusOK)
	defer done()
	fullLogin(t, m, "work")

	m.mu.Lock()
	old := *m.tokens["work"]
	old.ExpiresAt = m.now().Add(time.Minute)
	m.tokens["work"] = &old
	m.mu.Unlock()

	f.swapResp(`{"error":"invalid_grant"}`, http.StatusBadRequest)
	before := f.callCount()
	if _, err := m.AccessToken(context.Background(), "work"); err == nil {
		t.Fatal("refresh failure must surface")
	}
	if got := f.callCount() - before; got != 1 {
		t.Fatalf("non-retryable invalid_grant attempted %d times", got)
	}
}

func TestLogoutWinsOverInflightRefresh(t *testing.T) {
	m, f, done := newTestManager(t, tokenRespJSON, http.StatusOK)
	defer done()
	fullLogin(t, m, "work")
	m.mu.Lock()
	old := *m.tokens["work"]
	old.ExpiresAt = m.now().Add(time.Minute)
	m.tokens["work"] = &old
	m.mu.Unlock()
	f.swapResp(`{"access_token":"at-new","refresh_token":"rt-new","expires_in":3600}`, http.StatusOK)
	f.mu.Lock()
	f.delay = 75 * time.Millisecond
	before := f.calls
	f.mu.Unlock()
	result := make(chan error, 1)
	go func() {
		_, err := m.AccessToken(context.Background(), "work")
		result <- err
	}()
	waitForOAuth(t, time.Second, func() bool { return f.callCount() > before })
	if err := m.Logout("work"); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err == nil || !strings.Contains(err.Error(), "logged out") {
		t.Fatalf("in-flight refresh result=%v", err)
	}
	if _, active := m.Account("work"); active {
		t.Fatal("in-flight refresh resurrected logged-out account")
	}
	if _, persisted := m.store.Get(storeKey("work")); persisted {
		t.Fatal("in-flight refresh resurrected persisted token")
	}
}

func TestSupersededBrowserLoginCannotOverwriteNewAttempt(t *testing.T) {
	m, f, done := newTestManager(t, tokenRespJSON, http.StatusOK)
	defer done()
	if _, err := m.StartLogin("work"); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	oldState := m.pending["work"].State
	m.mu.Unlock()
	f.mu.Lock()
	f.delay = 75 * time.Millisecond
	before := f.calls
	f.mu.Unlock()
	result := make(chan error, 1)
	go func() { result <- m.CompleteLogin("work", oldState, "old-code") }()
	waitForOAuth(t, time.Second, func() bool { return f.callCount() > before })
	if _, err := m.StartLogin("work"); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err == nil || !strings.Contains(err.Error(), "superseded") {
		t.Fatalf("old login result=%v", err)
	}
	if _, active := m.Account("work"); active {
		t.Fatal("superseded login activated an account")
	}
	status, pending := m.LoginStatus("work")
	if !pending || status.Status != loginStatusPending {
		t.Fatalf("new pending login was lost: pending=%v status=%+v", pending, status)
	}
}

func TestNotLoggedInError(t *testing.T) {
	m, _, done := newTestManager(t, tokenRespJSON, http.StatusOK)
	defer done()
	if _, err := m.AccessToken(context.Background(), "ghost"); err == nil {
		t.Fatal("unknown account must fail")
	}
}

func TestLogout(t *testing.T) {
	m, _, done := newTestManager(t, tokenRespJSON, http.StatusOK)
	defer done()
	fullLogin(t, m, "work")

	if err := m.Logout("work"); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.store.Get(storeKey("work")); ok {
		t.Fatal("logout left the token in the store")
	}
	if len(m.Accounts()) != 0 {
		t.Fatal("logout left the account in memory")
	}
	if _, err := m.AccessToken(context.Background(), "work"); err == nil {
		t.Fatal("token still usable after logout")
	}
}

func TestValidAccountNames(t *testing.T) {
	for _, ok := range []string{"a", "work", "Work_2", "x-9_y"} {
		if err := validAccount(ok); err != nil {
			t.Errorf("validAccount(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-lead", "_lead", "has.dot", "has space", strings.Repeat("x", 65)} {
		if err := validAccount(bad); err == nil {
			t.Errorf("validAccount(%q) accepted", bad)
		}
	}
	store, err := secretstore.Open(t.TempDir() + "/secrets.json")
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(store)
	if _, err := m.StartLogin("bad.name"); err == nil {
		t.Fatal("StartLogin accepted an invalid account name")
	}
}

func TestRefreshServerErrorsAreNotReplayed(t *testing.T) {
	m, f, done := newTestManager(t, tokenRespJSON, http.StatusOK)
	defer done()
	fullLogin(t, m, "work")
	m.mu.Lock()
	old := *m.tokens["work"]
	old.ExpiresAt = m.now().Add(time.Minute)
	m.tokens["work"] = &old
	m.mu.Unlock()

	f.swapResp(`{"error":"server_error"}`, http.StatusInternalServerError)
	before := f.callCount()
	if _, err := m.AccessToken(context.Background(), "work"); err == nil {
		t.Fatal("5xx refresh failure must surface")
	}
	if got := f.callCount() - before; got != 1 {
		t.Fatalf("ambiguous 5xx refresh attempted %d times; a replayed grant may already be rotated", got)
	}
}

func TestRetryableRefreshTransportError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "dns failure",
			err:  &url.Error{Op: "Post", URL: tokenURL, Err: &net.DNSError{Err: "no such host", Name: "auth.openai.com", IsNotFound: true}},
			want: true,
		},
		{
			name: "connection refused",
			err:  &url.Error{Op: "Post", URL: tokenURL, Err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}},
			want: true,
		},
		{
			name: "request timeout may have been processed",
			err:  &url.Error{Op: "Post", URL: tokenURL, Err: context.DeadlineExceeded},
			want: false,
		},
		{
			name: "mid-response reset",
			err:  &url.Error{Op: "Post", URL: tokenURL, Err: &net.OpError{Op: "read", Net: "tcp", Err: io.ErrUnexpectedEOF}},
			want: false,
		},
		{
			name: "not a transport error",
			err:  errors.New("boom"),
			want: false,
		},
	}
	for _, tc := range cases {
		if got := retryableRefreshTransportError(tc.err); got != tc.want {
			t.Errorf("%s: retryable=%v want %v", tc.name, got, tc.want)
		}
	}
}

func TestRefreshKeepsRotatedTokenWhenPersistFails(t *testing.T) {
	m, f, done := newTestManager(t, tokenRespJSON, http.StatusOK)
	defer done()
	fullLogin(t, m, "work")
	m.mu.Lock()
	old := *m.tokens["work"]
	old.ExpiresAt = m.now().Add(time.Minute)
	m.tokens["work"] = &old
	m.mu.Unlock()
	f.swapResp(`{"access_token":"at-new","refresh_token":"rt-new","expires_in":3600}`, http.StatusOK)

	if err := m.store.Close(); err != nil {
		t.Fatal(err)
	}
	at, err := m.AccessToken(context.Background(), "work")
	if err != nil || at != "at-new" {
		t.Fatalf("refresh after persist failure: token=%q err=%v", at, err)
	}
	m.mu.Lock()
	rotated := m.tokens["work"].RefreshToken
	m.mu.Unlock()
	if rotated != "rt-new" {
		t.Fatalf("rotated refresh token dropped from memory: %q", rotated)
	}
}
