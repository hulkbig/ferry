package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"strings"
	"time"
)

type loopbackCallbackServer struct {
	server   *http.Server
	listener net.Listener
}

// EnsureLoopback starts the optional browser callback listener. One
// loopback-only server serves every concurrent pending account and resolves
// the account exclusively from the anti-CSRF state value.
func (m *Manager) EnsureLoopback() (bool, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return false, fmt.Errorf("OAuth manager is closed")
	}
	m.loopbackMu.Lock()
	m.mu.Unlock()
	defer m.loopbackMu.Unlock()
	if m.loopback != nil {
		return true, nil
	}
	listener, err := net.Listen("tcp", "localhost:1455")
	if err != nil {
		return false, fmt.Errorf("listen for OAuth callback on localhost:1455: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", m.handleLoopbackCallback)
	mux.HandleFunc("/success", func(w http.ResponseWriter, _ *http.Request) {
		writeLoopbackPage(w, http.StatusOK, "Codex login complete", "You can close this window and return to Ferry.")
	})
	server := &http.Server{
		Handler: mux, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second,
	}
	m.loopback = &loopbackCallbackServer{server: server, listener: listener}
	go func() {
		if serveErr := server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			m.loopbackMu.Lock()
			if m.loopback != nil && m.loopback.server == server {
				m.loopback = nil
			}
			m.loopbackMu.Unlock()
		}
	}()
	return true, nil
}

// LoopbackListening reports whether the optional callback listener is active.
func (m *Manager) LoopbackListening() bool {
	m.loopbackMu.Lock()
	defer m.loopbackMu.Unlock()
	return m.loopback != nil
}

func (m *Manager) handleLoopbackCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	account := m.accountForState(state)
	if account == "" {
		writeLoopbackPage(w, http.StatusBadRequest, "Codex login failed", "The login state is missing, expired, or no longer active.")
		return
	}
	if providerErr := strings.TrimSpace(r.URL.Query().Get("error")); providerErr != "" {
		description := strings.TrimSpace(r.URL.Query().Get("error_description"))
		if description == "" {
			description = providerErr
		}
		m.failBrowserPending(account, state, description)
		writeLoopbackPage(w, http.StatusBadRequest, "Codex login failed", description)
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" {
		writeLoopbackPage(w, http.StatusBadRequest, "Codex login failed", "The callback did not contain an authorization code.")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err := m.CompleteLoginContext(ctx, account, state, code)
	cancel()
	if err != nil {
		m.failBrowserPending(account, state, err.Error())
		writeLoopbackPage(w, http.StatusBadRequest, "Codex login failed", err.Error())
		return
	}
	http.Redirect(w, r, "/success", http.StatusFound)
}

func (m *Manager) accountForState(state string) string {
	if state == "" {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for account, pending := range m.pending {
		if pending != nil && pending.Mode != loginModeDevice && pending.State == state {
			return account
		}
	}
	return ""
}

func (m *Manager) failBrowserPending(account, state, message string) {
	m.mu.Lock()
	pending := m.pending[account]
	if pending == nil || pending.State != state {
		m.mu.Unlock()
		return
	}
	pending.Status = loginStatusFailed
	pending.Error = truncateOAuthMessage(message)
	snapshot := *pending
	if raw, err := json.Marshal(&snapshot); err == nil {
		_ = m.store.Set(pendingStoreKey(account), string(raw))
	}
	m.mu.Unlock()
}

// Close stops device pollers and the optional loopback listener. It is safe to
// call when no login flow has been started.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	cancels := make([]context.CancelFunc, 0, len(m.pendingCancels))
	for _, cancel := range m.pendingCancels {
		cancels = append(cancels, cancel)
	}
	m.pendingCancels = map[string]context.CancelFunc{}
	m.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	m.loopbackMu.Lock()
	callback := m.loopback
	m.loopback = nil
	m.loopbackMu.Unlock()
	if callback == nil {
		return nil
	}
	return callback.server.Shutdown(ctx)
}

func writeLoopbackPage(w http.ResponseWriter, status int, title, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "<!doctype html><html><head><title>%s</title></head><body><h1>%s</h1><p>%s</p></body></html>",
		html.EscapeString(title), html.EscapeString(title), html.EscapeString(message))
}

func truncateOAuthMessage(message string) string {
	message = strings.TrimSpace(message)
	if len(message) > 512 {
		return message[:512]
	}
	return message
}
