package oauth

// These checks use a closed, in-memory RoundTripper. No socket, production
// credential, or real reset endpoint is ever used, including on unexpected calls.
import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"ferry/internal/secretstore"
)

type resetFixture struct {
	t           *testing.T
	mu          sync.Mutex
	posts       []map[string]string
	accounts    []string
	response    string
	fail        bool
	detailError bool
	usage       *UsageSnapshot
	credits     *UsageResetCredits
	beforePost  func()
}

func (f *resetFixture) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.URL.Host != "reset-fixture.invalid" || req.Header.Get("Authorization") == "" || req.Header.Get("ChatGPT-Account-Id") != "acct-a" {
		return nil, errors.New("unexpected network destination or identity; no socket opened")
	}
	var payload any
	switch req.Method + " " + req.URL.Path {
	case "GET /usage":
		payload = f.usage
	case "GET /rate-limit-reset-credits":
		if f.detailError {
			return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("private diagnostic")), Header: make(http.Header)}, nil
		}
		payload = f.credits
	case "POST /rate-limit-reset-credits/consume":
		var body map[string]string
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		f.posts = append(f.posts, body)
		f.accounts = append(f.accounts, req.Header.Get("ChatGPT-Account-Id"))
		if f.beforePost != nil {
			f.beforePost()
		}
		if f.fail {
			return nil, errors.New("simulated lost response")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(f.response)), Header: make(http.Header)}, nil
	default:
		return nil, errors.New("unexpected method or path; no socket opened")
	}
	raw, _ := json.Marshal(payload)
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(raw))), Header: make(http.Header)}, nil
}
func resetTestManager(t *testing.T) (*Manager, *resetFixture, UsageResetBinding) {
	t.Helper()
	store, err := secretstore.OpenSQLite(t.TempDir() + "/control.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	m := NewManager(store)
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	token := &Token{AccessToken: "fake." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"person-a"}`)) + ".fake", RefreshToken: "offline-refresh-token", AccountID: "acct-a", Email: "a@example.invalid", ExpiresAt: now.Add(24 * time.Hour)}
	m.tokens["local-a"] = token
	raw, _ := json.Marshal(token)
	if err := store.Set(storeKey("local-a"), string(raw)); err != nil {
		t.Fatal(err)
	}
	expiry := "2026-10-21T00:00:00Z"
	f := &resetFixture{t: t, response: `{"code":"reset","windows_reset":2}`, usage: &UsageSnapshot{AccountID: "acct-a", RateLimit: &UsageRateLimit{PrimaryWindow: &UsageWindow{LimitWindowSeconds: 18000, UsedPercent: 91}}, RateLimitResetCredits: &UsageResetCredits{AvailableCount: 2}}, credits: &UsageResetCredits{AvailableCount: 2, Credits: []UsageResetCredit{{ID: "credit-a", ResetType: "codex_rate_limits", Status: "available", ExpiresAt: &expiry}, {ID: "credit-b", ResetType: "codex_rate_limits", Status: "available"}}}}
	m.hc = &http.Client{Transport: f}
	original := usageURL
	usageURL = "https://reset-fixture.invalid/usage"
	t.Cleanup(func() { usageURL = original })
	return m, f, UsageResetBinding{ProviderID: "provider-a", ConnectionID: "connection-a", Account: "local-a"}
}
func noResetGuard() (func(), error) { return func() {}, nil }
func prepareReset(t *testing.T, m *Manager, b UsageResetBinding) *UsageResetRequest {
	t.Helper()
	r, e := m.PrepareUsageReset(context.Background(), b, "credit-a", "acct-a", "offline-admin")
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func confirmReset(m *Manager, b UsageResetBinding, r *UsageResetRequest) (*UsageResetRequest, error) {
	return m.ConfirmUsageReset(context.Background(), b, r.ID, r.AccountID, r.Credit.ID, noResetGuard)
}

func TestUsageResetSelectedIdentityAndReplay(t *testing.T) {
	m, f, b := resetTestManager(t)
	r := prepareReset(t, m, b)
	if len(f.posts) != 0 {
		t.Fatal("prepare must never dispatch consume")
	}
	again := prepareReset(t, m, b)
	if again.ID != r.ID {
		t.Fatal("duplicate prepare lost idempotency")
	}
	for _, binding := range []UsageResetBinding{{ProviderID: "other", ConnectionID: b.ConnectionID, Account: b.Account}, {ProviderID: b.ProviderID, ConnectionID: "other", Account: b.Account}} {
		if _, err := confirmReset(m, binding, r); err == nil {
			t.Fatal("cross-resource confirmation accepted")
		}
	}
	for _, ids := range [][2]string{{"acct-b", "credit-a"}, {"acct-a", "credit-b"}} {
		if _, err := m.ConfirmUsageReset(context.Background(), b, r.ID, ids[0], ids[1], noResetGuard); err == nil {
			t.Fatal("wrong account/credit accepted")
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := confirmReset(m, b, r)
			if err == nil && result.Status != "reset" {
				err = errors.New("wrong outcome")
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(f.posts) != 1 || f.posts[0]["credit_id"] != "credit-a" || f.posts[0]["redeem_request_id"] != r.ID || f.accounts[0] != "acct-a" {
		t.Fatalf("wrong or duplicate dispatch: %+v", f.posts)
	}
	if _, err := m.PrepareUsageReset(context.Background(), b, "credit-a", "acct-a", "offline-admin"); err == nil {
		t.Fatal("stale available list recreated a consumed credit")
	}
}
func TestUsageResetUnknownSurvivesRestartAndOnlyReusesOriginal(t *testing.T) {
	m, f, b := resetTestManager(t)
	r := prepareReset(t, m, b)
	f.fail = true
	result, err := confirmReset(m, b, r)
	if err != nil || result.Status != "uncertain" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	restarted := NewManager(m.store)
	restarted.now = m.now
	restarted.hc = m.hc
	requests, err := restarted.UsageResetRequests()
	if err != nil || len(requests) != 1 || requests[0].Status != "uncertain" {
		t.Fatal("uncertain intent lost")
	}
	if _, err := restarted.CancelUsageReset(b, r.ID); err == nil {
		t.Fatal("uncertain intent canceled")
	}
	alias := *restarted.tokens[b.Account]
	restarted.tokens["alias"] = &alias
	other := b
	other.Account = "alias"
	other.ConnectionID = "alias-connection"
	for _, binding := range []UsageResetBinding{b, other} {
		if _, err := restarted.PrepareUsageReset(context.Background(), binding, "credit-b", "acct-a", "offline-admin"); err == nil {
			t.Fatal("new credit bypassed unresolved account")
		}
	}
	f.fail = false
	f.response = `{"code":"already_redeemed","windows_reset":2}`
	result, err = confirmReset(restarted, b, r)
	if err != nil || result.Status != "already_redeemed" {
		t.Fatalf("%+v %v", result, err)
	}
	if len(f.posts) != 2 || f.posts[0]["redeem_request_id"] != f.posts[1]["redeem_request_id"] || f.posts[1]["credit_id"] != "credit-a" {
		t.Fatal("retry switched request or credit")
	}
}
func TestUsageResetPreflightAndIdentityFailClosed(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Manager, *resetFixture)
	}{
		{"mismatched account", func(m *Manager, f *resetFixture) { f.usage.AccountID = "acct-b" }},
		{"wrong reset scope", func(m *Manager, f *resetFixture) { f.credits.Credits[0].ResetType = "other_reset" }},
		{"redeemed", func(m *Manager, f *resetFixture) { f.credits.Credits[0].Status = "redeemed" }},
		{"expired credit", func(m *Manager, f *resetFixture) { v := "2026-09-20T00:00:00Z"; f.credits.Credits[0].ExpiresAt = &v }},
		{"invalid expiry", func(m *Manager, f *resetFixture) { v := "unknown"; f.credits.Credits[0].ExpiresAt = &v }},
		{"ambiguous credit", func(m *Manager, f *resetFixture) { f.credits.Credits = append(f.credits.Credits, f.credits.Credits[0]) }},
		{"details unavailable", func(m *Manager, f *resetFixture) { f.detailError = true }},
		{"journal unavailable", func(m *Manager, f *resetFixture) { _ = m.store.Close() }},
		{"journal corrupt", func(m *Manager, f *resetFixture) { _ = m.store.Set(usageResetPrefix+"bad", "{}") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, f, b := resetTestManager(t)
			tc.change(m, f)
			if _, err := m.PrepareUsageReset(context.Background(), b, "credit-a", "acct-a", "offline-admin"); err == nil {
				t.Fatal("unsafe intent accepted")
			}
			if len(f.posts) != 0 {
				t.Fatal("preflight consumed credit")
			}
		})
	}
	for _, kind := range []string{"principal", "workspace", "expiry", "guard", "credential during preflight", "store before dispatch", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			m, f, b := resetTestManager(t)
			r := prepareReset(t, m, b)
			guard := noResetGuard
			switch kind {
			case "principal":
				m.tokens[b.Account].AccessToken = "another-opaque-person"
			case "workspace":
				m.tokens[b.Account].AccountID = "acct-b"
			case "expiry":
				now := m.now().Add(11 * time.Minute)
				m.now = func() time.Time { return now }
			case "guard":
				guard = func() (func(), error) { return nil, errors.New("connection remapped") }
			case "credential during preflight":
				guard = func() (func(), error) { m.tokens[b.Account].AccessToken = "replaced"; return func() {}, nil }
			case "store before dispatch":
				guard = func() (func(), error) { _ = m.store.Close(); return func() {}, nil }
			case "canceled":
				if _, err := m.CancelUsageReset(b, r.ID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := m.ConfirmUsageReset(context.Background(), b, r.ID, r.AccountID, r.Credit.ID, guard); err == nil {
				t.Fatal("unsafe confirmation accepted")
			}
			if len(f.posts) != 0 {
				t.Fatal("unsafe confirmation dispatched")
			}
		})
	}
}
func TestUsageResetDoesNotGateOnUsagePercent(t *testing.T) {
	for _, used := range []float64{0, 50, 89, 100} {
		m, f, b := resetTestManager(t)
		f.usage.RateLimit.PrimaryWindow.UsedPercent = used
		r := prepareReset(t, m, b)
		if len(f.posts) != 0 {
			t.Fatal("preparation consumed a credit")
		}
		result, err := confirmReset(m, b, r)
		if err != nil || result.Status != "reset" || len(f.posts) != 1 || f.posts[0]["credit_id"] != "credit-a" {
			t.Fatalf("used=%v: unexpected offline result %+v, err=%v", used, result, err)
		}
	}
}
func TestUsageResetJournalSavedBeforeDispatchAndRecovery(t *testing.T) {
	m, f, b := resetTestManager(t)
	r := prepareReset(t, m, b)
	f.beforePost = func() {
		entries, err := m.store.EntriesWithPrefix(usageResetPrefix)
		if err != nil {
			t.Error(err)
		}
		var record usageResetRecord
		_ = json.Unmarshal([]byte(entries[usageResetPrefix+r.ID]), &record)
		if record.Request.Status != "in_flight" {
			t.Error("dispatch preceded durable journal")
		}
		_ = m.store.Close()
	}
	result, err := confirmReset(m, b, r)
	if err != nil || result.Status != "uncertain" || len(f.posts) != 1 {
		t.Fatalf("%+v %v", result, err)
	}
	// Separate restart case models a process stopping after durable in_flight.
	m, f, b = resetTestManager(t)
	r = prepareReset(t, m, b)
	records, _ := m.resetRecords()
	records[0].Request.Status = "in_flight"
	if err := m.saveReset(records[0]); err != nil {
		t.Fatal(err)
	}
	restarted := NewManager(m.store)
	restarted.now = m.now
	restarted.hc = m.hc
	requests, err := restarted.UsageResetRequests()
	if err != nil || requests[0].Status != "uncertain" {
		t.Fatal("interrupted dispatch lost reservation")
	}
	if _, err := restarted.PrepareUsageReset(context.Background(), b, "credit-b", "acct-a", "offline-admin"); err == nil {
		t.Fatal("interrupted request allowed second credit")
	}
}
func TestUsageCreditDetailsPartialNullAndFailure(t *testing.T) {
	m, f, b := resetTestManager(t)
	f.credits.AvailableCount = 3
	usage, err := m.Usage(context.Background(), b.Account)
	if err != nil {
		t.Fatal(err)
	}
	if usage.AccountID != "acct-a" || usage.RateLimitResetCredits.AvailableCount != 3 || len(usage.RateLimitResetCredits.Credits) != 2 || usage.RateLimitResetCredits.Credits[1].ExpiresAt != nil {
		t.Fatalf("partial/null details lost: %+v", usage)
	}
	f.detailError = true
	usage, err = m.Usage(context.Background(), b.Account)
	if err != nil {
		t.Fatal(err)
	}
	if usage.RateLimitResetCredits.AvailableCount != 2 || usage.RateLimitResetCredits.Credits != nil || usage.RateLimitResetCredits.DetailsError == "" {
		t.Fatal("failed details claimed completeness")
	}
	if len(f.posts) != 0 {
		t.Fatal("read-only usage dispatched consume")
	}
}
