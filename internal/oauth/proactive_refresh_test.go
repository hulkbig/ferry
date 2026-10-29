package oauth

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func makeValidTokenDue(m *Manager, account string) Token {
	m.mu.Lock()
	defer m.mu.Unlock()
	tok := *m.tokens[account]
	tok.ObtainedAt = m.now().Add(-20 * 24 * time.Hour)
	tok.ExpiresAt = m.now().Add(4 * 24 * time.Hour)
	m.tokens[account] = &tok
	return tok
}

func TestProactiveRefreshRotatesValidDueTokenAndRechecksStaleScan(t *testing.T) {
	m, f, done := newTestManager(t, tokenRespJSON, http.StatusOK)
	defer done()
	fullLogin(t, m, "work")
	old := makeValidTokenDue(m, "work")
	if !old.Valid(m.now()) {
		t.Fatal("fixture must still be usable")
	}
	if due := m.accountsDueForRefresh(m.now()); len(due) != 1 || due[0] != "work" {
		t.Fatalf("due=%v", due)
	}
	f.swapResp(`{"access_token":"at-new","refresh_token":"rt-new","expires_in":2592000}`, http.StatusOK)
	before := f.callCount()
	m.refreshDueAccounts(context.Background())
	access, err := m.AccessToken(context.Background(), "work")
	if err != nil || access != "at-new" || f.callCount()-before != 1 {
		t.Fatalf("refresh: access=%q err=%v calls=%d", access, err, f.callCount()-before)
	}
	m.refreshDueAccounts(context.Background())
	// Simulate a background scan that selected the account before replacement.
	fresh, err := m.refreshAccountWhen(context.Background(), "work", false, "", true)
	if err != nil || fresh.RefreshToken != "rt-new" || f.callCount()-before != 1 {
		t.Fatalf("stale scan rotated replacement: token=%+v err=%v calls=%d", fresh, err, f.callCount()-before)
	}
}

func TestProactiveRefreshSharesReservationWithForcedRefresh(t *testing.T) {
	m, f, done := newTestManager(t, tokenRespJSON, http.StatusOK)
	defer done()
	fullLogin(t, m, "work")
	old := makeValidTokenDue(m, "work")
	f.swapResp(`{"access_token":"at-new","refresh_token":"rt-new","expires_in":2592000}`, http.StatusOK)
	f.mu.Lock()
	f.delay = 100 * time.Millisecond
	f.mu.Unlock()
	before := f.callCount()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	errs := make(chan error, 13)
	refresh := func(force bool) {
		tok, err := m.refreshAccountWhen(ctx, "work", force, old.AccessToken, !force)
		if err == nil && (tok == nil || tok.AccessToken != "at-new") {
			err = fmt.Errorf("refresh did not return the replacement")
		}
		errs <- err
	}
	go refresh(false)
	waitForOAuth(t, time.Second, func() bool { return f.callCount() > before })
	// Request-path reads remain usable while the proactive rotation is in flight.
	if access, err := m.AccessToken(ctx, "work"); err != nil || (access != old.AccessToken && access != "at-new") {
		t.Fatalf("lazy read: %q %v", access, err)
	}
	for i := 0; i < 12; i++ {
		go refresh(i%2 == 0)
	}
	for i := 0; i < 13; i++ {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
	if f.callCount()-before != 1 {
		t.Fatalf("concurrent refreshes rotated %d times", f.callCount()-before)
	}
}

func TestProactiveTransientFailurePreservesUsableCredential(t *testing.T) {
	m, f, done := newTestManager(t, tokenRespJSON, http.StatusOK)
	defer done()
	fullLogin(t, m, "work")
	old := makeValidTokenDue(m, "work")
	f.swapResp(`{"error":"server_error"}`, http.StatusInternalServerError)
	before := f.callCount()
	m.refreshDueAccounts(context.Background())
	access, err := m.AccessToken(context.Background(), "work")
	if err != nil || access != old.AccessToken || f.callCount()-before != 1 {
		t.Fatalf("usable token lost: access=%q err=%v calls=%d", access, err, f.callCount()-before)
	}
	if _, failed := m.AuthFailedUntil("work"); failed {
		t.Fatal("transient failure parked the account as an invalid grant")
	}
	m.mu.Lock()
	unchanged := *m.tokens["work"] == old
	m.mu.Unlock()
	if !unchanged {
		t.Fatal("failed background refresh changed stored credentials")
	}
}
