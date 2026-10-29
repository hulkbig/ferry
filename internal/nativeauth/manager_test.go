package nativeauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ferry/internal/secretstore"
)

type managerDriver struct{ refreshes atomic.Int32 }

func (d *managerDriver) Info() Info {
	return Info{ID: "fixture", Name: "Fixture", Mode: "device", BaseURL: "https://native.example/v1", Protocol: "openai-chat"}
}
func (d *managerDriver) Begin(_ context.Context, _ *http.Client, o LoginOptions) (Session, error) {
	return Session{State: o.State, Verifier: o.Verifier, DeviceCode: "secret-device", URL: "https://login.example", UserCode: "visible", ExpiresAt: time.Now().Add(time.Minute)}, nil
}
func (d *managerDriver) Complete(context.Context, *http.Client, Session, string) (Token, error) {
	return Token{AccessToken: "secret-access", RefreshToken: "secret-refresh", ExpiresAt: time.Now().Add(-time.Second)}, nil
}
func (d *managerDriver) Refresh(_ context.Context, _ *http.Client, t Token) (Token, error) {
	d.refreshes.Add(1)
	time.Sleep(time.Millisecond)
	t.AccessToken = "rotated-access"
	t.RefreshToken = "rotated-refresh"
	t.ExpiresAt = time.Now().Add(time.Hour)
	return t, nil
}
func testManager(t *testing.T) (*Manager, *managerDriver) {
	t.Helper()
	store, e := secretstore.Open(t.TempDir() + "/secrets.json")
	if e != nil {
		t.Fatal(e)
	}
	d := &managerDriver{}
	return NewManager(store, d), d
}
func TestManagerPersistsAndSerializesRotation(t *testing.T) {
	m, d := testManager(t)
	ctx := context.Background()
	v, e := m.Begin(ctx, "fixture", "a", nil)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(v)
	if strings.Contains(string(raw), "secret-device") || strings.Contains(string(raw), "verifier") {
		t.Fatal("public view exposes session secret")
	}
	if _, e = m.Complete(ctx, "fixture", "a", ""); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, e := m.Identity(ctx, Ref("fixture", "a"), "secret-access")
			if e != nil || token.AccessToken != "rotated-access" {
				t.Errorf("rotation failed: %v", e)
			}
		}()
	}
	wg.Wait()
	if d.refreshes.Load() != 1 {
		t.Fatal("rotating refresh replayed")
	}
	restarted := NewManager(m.store, d)
	token, e := restarted.Identity(ctx, Ref("fixture", "a"), "")
	if e != nil || token.RefreshToken != "rotated-refresh" {
		t.Fatal("rotation was not durable")
	}
	v, e = restarted.Status("fixture", "a")
	raw, _ = json.Marshal(v)
	if e != nil || strings.Contains(string(raw), "rotated") {
		t.Fatal("status exposes credential")
	}
	if e = restarted.Disconnect("fixture", "a"); e != nil {
		t.Fatal(e)
	}
	if _, e = restarted.Identity(ctx, Ref("fixture", "a"), ""); e == nil {
		t.Fatal("disconnected token reused")
	}
}

type managerTransport func(*http.Request) (*http.Response, error)

func (f managerTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestNativeTransportNeverLeaksToOtherEndpoint(t *testing.T) {
	m, _ := testManager(t)
	_, _ = m.Begin(context.Background(), "fixture", "a", nil)
	_, _ = m.Complete(context.Background(), "fixture", "a", "")
	calls := 0
	client := m.ClientFor(Ref("fixture", "a"), &http.Client{Transport: managerTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Authorization") != "Bearer rotated-access" || r.Header.Get("X-Api-Key") != "" {
			t.Fatal("incorrect auth")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}}, nil
	})})
	for _, endpoint := range []string{"https://evil.example/v1/chat/completions", "http://native.example/v1/chat/completions", "https://native.example.evil/v1/chat/completions"} {
		req, _ := http.NewRequest("POST", endpoint, strings.NewReader("{}"))
		if _, e := client.Do(req); e == nil {
			t.Fatal("accepted foreign endpoint")
		}
	}
	req, _ := http.NewRequest("POST", "https://native.example/v1/chat/completions", strings.NewReader("{}"))
	resp, e := client.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	_ = resp.Body.Close()
	if calls != 1 {
		t.Fatal("foreign requests reached transport")
	}
	if m.ValidateBinding(Ref("fixture", "a"), "https://native.example/v1", "anthropic") == nil {
		t.Fatal("wrong protocol accepted")
	}
	if m.ValidateBinding(Ref("fixture", "a"), "https://native.example/v1?key=x", "openai-chat") == nil {
		t.Fatal("query override accepted")
	}
}
func TestNativeTransportRetriesOnlyRejectedCredential(t *testing.T) {
	m, d := testManager(t)
	ref := Ref("fixture", "a")
	_ = m.save(ref, accountRecord{Token: &Token{AccessToken: "initial", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour)}})
	calls := 0
	client := m.ClientFor(ref, &http.Client{Transport: managerTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		body, _ := io.ReadAll(r.Body)
		if string(body) != "original" {
			t.Fatal("body not replayed")
		}
		code := 401
		if calls == 2 {
			code = 200
			if r.Header.Get("Authorization") != "Bearer rotated-access" {
				t.Fatal("stale retry")
			}
		}
		return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}}, nil
	})})
	req, _ := http.NewRequest("POST", "https://native.example/v1/chat/completions", strings.NewReader("original"))
	resp, e := client.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || calls != 2 || d.refreshes.Load() != 1 {
		t.Fatal(fmt.Sprint("bad retry", calls))
	}
}

func TestManagerVertexTransientRefreshRemainsRetryable(t *testing.T) {
	for _, legacyJournal := range []bool{false, true} {
		t.Run(fmt.Sprint("legacy_journal=", legacyJournal), func(t *testing.T) {
			m, _ := testManager(t)
			driver := NewVertexDriver()
			m.drivers = map[string]Driver{"vertex": driver}
			if !safeNonRotatingRefresh(driver) {
				t.Fatal("Vertex did not declare nonrotating refresh")
			}
			session := vertexTestSession(t, "global")
			ref := Ref("vertex", "a")
			original := accountRecord{BindingID: "existing-binding", RefreshInFlight: legacyJournal, Token: &Token{AccessToken: "expired-access", ExpiresAt: time.Now().Add(-time.Hour), Extra: session.Extra}}
			if e := m.save(ref, original); e != nil {
				t.Fatal(e)
			}
			calls := 0
			m.client = &http.Client{Transport: managerTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.String() != vertexExchangeEndpoint {
					t.Fatal(r.URL)
				}
				record, e := m.read(ref)
				if e != nil || record.RefreshInFlight {
					t.Fatal("nonrotating request was journaled", e)
				}
				if calls == 1 {
					return vertexTestResponse(503, `{"error":"temporarily_unavailable"}`), nil
				}
				return vertexTestResponse(200, `{"access_token":"renewed-access","token_type":"Bearer","expires_in":3600}`), nil
			})}
			if _, e := m.Identity(context.Background(), ref, ""); e == nil || errors.Is(e, ErrLoginRequired) {
				t.Fatalf("transient failure demanded login: %v", e)
			}
			stored, e := m.read(ref)
			if e != nil || stored.RefreshInFlight || stored.Token.AccessToken != "expired-access" || stored.BindingID != "existing-binding" {
				t.Fatal("old credentials/journal corrupted", e)
			}
			if _, e := m.Identity(context.Background(), ref, ""); e == nil || errors.Is(e, ErrLoginRequired) || calls != 1 {
				t.Fatal("temporary backoff not respected", e, calls)
			}
			// Simulate expiry without sleeps; the account never needs a new login.
			m.blocked.Store(ref, time.Now().Add(-time.Second))
			token, e := m.Identity(context.Background(), ref, "")
			if e != nil || token.AccessToken != "renewed-access" || token.BindingID != "existing-binding" {
				t.Fatal("retry failed", e)
			}
			if _, ok := m.blocked.Load(ref); ok {
				t.Fatal("successful refresh retained blocked state")
			}
			restarted := NewManager(m.store, driver)
			token, e = restarted.Identity(context.Background(), ref, "")
			if e != nil || token.AccessToken != "renewed-access" {
				t.Fatal("renewal not durable", e)
			}
		})
	}
}

type managerReviewDriver struct {
	id       string
	refresh  func(context.Context, Token) (Token, error)
	complete func() (Token, error)
}

func (d *managerReviewDriver) Info() Info {
	return Info{ID: d.id, Mode: "browser", BaseURL: "https://fixture.example", Protocol: "openai-chat"}
}
func (d *managerReviewDriver) Begin(_ context.Context, _ *http.Client, o LoginOptions) (Session, error) {
	return Session{State: o.State, Verifier: o.Verifier, ExpiresAt: time.Now().Add(time.Hour)}, nil
}
func (d *managerReviewDriver) Complete(context.Context, *http.Client, Session, string) (Token, error) {
	return d.complete()
}
func (d *managerReviewDriver) Refresh(ctx context.Context, _ *http.Client, t Token) (Token, error) {
	return d.refresh(ctx, t)
}

func TestManagerRotatingUncertainRefreshStaysClosedAcrossRestart(t *testing.T) {
	m, _ := testManager(t)
	ref := Ref("rotating", "a")
	calls := 0
	d := &managerReviewDriver{id: "rotating", refresh: func(context.Context, Token) (Token, error) { calls++; return Token{}, errors.New("uncertain exchange") }}
	m.drivers = map[string]Driver{"rotating": d}
	if safeNonRotatingRefresh(d) {
		t.Fatal("default driver assumed retry-safe")
	}
	if e := m.save(ref, accountRecord{BindingID: "binding", Token: &Token{AccessToken: "old", RefreshToken: "one-use", ExpiresAt: time.Now().Add(-time.Hour)}}); e != nil {
		t.Fatal(e)
	}
	if _, e := m.Identity(context.Background(), ref, ""); !errors.Is(e, ErrLoginRequired) {
		t.Fatal(e)
	}
	r, e := m.read(ref)
	if e != nil || !r.RefreshInFlight {
		t.Fatal("uncertain rotating refresh journal cleared", e)
	}
	m.blocked.Store(ref, time.Now().Add(-time.Hour))
	for _, manager := range []*Manager{m, NewManager(m.store, d)} {
		if _, e := manager.Identity(context.Background(), ref, ""); !errors.Is(e, ErrLoginRequired) {
			t.Fatal(e)
		}
	}
	if calls != 1 {
		t.Fatal("one-use token replayed", calls)
	}
	// A token-only partial record must not accidentally enter Meta's deliberate
	// key issuance recovery path for another rotating provider.
	r.Token.AccessToken = ""
	if e = m.save(ref, r); e != nil {
		t.Fatal(e)
	}
	if _, e = m.Complete(context.Background(), "rotating", "a", ""); e == nil || calls != 1 {
		t.Fatal("generic partial record replayed refresh", e, calls)
	}
}

func TestManagerMetaPartialLoginAndExplicitKeyRecovery(t *testing.T) {
	m, _ := testManager(t)
	d := NewMetaDriver()
	m.drivers = map[string]Driver{"meta": d}
	ref := Ref("meta", "a")
	if safeNonRotatingRefresh(d) {
		t.Fatal("Meta mint must not opt into automatic retry")
	}
	stale := accountRecord{RefreshInFlight: true, BindingID: "old-binding", Token: &Token{AccessToken: "old-key", RefreshToken: "old-dca"}}
	if e := m.save(ref, stale); e != nil {
		t.Fatal(e)
	}
	m.blocked.Store(ref, time.Now().Add(time.Hour))
	mintCalls, deviceCalls := 0, 0
	m.client = &http.Client{Transport: managerTransport(func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case metaDeviceURL:
			return vertexTestResponse(200, `{"device_code":"new-device","user_code":"USER","verification_uri":"https://auth.meta.com/device","expires_in":600,"interval":1}`), nil
		case metaExchangeEndpoint:
			deviceCalls++
			return vertexTestResponse(200, `{"access_token":"new-dca","token_type":"Bearer","expires_in":3600}`), nil
		case metaMintURL:
			mintCalls++
			if r.Header.Get("Authorization") != "Bearer new-dca" {
				t.Fatal("old DCA reused")
			}
			if mintCalls < 3 {
				return vertexTestResponse(503, `{"error":"temporarily_unavailable"}`), nil
			}
			return vertexTestResponse(200, `{"api_key":"new-key","base_url":"https://api.meta.ai/v1","user_email":"fixture@example.test"}`), nil
		default:
			t.Fatal(r.URL)
			return nil, nil
		}
	})}
	if _, e := m.Begin(context.Background(), "meta", "a", nil); e != nil {
		t.Fatal(e)
	}
	r, e := m.read(ref)
	if e != nil {
		t.Fatal(e)
	}
	// Starting an uncompleted login is not authority to unlock the old credential.
	if !r.RefreshInFlight {
		t.Fatal("Begin unlocked old uncertain credential")
	}
	if _, ok := m.blocked.Load(ref); !ok {
		t.Fatal("Begin cleared old backoff")
	}
	r.NextPoll = time.Time{}
	if e = m.save(ref, r); e != nil {
		t.Fatal(e)
	}
	v, e := m.Complete(context.Background(), "meta", "a", "")
	if e != nil || v.Status != "key_pending" {
		t.Fatal("partial login not recoverable", v, e)
	}
	r, e = m.read(ref)
	if e != nil || r.Token.RefreshToken != "new-dca" || r.RefreshInFlight || r.BindingID != "" || r.Session != nil || !r.NextPoll.IsZero() {
		t.Fatal("partial login retained stale state", e)
	}
	if _, ok := m.blocked.Load(ref); ok {
		t.Fatal("partial login retained old blocked map")
	}
	if _, e = m.Identity(context.Background(), ref, ""); !errors.Is(e, ErrLoginRequired) || mintCalls != 1 {
		t.Fatal("partial key minted automatically", e, mintCalls)
	}
	if _, e = m.Complete(context.Background(), "meta", "a", ""); e == nil {
		t.Fatal("failed mint claimed success")
	}
	v, e = m.Status("meta", "a")
	if e != nil || v.Status != "key_pending" {
		t.Fatal("explicit failure lost recoverability", v, e)
	}
	// Also recover persisted pending records carrying a stale legacy journal.
	r, _ = m.read(ref)
	r.RefreshInFlight = true
	r.BindingID = "stale-partial-binding"
	if e = m.save(ref, r); e != nil {
		t.Fatal(e)
	}
	m.blocked.Store(ref, time.Now().Add(time.Hour))
	v, e = m.Complete(context.Background(), "meta", "a", "")
	if e != nil || v.Status != "connected" {
		t.Fatal("mint completion still locked", v, e)
	}
	r, e = m.read(ref)
	if e != nil || r.RefreshInFlight || r.BindingID == "" || r.BindingID == "stale-partial-binding" || r.Token.AccessToken != "new-key" || r.Token.RefreshToken != "new-dca" || r.Session != nil || !r.NextPoll.IsZero() {
		t.Fatal("bad successful binding", e)
	}
	if _, ok := m.blocked.Load(ref); ok {
		t.Fatal("mint success retained backoff")
	}
	restarted := NewManager(m.store, d)
	token, e := restarted.Identity(context.Background(), ref, "")
	if e != nil || token.AccessToken != "new-key" || token.BindingID != r.BindingID {
		t.Fatal("mint success not durable", e)
	}
	if deviceCalls != 1 || mintCalls != 3 {
		t.Fatal("device grant replayed or unexpected mint", deviceCalls, mintCalls)
	}
}

func TestManagerMetaRecoveryDoesNotClearBlockBeforePersistence(t *testing.T) {
	for _, partialLogin := range []bool{false, true} {
		t.Run(fmt.Sprint("partial_login=", partialLogin), func(t *testing.T) {
			path := t.TempDir() + "/secret.db"
			store, e := secretstore.OpenSQLite(path)
			if e != nil {
				t.Fatal(e)
			}
			defer store.Close()
			d := &managerReviewDriver{id: "meta"}
			m := NewManager(store, d)
			ref := Ref("meta", "a")
			record := accountRecord{RefreshInFlight: true, BindingID: "old", Token: &Token{RefreshToken: "existing-dca"}}
			if partialLogin {
				record.Session = &Session{ExpiresAt: time.Now().Add(time.Hour)}
			}
			if e = m.save(ref, record); e != nil {
				t.Fatal(e)
			}
			m.blocked.Store(ref, time.Now().Add(time.Hour))
			failStore := func() {
				if e := store.Close(); e != nil {
					t.Fatal(e)
				}
			}
			d.refresh = func(context.Context, Token) (Token, error) {
				failStore()
				return Token{AccessToken: "minted", RefreshToken: "existing-dca"}, nil
			}
			d.complete = func() (Token, error) { failStore(); return Token{RefreshToken: "new-dca"}, errors.New("mint failed") }
			if _, e = m.Complete(context.Background(), "meta", "a", ""); e == nil {
				t.Fatal("failed persistence claimed completion")
			}
			if _, ok := m.blocked.Load(ref); !ok {
				t.Fatal("block removed before durable save")
			}
			reopened, e := secretstore.OpenSQLite(path)
			if e != nil {
				t.Fatal(e)
			}
			defer reopened.Close()
			r, e := NewManager(reopened, d).read(ref)
			if e != nil || !r.RefreshInFlight || r.BindingID != "old" || r.Token.RefreshToken != "existing-dca" {
				t.Fatal("failed save changed durable state", e)
			}
		})
	}
}
