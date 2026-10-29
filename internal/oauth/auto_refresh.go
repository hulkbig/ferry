package oauth

import (
	"context"
	"log"
	"sort"
	"time"
)

const (
	// autoRefreshLead is how far ahead of expiry a token is replaced when its
	// lifetime allows. Codex access tokens are long-lived, and refreshing well
	// before expiry keeps the refresh off the request path entirely.
	autoRefreshLead = 5 * 24 * time.Hour
	// autoRefreshInterval is how often the scheduler re-examines accounts.
	autoRefreshInterval = time.Minute
	// autoRefreshFloor keeps a short-lived token from being refreshed on every
	// pass: it is replaced only once it is past the halfway point.
	autoRefreshFloor = 2
	// authFailureBackoff is how long an account that failed to refresh with a
	// non-retryable grant error is left alone. Replaying a rejected refresh
	// token in a loop is what makes a provider treat rotation as abuse.
	authFailureBackoff = 30 * time.Minute
)

// StartAutoRefresh runs proactive token renewal until ctx is done. Accounts
// whose refresh token the provider has rejected are skipped for a cooling
// period rather than retried every pass.
func (m *Manager) StartAutoRefresh(ctx context.Context) {
	if m.standby {
		return
	}
	go m.runAutoRefresh(ctx)
}

func (m *Manager) runAutoRefresh(ctx context.Context) {
	ticker := time.NewTicker(autoRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.refreshDueAccounts(ctx)
		}
	}
}

func (m *Manager) refreshDueAccounts(ctx context.Context) {
	for _, account := range m.accountsDueForRefresh(m.now()) {
		if ctx.Err() != nil {
			return
		}
		if _, err := m.refreshAccountWhen(ctx, account, false, "", true); err != nil {
			log.Printf("oauth: proactive refresh for account %s failed: %v", account, err)
		}
	}
}

// accountsDueForRefresh lists accounts whose access token should be replaced
// now, in stable order. An account excluded here is either healthy, already
// refreshing, or serving a refresh-failure backoff.
func (m *Manager) accountsDueForRefresh(now time.Time) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	due := make([]string, 0, len(m.tokens))
	for account, tok := range m.tokens {
		if tok == nil || tok.RefreshToken == "" {
			continue
		}
		if m.refreshes[account] != nil {
			continue
		}
		if until, failed := m.authFailedUntil[account]; failed && now.Before(until) {
			continue
		}
		if tokenDueForRefresh(tok, now) {
			due = append(due, account)
		}
	}
	sort.Strings(due)
	return due
}

// tokenDueForRefresh aims at the configured lead, but never asks for more
// than half of a token's own lifetime so a short-lived grant is refreshed at
// most twice per lifetime instead of on every scheduler pass.
func tokenDueForRefresh(tok *Token, now time.Time) bool {
	if tok == nil || tok.ImportedDormant || tok.ExpiresAt.IsZero() {
		return false
	}
	lead := autoRefreshLead
	if lifetime := tok.Lifetime(); lifetime > 0 && lifetime/autoRefreshFloor < lead {
		lead = lifetime / autoRefreshFloor
	}
	return !now.Before(tok.ExpiresAt.Add(-lead))
}

// noteAuthFailureLocked parks an account whose refresh token the provider has
// rejected outright. Callers must hold m.mu.
func (m *Manager) noteAuthFailureLocked(account string, now time.Time) {
	if m.authFailedUntil == nil {
		m.authFailedUntil = map[string]time.Time{}
	}
	m.authFailedUntil[account] = now.Add(authFailureBackoff)
}

// clearAuthFailureLocked readmits an account after any successful token
// acquisition. Callers must hold m.mu.
func (m *Manager) clearAuthFailureLocked(account string) {
	delete(m.authFailedUntil, account)
}

// AuthFailedUntil reports the refresh-backoff deadline for one account, if it
// is currently serving one. Used by management status views.
func (m *Manager) AuthFailedUntil(account string) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	until, ok := m.authFailedUntil[account]
	if !ok || !m.now().Before(until) {
		return time.Time{}, false
	}
	return until, true
}
