package oauth

import (
	"context"
	"encoding/json"
	"ferry/internal/domain"
	"ferry/internal/secretstore"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestUsageProtectionWindows(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name                                      string
		primary, secondary                        float64
		allow, credits, unlimited, reached, spend bool
		blocked                                   bool
	}{
		{name: "below", primary: 97.9, secondary: 60},
		{name: "boundary", primary: 98, blocked: true},
		{name: "weekly", primary: 10, secondary: 98, blocked: true},
		{name: "credits default off", primary: 100, credits: true, blocked: true},
		{name: "opt in below exhaustion", primary: 99, allow: true},
		{name: "opt in paid", primary: 100, allow: true, credits: true},
		{name: "unlimited", primary: 100, allow: true, unlimited: true},
		{name: "empty credits", primary: 100, allow: true, blocked: true},
		{name: "upstream flag", primary: 20, reached: true, blocked: true},
		{name: "spending cap", primary: 100, allow: true, credits: true, spend: true, blocked: true},
		{name: "invalid", primary: math.NaN(), blocked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := &UsageSnapshot{ObservedAt: now, RateLimit: &UsageRateLimit{LimitReached: tc.reached, PrimaryWindow: &UsageWindow{UsedPercent: tc.primary, ResetAt: now.Add(time.Hour).Unix()}, SecondaryWindow: &UsageWindow{UsedPercent: tc.secondary, ResetAt: now.Add(7 * 24 * time.Hour).Unix()}}, Credits: &UsageCredits{HasCredits: tc.credits, Unlimited: tc.unlimited}, SpendControl: &UsageSpendControl{Reached: tc.spend}}
			ws := ProtectionWindows(u, UsageProtection{MaxUsedPercent: 98, AllowCredits: tc.allow}, now)
			blocked := false
			for _, w := range ws {
				blocked = blocked || w.Availability == domain.QuotaExhausted
			}
			if blocked != tc.blocked {
				t.Fatalf("windows=%+v", ws)
			}
		})
	}
	for _, p := range []float64{0, -1, 101, math.Inf(1), math.NaN()} {
		if (UsageProtection{MaxUsedPercent: p}).Validate() == nil {
			t.Fatal(p)
		}
	}
	if ProtectionWindows(nil, DefaultUsageProtection(), now)[0].Availability != domain.QuotaExhausted {
		t.Fatal("unknown quota must block")
	}
	u := &UsageSnapshot{ObservedAt: now, RateLimit: &UsageRateLimit{PrimaryWindow: &UsageWindow{UsedPercent: 98, ResetAfterSeconds: 3600}}}
	a := ProtectionWindows(u, DefaultUsageProtection(), now)[0]
	b := ProtectionWindows(u, DefaultUsageProtection(), now.Add(time.Minute))[0]
	if !a.ResetAt.Equal(b.ResetAt) {
		t.Fatal("replaying cache extended reset")
	}
}

func TestUsageGuardResponseHeadersAndFallback(t *testing.T) {
	now := time.Now().UTC()
	reads := 0
	used := 50.0
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads++
		if fail {
			w.WriteHeader(503)
			return
		}
		fmt.Fprintf(w, `{"account_id":"acct","rate_limit":{"allowed":true,"primary_window":{"used_percent":%g,"reset_at":%d},"secondary_window":{"used_percent":10,"reset_at":%d}},"credits":{"has_credits":true,"unlimited":false}}`, used, now.Add(time.Hour).Unix(), now.Add(7*24*time.Hour).Unix())
	}))
	defer srv.Close()
	old := usageURL
	usageURL = srv.URL
	t.Cleanup(func() { usageURL = old })
	st, err := secretstore.OpenSQLite(t.TempDir() + "/secrets.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	token, _ := json.Marshal(Token{AccessToken: "fixture-access", RefreshToken: "fixture-refresh", AccountID: "acct", ExpiresAt: now.Add(time.Hour)})
	if err := st.Set("oauth:codex:work", string(token)); err != nil {
		t.Fatal(err)
	}
	m := NewManager(st)
	m.now = func() time.Time { return now }
	policy := DefaultUsageProtection()
	var observed []domain.QuotaWindowState
	m.SetUsageProtection(func() (UsageProtection, error) { return policy, nil }, func(_ string, _ *UsageSnapshot, ws []domain.QuotaWindowState) { observed = ws })
	check := func(wantBlocked bool) {
		t.Helper()
		f := m.CheckUsage(context.Background(), "work", "acct")
		if (f != nil) != wantBlocked {
			t.Fatalf("failure=%+v reads=%d", f, reads)
		}
	}
	check(false)
	check(false)
	if reads != 1 {
		t.Fatalf("repeated fresh requests queried usage %d times", reads)
	}
	headers := http.Header{}
	for _, name := range []string{"primary", "secondary"} {
		headers.Set("x-codex-"+name+"-used-percent", "98")
		headers.Set("x-codex-"+name+"-window-minutes", "300")
		headers.Set("x-codex-"+name+"-reset-at", strconv.FormatInt(now.Add(time.Hour).Unix(), 10))
	}
	headers.Set("x-codex-credits-has-credits", "true")
	headers.Set("x-codex-credits-unlimited", "false")
	m.ObserveUsageHeaders("work", "acct", headers)
	check(true)
	if reads != 1 || len(observed) == 0 || observed[0].Availability != domain.QuotaExhausted {
		t.Fatal("headers did not park connection without a fetch")
	}
	// Older/lower responses must not undo the threshold in the same window.
	headers.Set("x-codex-primary-used-percent", "30")
	m.ObserveUsageHeaders("work", "acct", headers)
	check(true)
	// A delayed full usage response must not lower a window already tightened
	// by inference headers from the same reset interval.
	m.mu.Lock()
	m.usageVerified["work"] = time.Time{}
	m.mu.Unlock()
	check(true)
	if reads != 2 {
		t.Fatal(reads)
	}
	policy.AllowCredits = true
	check(false)
	m.ObserveUsageHeaders("work", "other-account", headers)
	now = now.Add(31 * time.Second)
	used = 100
	check(false)
	if reads != 3 {
		t.Fatal("expired evidence not refreshed")
	}
	policy.AllowCredits = false
	check(true)
	now = now.Add(31 * time.Second)
	fail = true
	check(true)
	if reads != 4 {
		t.Fatal(reads)
	}
	// Incomplete and malformed headers must not mint a fresh observation.
	cached, _ := m.CachedUsage("work")
	at := cached.ObservedAt
	m.ObserveUsageHeaders("work", "acct", http.Header{"X-Codex-Primary-Used-Percent": []string{"NaN"}})
	cached, _ = m.CachedUsage("work")
	if !cached.ObservedAt.Equal(at) {
		t.Fatal("invalid response renewed freshness")
	}
}

func TestUsageProtectionMissingPercentIsNotZero(t *testing.T) {
	var u UsageSnapshot
	if err := json.Unmarshal([]byte(`{"rate_limit":{"primary_window":{"reset_at":2000000000}}}`), &u); err != nil {
		t.Fatal(err)
	}
	ws := ProtectionWindows(&u, DefaultUsageProtection(), time.Now())
	if len(ws) == 0 || ws[0].Availability != domain.QuotaExhausted {
		t.Fatal("missing used_percent permitted inference")
	}
}

func TestUsageMergeDoesNotHideMissingPercent(t *testing.T) {
	old := &UsageWindow{UsedPercent: 98, ResetAt: 2000000000}
	invalid := &UsageWindow{missingUsedPercent: true, ResetAt: 2000000000}
	if conservativeUsageWindow(old, invalid) != invalid {
		t.Fatal("missing percentage was masked by cached evidence")
	}
}
