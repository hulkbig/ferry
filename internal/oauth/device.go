// Portions reference/adapt CLIProxyAPI (MIT); see NOTICE and
// LICENSES/CLIProxyAPI/LICENSE for retained upstream attribution.
package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	loginModeBrowser   = "browser"
	loginModeDevice    = "device"
	loginStatusPending = "pending"
	loginStatusPolling = "polling"
	loginStatusFailed  = "failed"

	DeviceVerificationURL = "https://auth.openai.com/codex/device"
	deviceLoginTimeout    = 15 * time.Minute
	deviceDefaultInterval = 5
	// devicePollMaxConsecutiveErrors ends an attempt only after the polling
	// endpoint has been unreachable for a sustained stretch, not on one blip.
	devicePollMaxConsecutiveErrors = 5
)

var (
	deviceUserCodeURL = "https://auth.openai.com/api/accounts/deviceauth/usercode"
	deviceTokenURL    = "https://auth.openai.com/api/accounts/deviceauth/token" // #nosec G101 -- public OAuth endpoint, not a credential
)

// SetDeviceEndpoints overrides the Codex device endpoints for tests and
// private compatible deployments. Call the returned restore function.
func SetDeviceEndpoints(userCodeURL, pollURL string) func() {
	oldUserCode, oldPoll := deviceUserCodeURL, deviceTokenURL
	deviceUserCodeURL, deviceTokenURL = userCodeURL, pollURL
	return func() { deviceUserCodeURL, deviceTokenURL = oldUserCode, oldPoll }
}

type deviceCodeResponse struct {
	DeviceAuthID string          `json:"device_auth_id"`
	UserCode     string          `json:"user_code"`
	UserCodeAlt  string          `json:"usercode"`
	Interval     json.RawMessage `json:"interval"`
}

type deviceAuthorization struct {
	AuthorizationCode string `json:"authorization_code"`
	CodeVerifier      string `json:"code_verifier"`
	CodeChallenge     string `json:"code_challenge"`
}

// StartDeviceLogin starts the non-browser Codex flow and returns immediately
// with the URL/code. Polling and the final token exchange continue in the
// background, survive process restarts through the persisted pending record,
// and can be observed/cancelled through LoginStatus/CancelLogin.
func (m *Manager) StartDeviceLogin(ctx context.Context, account string) (LoginStatusView, error) {
	if m.standby {
		return LoginStatusView{}, ErrStandby
	}
	if err := validAccount(account); err != nil {
		return LoginStatusView{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return LoginStatusView{}, fmt.Errorf("OAuth manager is closed")
	}
	response, err := m.requestDeviceCode(ctx)
	if err != nil {
		return LoginStatusView{}, err
	}
	userCode := strings.TrimSpace(response.UserCode)
	if userCode == "" {
		userCode = strings.TrimSpace(response.UserCodeAlt)
	}
	deviceAuthID := strings.TrimSpace(response.DeviceAuthID)
	if userCode == "" || deviceAuthID == "" {
		return LoginStatusView{}, fmt.Errorf("codex device flow did not return required fields")
	}
	pending := &pendingLogin{
		Mode: loginModeDevice, DeviceAuthID: deviceAuthID, UserCode: userCode,
		VerificationURL: DeviceVerificationURL,
		IntervalSeconds: parsePositiveSeconds(response.Interval, deviceDefaultInterval),
		Status:          loginStatusPolling, Started: m.now(),
	}
	if err := m.persistPending(account, pending); err != nil {
		return LoginStatusView{}, err
	}
	m.resumeDeviceLogin(account, pending)
	return pending.view(account), nil
}

func (m *Manager) persistPending(account string, pending *pendingLogin) error {
	raw, err := json.Marshal(pending)
	if err != nil {
		return err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return fmt.Errorf("OAuth manager is closed")
	}
	if err := m.store.Set(pendingStoreKey(account), string(raw)); err != nil {
		m.mu.Unlock()
		return fmt.Errorf("persist pending login: %w", err)
	}
	oldCancel := m.pendingCancels[account]
	delete(m.pendingCancels, account)
	m.pending[account] = pending
	m.mu.Unlock()
	if oldCancel != nil {
		oldCancel()
	}
	return nil
}

func (m *Manager) requestDeviceCode(ctx context.Context) (*deviceCodeResponse, error) {
	body, err := json.Marshal(map[string]string{"client_id": ClientID})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, deviceUserCodeURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create codex device request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := m.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request codex device code: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, fmt.Errorf("read codex device code response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("codex device code request failed with status %d: %s",
			resp.StatusCode, safeProviderMessage(raw))
	}
	var parsed deviceCodeResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode codex device code response: %w", err)
	}
	return &parsed, nil
}

func (m *Manager) resumeDeviceLogin(account string, pending *pendingLogin) {
	if pending == nil || pending.DeviceAuthID == "" {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		cancel()
		return
	}
	current := m.pending[account]
	if current == nil || current.Mode != loginModeDevice || current.DeviceAuthID != pending.DeviceAuthID {
		m.mu.Unlock()
		cancel()
		return
	}
	if old := m.pendingCancels[account]; old != nil {
		old()
	}
	m.pendingCancels[account] = cancel
	m.mu.Unlock()
	snapshot := *pending
	go m.runDeviceLogin(ctx, account, &snapshot)
}

func (m *Manager) runDeviceLogin(ctx context.Context, account string, pending *pendingLogin) {
	deadline := pending.Started.Add(deviceLoginTimeout)
	interval := time.Duration(pending.IntervalSeconds) * time.Second
	if interval <= 0 {
		interval = time.Duration(deviceDefaultInterval) * time.Second
	}
	// A device login owns a 15-minute window during which the user is typing a
	// code in a browser. One transient poll failure must not throw that away,
	// so only a sustained run of failures ends the attempt.
	consecutiveErrors := 0
	for {
		if !m.now().Before(deadline) {
			m.failPending(account, pending.DeviceAuthID, "codex device authentication timed out after 15 minutes")
			return
		}
		authorization, ready, err := m.pollDeviceOnce(ctx, pending.DeviceAuthID, pending.UserCode)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			consecutiveErrors++
			if consecutiveErrors >= devicePollMaxConsecutiveErrors {
				m.failPending(account, pending.DeviceAuthID, err.Error())
				return
			}
			if waitErr := m.wait(ctx, interval); waitErr != nil {
				return
			}
			continue
		}
		consecutiveErrors = 0
		if ready {
			if strings.TrimSpace(authorization.AuthorizationCode) == "" ||
				strings.TrimSpace(authorization.CodeVerifier) == "" ||
				strings.TrimSpace(authorization.CodeChallenge) == "" {
				m.failPending(account, pending.DeviceAuthID, "codex device token response missing required fields")
				return
			}
			exchangeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			token, exchangeErr := ExchangeWithRedirect(exchangeCtx, m.hc,
				authorization.AuthorizationCode, authorization.CodeVerifier, DeviceRedirectURI)
			cancel()
			if exchangeErr != nil {
				m.failPending(account, pending.DeviceAuthID, "device token exchange: "+exchangeErr.Error())
				return
			}
			if err := m.storeToken(account, token, loginModeDevice, pending.DeviceAuthID); err != nil {
				m.failPending(account, pending.DeviceAuthID, "store device token: "+err.Error())
			}
			return
		}
		if err := m.wait(ctx, interval); err != nil {
			return
		}
	}
}

func (m *Manager) pollDeviceOnce(ctx context.Context, deviceAuthID, userCode string) (*deviceAuthorization, bool, error) {
	body, err := json.Marshal(map[string]string{"device_auth_id": deviceAuthID, "user_code": userCode})
	if err != nil {
		return nil, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, deviceTokenURL, bytes.NewReader(body))
	if err != nil {
		return nil, false, fmt.Errorf("create codex device poll request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := m.hc.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("poll codex device token: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, false, fmt.Errorf("read codex device poll response: %w", err)
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false, fmt.Errorf("codex device token polling failed with status %d: %s",
			resp.StatusCode, safeProviderMessage(raw, deviceAuthID, userCode))
	}
	var parsed deviceAuthorization
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, false, fmt.Errorf("decode codex device token response: %w", err)
	}
	return &parsed, true, nil
}

func (m *Manager) failPending(account, deviceAuthID, message string) {
	message = strings.TrimSpace(message)
	if len(message) > 512 {
		message = message[:512]
	}
	m.mu.Lock()
	pending := m.pending[account]
	if pending == nil || pending.DeviceAuthID != deviceAuthID {
		m.mu.Unlock()
		return
	}
	pending.Status = loginStatusFailed
	pending.Error = message
	snapshot := *pending
	delete(m.pendingCancels, account)
	raw, err := json.Marshal(&snapshot)
	if err == nil {
		_ = m.store.Set(pendingStoreKey(account), string(raw))
	}
	m.mu.Unlock()
}

func safeProviderMessage(raw []byte, sensitive ...string) string {
	var providerErr struct {
		Error            string `json:"error"`
		Code             string `json:"code"`
		ErrorDescription string `json:"error_description"`
		Message          string `json:"message"`
	}
	_ = json.Unmarshal(raw, &providerErr)
	message := firstNonEmptyOAuth(providerErr.ErrorDescription, providerErr.Message,
		providerErr.Error, providerErr.Code)
	if message == "" {
		message = "provider rejected the request"
	}
	for _, secret := range sensitive {
		if secret = strings.TrimSpace(secret); secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	if len(message) > 512 {
		message = message[:512]
	}
	return message
}

func firstNonEmptyOAuth(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
