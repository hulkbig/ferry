package oauth

import (
	"testing"
	"time"

	"ferry/internal/secretstore"
)

func newTestStore(t *testing.T) *secretstore.Store {
	t.Helper()
	store, err := secretstore.Open(t.TempDir() + "/secrets.json")
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestTokenDueForRefreshUsesLeadCappedByLifetime(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		obtained time.Time
		expires  time.Time
		wantDue  bool
	}{
		{
			// Short-lived token, freshly minted: refreshing now would mean
			// refreshing on every scheduler pass.
			name: "fresh short lived", obtained: now.Add(-time.Minute),
			expires: now.Add(59 * time.Minute), wantDue: false,
		},
		{
			name: "short lived past halfway", obtained: now.Add(-40 * time.Minute),
			expires: now.Add(20 * time.Minute), wantDue: true,
		},
		{
			// Long-lived token: the absolute lead binds before halfway does.
			name: "long lived inside lead", obtained: now.Add(-20 * 24 * time.Hour),
			expires: now.Add(4 * 24 * time.Hour), wantDue: true,
		},
		{
			name: "long lived outside lead", obtained: now.Add(-24 * time.Hour),
			expires: now.Add(30 * 24 * time.Hour), wantDue: false,
		},
		{
			// Records written before ObtainedAt existed fall back to the lead.
			name:    "legacy record without obtained_at",
			expires: now.Add(30 * 24 * time.Hour), wantDue: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tok := &Token{AccessToken: "at", RefreshToken: "rt",
				ObtainedAt: test.obtained, ExpiresAt: test.expires}
			if got := tokenDueForRefresh(tok, now); got != test.wantDue {
				t.Fatalf("due=%v want %v", got, test.wantDue)
			}
		})
	}
	if tokenDueForRefresh(nil, now) {
		t.Fatal("nil token reported as due")
	}
}

func TestAccountsDueForRefreshSkipsRejectedAndInFlight(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	m := NewManager(newTestStore(t))
	m.now = func() time.Time { return now }
	due := &Token{AccessToken: "at", RefreshToken: "rt",
		ObtainedAt: now.Add(-40 * time.Minute), ExpiresAt: now.Add(20 * time.Minute)}
	m.tokens["healthy"] = due
	m.tokens["rejected"] = &Token{AccessToken: "at2", RefreshToken: "rt2",
		ObtainedAt: due.ObtainedAt, ExpiresAt: due.ExpiresAt}
	m.tokens["refreshing"] = &Token{AccessToken: "at3", RefreshToken: "rt3",
		ObtainedAt: due.ObtainedAt, ExpiresAt: due.ExpiresAt}
	m.tokens["no-refresh-token"] = &Token{AccessToken: "at4", ExpiresAt: due.ExpiresAt}
	m.authFailedUntil["rejected"] = now.Add(time.Minute)
	m.refreshes["refreshing"] = &refreshCall{done: make(chan struct{})}

	got := m.accountsDueForRefresh(now)
	if len(got) != 1 || got[0] != "healthy" {
		t.Fatalf("due accounts=%v want [healthy]", got)
	}

	// Once the backoff lapses the account is readmitted.
	m.authFailedUntil["rejected"] = now.Add(-time.Minute)
	got = m.accountsDueForRefresh(now)
	if len(got) != 2 || got[0] != "healthy" || got[1] != "rejected" {
		t.Fatalf("due accounts after backoff=%v", got)
	}
}

func TestRefreshBackoffBlocksReplayOfRejectedToken(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	m := NewManager(newTestStore(t))
	m.now = func() time.Time { return now }
	m.tokens["acct"] = &Token{AccessToken: "stale", RefreshToken: "rt", ExpiresAt: now.Add(-time.Hour)}
	m.noteAuthFailureLocked("acct", now)

	if _, err := m.refreshAccount(nil, "acct", true, "stale"); err == nil {
		t.Fatal("rejected refresh token was replayed")
	}
	if _, ok := m.AuthFailedUntil("acct"); !ok {
		t.Fatal("account not reported as parked")
	}
	m.mu.Lock()
	m.clearAuthFailureLocked("acct")
	m.mu.Unlock()
	if _, ok := m.AuthFailedUntil("acct"); ok {
		t.Fatal("backoff survived an explicit clear")
	}
}
