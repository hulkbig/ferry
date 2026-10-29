package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"ferry/internal/domain"
	"ferry/internal/secretstore"
)

// Manager keeps every logged-in Codex account's live token in memory, backed
// by the secret store (JSON-encoded Token under a reserved name). Refreshes
// are lazy and single-flight: the first caller inside the expiry margin
// refreshes synchronously; concurrent callers share the result.
type Manager struct {
	standby     bool       // immutable after construction; copied grants must not rotate
	autoResetMu sync.Mutex // serializes automatic selection through dispatch
	resetMu     sync.Mutex // serializes durable usage-reset intents, never cooldown/quota operations
	mu          sync.Mutex
	store       *secretstore.Store
	hc          *http.Client
	tokens      map[string]*Token // account → live token
	// usageCache keeps the latest successful, redacted subscription snapshot
	// per account. Routing authorization always fetches fresh evidence.
	usageCache    map[string]*UsageSnapshot
	usageVerified map[string]time.Time // full network reads in this process, never restored from disk
	usagePolicy   func() (UsageProtection, error)
	usageObserver func(string, *UsageSnapshot, []domain.QuotaWindowState)
	// pending holds in-flight login attempts: account → {state, verifier,
	// started}. state is the anti-CSRF nonce checked at callback time.
	pending map[string]*pendingLogin
	// refreshes is a real per-account single-flight registry. A rotating
	// refresh token must never be submitted concurrently by this process.
	refreshes      map[string]*refreshCall
	pendingCancels map[string]context.CancelFunc
	// authFailedUntil parks accounts whose refresh token the provider has
	// rejected outright, so neither the scheduler nor a 401 retry replays it.
	authFailedUntil map[string]time.Time
	onChange        func(string)
	closed          bool
	now             func() time.Time
	wait            func(context.Context, time.Duration) error
	loopbackMu      sync.Mutex
	loopback        *loopbackCallbackServer
}

// pendingLogin is one started-but-not-finished browser or device login.
type pendingLogin struct {
	Mode            string    `json:"mode,omitempty"`
	State           string    `json:"state,omitempty"`
	Verifier        string    `json:"verifier,omitempty"`
	DeviceAuthID    string    `json:"device_auth_id,omitempty"`
	UserCode        string    `json:"user_code,omitempty"`
	VerificationURL string    `json:"verification_url,omitempty"`
	IntervalSeconds int       `json:"interval_seconds,omitempty"`
	Status          string    `json:"status,omitempty"`
	Error           string    `json:"error,omitempty"`
	Started         time.Time `json:"started"`
}

type refreshCall struct {
	done  chan struct{}
	token *Token
	err   error
}

// storePrefix namespaces OAuth tokens inside the shared secret store so they
// can never collide with console-entered API key names (which forbid colons).
const storePrefix = "oauth:codex:"

const pendingStorePrefix = "oauth:pending:codex:"

const usageStorePrefix = "oauth:usage:codex:"

// storeKey is the secret-store entry holding one account's token JSON.
func storeKey(account string) string { return storePrefix + account }

func pendingStoreKey(account string) string { return pendingStorePrefix + account }

func usageStoreKey(account string) string { return usageStorePrefix + account }

// NewManager builds a manager over the given store and loads any tokens that
// were persisted by previous runs.
func NewManager(store *secretstore.Store) *Manager {
	return NewManagerWithOptions(store, Options{})
}

// Options controls ownership of copied upstream grants during migration.
type Options struct {
	Standby bool
}

var ErrStandby = errors.New("upstream account changes are disabled in standby mode")

func NewManagerWithOptions(store *secretstore.Store, options Options) *Manager {
	m := &Manager{
		standby:         options.Standby,
		store:           store,
		hc:              &http.Client{Timeout: 30 * time.Second},
		tokens:          map[string]*Token{},
		usageCache:      map[string]*UsageSnapshot{},
		pending:         map[string]*pendingLogin{},
		refreshes:       map[string]*refreshCall{},
		pendingCancels:  map[string]context.CancelFunc{},
		authFailedUntil: map[string]time.Time{},
		now:             time.Now,
		wait: func(ctx context.Context, delay time.Duration) error {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
	}
	m.loadPersisted()
	return m
}

// loadPersisted hydrates the in-memory cache from the secret store.
func (m *Manager) loadPersisted() {
	type resumableDevice struct {
		account string
		pending pendingLogin
	}
	var resumable []resumableDevice
	for _, name := range m.store.Names() {
		if strings.HasPrefix(name, usageStorePrefix) {
			acct := name[len(usageStorePrefix):]
			value, ok := m.store.Get(name)
			if !ok {
				continue
			}
			var snapshot UsageSnapshot
			if err := json.Unmarshal([]byte(value), &snapshot); err != nil || snapshot.ObservedAt.IsZero() {
				log.Printf("oauth account %s: cached usage unreadable, removing", acct)
				_ = m.store.Delete(name)
				continue
			}
			m.usageCache[acct] = &snapshot
			continue
		}
		if strings.HasPrefix(name, pendingStorePrefix) {
			acct := name[len(pendingStorePrefix):]
			v, ok := m.store.Get(name)
			if !ok {
				continue
			}
			var pending pendingLogin
			if err := json.Unmarshal([]byte(v), &pending); err != nil || !pending.valid() {
				log.Printf("oauth account %s: pending login unreadable, removing", acct)
				_ = m.store.Delete(name)
				continue
			}
			if m.now().Sub(pending.Started) > 15*time.Minute {
				_ = m.store.Delete(name)
				continue
			}
			m.pending[acct] = &pending
			if pending.Mode == loginModeDevice && pending.Status != loginStatusFailed {
				resumable = append(resumable, resumableDevice{account: acct, pending: pending})
			}
			continue
		}
		if !strings.HasPrefix(name, storePrefix) {
			continue
		}
		acct := name[len(storePrefix):]
		v, ok := m.store.Get(name)
		if !ok {
			continue
		}
		var tok Token
		if err := json.Unmarshal([]byte(v), &tok); err != nil {
			log.Printf("oauth account %s: stored token unreadable (%v), ignoring", acct, err)
			continue
		}
		m.tokens[acct] = &tok
	}
	if !m.standby {
		for i := range resumable {
			m.resumeDeviceLogin(resumable[i].account, &resumable[i].pending)
		}
	}
}

func (p *pendingLogin) valid() bool {
	if p == nil || p.Started.IsZero() {
		return false
	}
	switch p.Mode {
	case "", loginModeBrowser:
		return p.State != "" && p.Verifier != ""
	case loginModeDevice:
		return p.DeviceAuthID != "" && p.UserCode != ""
	default:
		return false
	}
}

// StartLogin creates a fresh PKCE pair + state for one account's browser flow
// and returns the URL the operator must open. Any earlier pending login for
// the same account is superseded.
func (m *Manager) StartLogin(account string) (string, error) {
	if m.standby {
		return "", ErrStandby
	}
	if err := validAccount(account); err != nil {
		return "", err
	}
	p, err := NewPKCE()
	if err != nil {
		return "", err
	}
	state, err := newState()
	if err != nil {
		return "", err
	}
	pending := &pendingLogin{Mode: loginModeBrowser, State: state, Verifier: p.Verifier,
		Status: loginStatusPending, Started: m.now()}
	raw, err := json.Marshal(pending)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return "", fmt.Errorf("OAuth manager is closed")
	}
	if err := m.store.Set(pendingStoreKey(account), string(raw)); err != nil {
		m.mu.Unlock()
		return "", fmt.Errorf("persist pending login: %w", err)
	}
	oldCancel := m.pendingCancels[account]
	delete(m.pendingCancels, account)
	m.pending[account] = pending
	m.mu.Unlock()
	if oldCancel != nil {
		oldCancel()
	}
	return LoginURL(state, p.Challenge), nil
}

var errNoPendingLogin = fmt.Errorf("no login in progress for this account (start one first)")

// CompleteLogin finishes a started login: state must match, the code is
// exchanged with the retained verifier, and the resulting token is persisted
// and cached. The token itself is never returned.
func (m *Manager) CompleteLogin(account, state, code string) error {
	if m.standby {
		return ErrStandby
	}
	return m.CompleteLoginContext(context.Background(), account, state, code)
}

// CompleteLoginContext completes a browser flow while honoring the caller's
// deadline for the token exchange.
func (m *Manager) CompleteLoginContext(ctx context.Context, account, state, code string) error {
	m.mu.Lock()
	p, ok := m.pending[account]
	if !ok {
		m.mu.Unlock()
		return errNoPendingLogin
	}
	if p.State != state {
		m.mu.Unlock()
		return fmt.Errorf("state mismatch: restart the login")
	}
	if m.now().Sub(p.Started) > 15*time.Minute {
		_ = m.store.Delete(pendingStoreKey(account))
		delete(m.pending, account)
		m.mu.Unlock()
		return fmt.Errorf("login attempt expired, start a new one")
	}
	verifier := p.Verifier
	m.mu.Unlock()

	tok, err := Exchange(ctx, m.hc, code, verifier)
	if err != nil {
		return fmt.Errorf("exchange: %w", err)
	}
	return m.storeToken(account, tok, loginModeBrowser, state)
}

// storeToken persists a successful browser/device login only while the exact
// pending attempt that produced it is still current.
func (m *Manager) storeToken(account string, tok *Token, mode, attemptID string) error {
	if tok == nil || tok.AccessToken == "" {
		return fmt.Errorf("oauth token missing access token")
	}
	b, err := json.Marshal(tok) // #nosec G117 -- OAuth token persistence boundary: the marshaled bytes go only to the private token store, never to logs or responses
	if err != nil {
		return err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return fmt.Errorf("OAuth manager is closed")
	}
	pending := m.pending[account]
	if pending == nil ||
		(mode == loginModeBrowser && pending.State != attemptID) ||
		(mode == loginModeDevice && pending.DeviceAuthID != attemptID) {
		m.mu.Unlock()
		return fmt.Errorf("OAuth login attempt was cancelled or superseded")
	}
	if err := m.store.Set(storeKey(account), string(b)); err != nil {
		m.mu.Unlock()
		return fmt.Errorf("persist token: %w", err)
	}
	m.tokens[account] = tok
	delete(m.usageCache, account)
	if err := m.store.Delete(usageStoreKey(account)); err != nil {
		log.Printf("oauth account %s: remove stale usage cache after login: %v", account, err)
	}
	delete(m.pending, account)
	// A fresh login supersedes any earlier refresh rejection for this account.
	m.clearAuthFailureLocked(account)
	if cancel := m.pendingCancels[account]; cancel != nil {
		cancel()
		delete(m.pendingCancels, account)
	}
	onChange := m.onChange
	cleanupErr := m.store.Delete(pendingStoreKey(account))
	m.mu.Unlock()
	if onChange != nil {
		// Every successful login can replace the account identity even when its
		// plan is unchanged. Notify synchronously so the host can finish that
		// identity switch before the login operation returns.
		onChange(account)
	}
	if cleanupErr != nil {
		return fmt.Errorf("remove completed pending login: %w", cleanupErr)
	}
	return nil
}

// storeRefreshedToken persists a refresh response only if the account still
// contains the credential that initiated it. Logout or a concurrent new login
// must win over an older in-flight refresh and must never be resurrected.
func (m *Manager) storeRefreshedToken(account string, tok, expected *Token) (*Token, error) {
	if tok == nil || expected == nil || tok.AccessToken == "" {
		return nil, fmt.Errorf("oauth refresh token result is incomplete")
	}
	b, err := json.Marshal(tok) // #nosec G117 -- OAuth token persistence boundary: the marshaled bytes go only to the private token store, never to logs or responses
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, fmt.Errorf("OAuth manager is closed")
	}
	current := m.tokens[account]
	if current == nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("account %s was logged out during token refresh", account)
	}
	if current.AccessToken != expected.AccessToken || current.RefreshToken != expected.RefreshToken {
		snapshot := *current
		m.mu.Unlock()
		return &snapshot, nil
	}
	if err := m.store.Set(storeKey(account), string(b)); err != nil {
		// The rotation already succeeded upstream. Dropping the new token here
		// would leave the account holding the consumed refresh token, and the
		// next refresh would replay it into revocation. Keep the rotated token
		// in memory; the next rotation retries persistence.
		log.Printf("oauth: persist refreshed token for account %s failed; rotated token kept in memory until the next refresh: %v", account, err)
	}
	planChanged := current.PlanType != tok.PlanType
	m.tokens[account] = tok
	onChange := m.onChange
	snapshot := *tok
	m.mu.Unlock()
	if planChanged && onChange != nil {
		// A refresh can be discovered while a runtime publish is already
		// resolving credentials. Notify asynchronously to avoid re-entering the
		// host's publish lock. Ordinary token rotation needs no publish because
		// dynamic callers resolve the token per attempt.
		go onChange(account)
	}
	return &snapshot, nil
}

// SetChangeHandler registers a non-blocking-safe hook used by the host to
// refresh runtime eligibility after login, logout, device completion, or a
// plan-changing token refresh. The callback must not call back while holding
// its own OAuth locks; Manager invokes it after releasing all locks.
func (m *Manager) SetChangeHandler(handler func(string)) {
	m.mu.Lock()
	m.onChange = handler
	m.mu.Unlock()
}

// Identity returns the live access token plus the account's chatgpt_account_id
// claim (needed for the Codex backend's identity header). Errors match
// AccessToken. Token material other than the access token never leaves.
func (m *Manager) Identity(ctx context.Context, account string) (accessToken, accountID string, err error) {
	_, err = m.AccessToken(ctx, account)
	if err != nil {
		return "", "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.tokens[account]
	if current == nil {
		return "", "", fmt.Errorf("account %s logged out while resolving OAuth identity", account)
	}
	return current.AccessToken, current.AccountID, nil
}

// RefreshIdentity forces one refresh after an upstream 401. If another caller
// already replaced the failed access token, that newer identity is reused.
func (m *Manager) RefreshIdentity(ctx context.Context, account, failedAccessToken string) (accessToken, accountID string, err error) {
	tok, err := m.refreshAccount(ctx, account, true, failedAccessToken)
	if err != nil {
		return "", "", err
	}
	return tok.AccessToken, tok.AccountID, nil
}

// AccessToken returns a usable access token for account, refreshing lazily
// when the cached one is within the expiry margin. Egress resolves it for
// every attempt, so a refresh takes effect without republishing config.
func (m *Manager) AccessToken(ctx context.Context, account string) (string, error) {
	m.mu.Lock()
	tok := m.tokens[account]
	needsRefresh := !tok.Valid(m.now())
	m.mu.Unlock()

	if !needsRefresh {
		return tok.AccessToken, nil
	}
	fresh, err := m.refreshAccount(ctx, account, false, "")
	if err != nil {
		return "", err
	}
	return fresh.AccessToken, nil
}

func (m *Manager) refreshAccount(ctx context.Context, account string, force bool, failedAccessToken string) (*Token, error) {
	return m.refreshAccountWhen(ctx, account, force, failedAccessToken, false)
}

// refreshAccountWhen rechecks the proactive due condition under the same lock
// that reserves a refresh. A fresh replacement must not be rotated again just
// because the background scan observed an older token.
func (m *Manager) refreshAccountWhen(ctx context.Context, account string, force bool, failedAccessToken string, proactive bool) (*Token, error) {
	if m.standby {
		return nil, ErrStandby
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	current := m.tokens[account]
	if current == nil || current.RefreshToken == "" {
		m.mu.Unlock()
		return nil, fmt.Errorf("account %s not logged in (complete the OAuth login first)", account)
	}
	if (proactive && !tokenDueForRefresh(current, m.now())) ||
		(!proactive && !force && current.Valid(m.now())) ||
		(force && failedAccessToken != "" && current.AccessToken != failedAccessToken) {
		copyOfCurrent := *current
		m.mu.Unlock()
		return &copyOfCurrent, nil
	}
	// The provider has already rejected this refresh token. Replaying it on
	// every 401 is exactly the pattern that gets rotation treated as abuse,
	// so fail fast and let the account be re-authorized instead.
	if until, failed := m.authFailedUntil[account]; failed && m.now().Before(until) {
		m.mu.Unlock()
		return nil, fmt.Errorf("account %s needs a new OAuth login: its refresh token was rejected (possible causes: account password changed, the same login also used by another client, or a consumed token replayed; retry suppressed until %s)",
			account, until.Format(time.RFC3339))
	}
	if call := m.refreshes[account]; call != nil {
		done := call.done
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-done:
			if call.token == nil {
				return nil, call.err
			}
			snapshot := *call.token
			return &snapshot, call.err
		}
	}
	call := &refreshCall{done: make(chan struct{})}
	m.refreshes[account] = call
	old := *current
	m.mu.Unlock()

	refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	fresh, err := m.refreshWithRetry(refreshCtx, old.RefreshToken)
	cancel()
	replacement := false
	if err != nil {
		// A new login may have replaced the credential while this older refresh
		// was in flight. Prefer that successful replacement over surfacing the
		// stale refresh error.
		m.mu.Lock()
		current := m.tokens[account]
		if current != nil && (current.AccessToken != old.AccessToken || current.RefreshToken != old.RefreshToken) {
			snapshot := *current
			fresh, err = &snapshot, nil
			replacement = true
		}
		m.mu.Unlock()
	}
	if err == nil {
		if !replacement {
			// OpenAI may rotate only part of the token set. Preserve every omitted
			// identity field and, critically, the previous refresh token.
			if fresh.RefreshToken == "" {
				fresh.RefreshToken = old.RefreshToken
			}
			if fresh.IDToken == "" {
				fresh.IDToken = old.IDToken
			}
			if fresh.AccountID == "" {
				fresh.AccountID = old.AccountID
			}
			if fresh.PlanType == "" {
				fresh.PlanType = old.PlanType
			}
			if fresh.Email == "" {
				fresh.Email = old.Email
			}
			fresh, err = m.storeRefreshedToken(account, fresh, &old)
		}
	}
	if err != nil {
		err = fmt.Errorf("refresh token for %s: %w (re-login may be required)", account, err)
	} else {
		log.Printf("oauth: refreshed access token for account %s (expires %s)",
			account, fresh.ExpiresAt.Format(time.RFC3339))
	}

	m.mu.Lock()
	call.token, call.err = fresh, err
	if err != nil {
		var endpointErr *TokenEndpointError
		if errors.As(err, &endpointErr) && endpointErr.NonRetryable() {
			m.noteAuthFailureLocked(account, m.now())
		}
	} else {
		m.clearAuthFailureLocked(account)
	}
	delete(m.refreshes, account)
	close(call.done)
	m.mu.Unlock()
	if fresh == nil {
		return nil, err
	}
	copyOfFresh := *fresh
	return &copyOfFresh, err
}

func (m *Manager) refreshWithRetry(ctx context.Context, refreshToken string) (*Token, error) {
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			if err := m.wait(ctx, time.Duration(attempt)*time.Second); err != nil {
				return nil, err
			}
		}
		fresh, err := Refresh(ctx, m.hc, refreshToken)
		if err == nil {
			return fresh, nil
		}
		last = err
		var endpointErr *TokenEndpointError
		if errors.As(err, &endpointErr) {
			if endpointErr.NonRetryable() {
				break
			}
			// 429 proves the endpoint rejected the request before processing
			// it, so the refresh token was not consumed and a backoff retry
			// is safe. Any other endpoint answer (5xx, 408) may already have
			// rotated the token, and replaying it is what revokes the grant.
			if endpointErr.Status != http.StatusTooManyRequests {
				break
			}
			continue
		}
		if !retryableRefreshTransportError(err) {
			break
		}
	}
	return nil, last
}

// retryableRefreshTransportError reports transport failures that provably
// never delivered the grant request to the token endpoint: DNS resolution
// failures and connection-refused. Timeouts, mid-response resets, and
// post-connect TLS failures leave the request state ambiguous — the endpoint
// may already have rotated the refresh token, and replaying a possibly
// consumed token can revoke the entire grant — so those surface as errors
// for the next scheduled refresh instead of being retried here.
func retryableRefreshTransportError(err error) bool {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return false
	}
	var dnsErr *net.DNSError
	if errors.As(urlErr.Err, &dnsErr) {
		return true
	}
	return errors.Is(urlErr.Err, syscall.ECONNREFUSED)
}

// Logout removes one account's token from memory and the store.
func (m *Manager) Logout(account string) error {
	m.mu.Lock()
	cancel := m.pendingCancels[account]
	delete(m.pendingCancels, account)
	var firstErr error
	if err := m.store.Delete(storeKey(account)); err != nil {
		firstErr = err
	}
	if err := m.store.Delete(pendingStoreKey(account)); err != nil && firstErr == nil {
		firstErr = err
	}
	if err := m.store.Delete(usageStoreKey(account)); err != nil && firstErr == nil {
		firstErr = err
	}
	delete(m.tokens, account)
	delete(m.usageCache, account)
	delete(m.pending, account)
	m.clearAuthFailureLocked(account)
	onChange := m.onChange
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if onChange != nil {
		onChange(account)
	}
	return firstErr
}

// CancelLogin stops one pending browser/device login without logging out an
// already-active token for the same account.
func (m *Manager) CancelLogin(account string) error {
	m.mu.Lock()
	cancel := m.pendingCancels[account]
	delete(m.pendingCancels, account)
	err := m.store.Delete(pendingStoreKey(account))
	delete(m.pending, account)
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return err
}

// PendingLogin returns a resumable authorize URL without exposing the PKCE
// verifier or anti-CSRF state. Persisted pending attempts survive restarts.
func (m *Manager) PendingLogin(account string) (authorizeURL string, started time.Time, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pending := m.pending[account]
	if pending == nil || pending.Mode == loginModeDevice || m.now().Sub(pending.Started) > 15*time.Minute {
		return "", time.Time{}, false
	}
	sum := sha256.Sum256([]byte(pending.Verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	return LoginURL(pending.State, challenge), pending.Started, true
}

// LoginStatus returns redacted progress for a pending browser/device login.
func (m *Manager) LoginStatus(account string) (LoginStatusView, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pending := m.pending[account]
	if pending == nil {
		return LoginStatusView{}, false
	}
	view := pending.view(account)
	if pending.Mode != loginModeDevice {
		sum := sha256.Sum256([]byte(pending.Verifier))
		view.AuthorizeURL = LoginURL(pending.State, base64.RawURLEncoding.EncodeToString(sum[:]))
	}
	return view, true
}

// LoginStatuses returns every pending login in stable account order.
func (m *Manager) LoginStatuses() []LoginStatusView {
	m.mu.Lock()
	accounts := make([]string, 0, len(m.pending))
	for account := range m.pending {
		accounts = append(accounts, account)
	}
	m.mu.Unlock()
	sort.Strings(accounts)
	out := make([]LoginStatusView, 0, len(accounts))
	for _, account := range accounts {
		if status, ok := m.LoginStatus(account); ok {
			out = append(out, status)
		}
	}
	return out
}

func (p *pendingLogin) view(account string) LoginStatusView {
	mode := p.Mode
	if mode == "" {
		mode = loginModeBrowser
	}
	status := p.Status
	if status == "" {
		status = loginStatusPending
	}
	return LoginStatusView{
		Account: account, Mode: mode, Status: status, UserCode: p.UserCode,
		VerificationURL: p.VerificationURL, StartedAt: p.Started.Format(time.RFC3339), Error: p.Error,
	}
}

// LoginStatusView is safe to return from management APIs. PKCE verifiers,
// device authorization ids, tokens, and OAuth state never leave the manager.
type LoginStatusView struct {
	Account         string `json:"account"`
	Mode            string `json:"mode"`
	Status          string `json:"status"`
	AuthorizeURL    string `json:"authorize_url,omitempty"`
	VerificationURL string `json:"verification_url,omitempty"`
	UserCode        string `json:"user_code,omitempty"`
	StartedAt       string `json:"started_at,omitempty"`
	Error           string `json:"error,omitempty"`
}

// Accounts returns redacted status for every known account (persisted or
// cached): identity and freshness only, never token material.
func (m *Manager) Accounts() []AccountView {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]AccountView, 0, len(m.tokens))
	for acct, tok := range m.tokens {
		v := AccountView{Name: acct, Email: tok.Email, PlanType: tok.PlanType,
			AccountID: tok.AccountID}
		if !tok.ExpiresAt.IsZero() {
			v.ExpiresAt = tok.ExpiresAt.Format(time.RFC3339)
			v.Expired = !m.now().Before(tok.ExpiresAt)
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Account returns one active account's redacted identity.
func (m *Manager) Account(account string) (AccountView, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tok := m.tokens[account]
	if tok == nil {
		return AccountView{}, false
	}
	view := AccountView{Name: account, Email: tok.Email, PlanType: tok.PlanType, AccountID: tok.AccountID}
	if !tok.ExpiresAt.IsZero() {
		view.ExpiresAt = tok.ExpiresAt.Format(time.RFC3339)
		view.Expired = !m.now().Before(tok.ExpiresAt)
	}
	return view, true
}

// AccountView is the redacted per-account status reported by the management
// API (domain.OAuthAccountStatus carries the JSON shape; this mirrors it to
// avoid importing domain here).
type AccountView = struct {
	Name      string `json:"name"`
	Email     string `json:"email,omitempty"`
	PlanType  string `json:"plan_type,omitempty"`
	AccountID string `json:"account_id,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	Expired   bool   `json:"expired"`
}

// validAccount enforces the same charset as config ids (accounts end up in
// target ids, store names, and URLs).
func validAccount(s string) error {
	if s == "" || len(s) > 64 {
		return fmt.Errorf("account name must be 1-64 chars")
	}
	for i, c := range s {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			(i > 0 && (c == '_' || c == '-'))
		if !ok {
			return fmt.Errorf("account name %q: only [a-zA-Z0-9_-] allowed (not first char)", s)
		}
	}
	return nil
}

func newState() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b[:]), nil
}
