// Portions reference/adapt CLIProxyAPI (MIT); see NOTICE and
// LICENSES/CLIProxyAPI/LICENSE for retained upstream attribution.
package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// UsageURL is the ChatGPT backend endpoint used by the official Codex client
// to read the signed-in account's current rate-limit and credit snapshot.
const UsageURL = "https://chatgpt.com/backend-api/wham/usage"

var usageURL = UsageURL

// SetUsageURL overrides the usage endpoint. It exists for local integration
// tests; production callers should leave the official endpoint unchanged.
func SetUsageURL(value string) { usageURL = value }

// CurrentUsageURL returns the active usage endpoint.
func CurrentUsageURL() string { return usageURL }

// UsageSnapshot is a redacted, bounded view of the Codex subscription usage
// response. It deliberately contains no credential material.
type UsageSnapshot struct {
	AccountID             string                     `json:"account_id,omitempty"`
	PlanType              string                     `json:"plan_type,omitempty"`
	RateLimit             *UsageRateLimit            `json:"rate_limit,omitempty"`
	Credits               *UsageCredits              `json:"credits,omitempty"`
	SpendControl          *UsageSpendControl         `json:"spend_control,omitempty"`
	AdditionalRateLimits  []UsageAdditionalRateLimit `json:"additional_rate_limits"`
	RateLimitReachedType  *UsageRateLimitReached     `json:"rate_limit_reached_type,omitempty"`
	RateLimitResetCredits *UsageResetCredits         `json:"rate_limit_reset_credits,omitempty"`
	ObservedAt            time.Time                  `json:"observed_at"`
}

type UsageRateLimit struct {
	Allowed         bool         `json:"allowed"`
	LimitReached    bool         `json:"limit_reached"`
	PrimaryWindow   *UsageWindow `json:"primary_window,omitempty"`
	SecondaryWindow *UsageWindow `json:"secondary_window,omitempty"`
}

type UsageWindow struct {
	missingUsedPercent bool
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int64   `json:"limit_window_seconds"`
	ResetAfterSeconds  int64   `json:"reset_after_seconds"`
	ResetAt            int64   `json:"reset_at"`
}

// Missing percentages must not become an authorization to spend at 0%.
func (w *UsageWindow) UnmarshalJSON(data []byte) error {
	type plain UsageWindow
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var present struct {
		UsedPercent *float64 `json:"used_percent"`
	}
	if err := json.Unmarshal(data, &present); err != nil {
		return err
	}
	*w = UsageWindow(decoded)
	w.missingUsedPercent = present.UsedPercent == nil
	return nil
}

type UsageCredits struct {
	HasCredits bool   `json:"has_credits"`
	Unlimited  bool   `json:"unlimited"`
	Balance    string `json:"balance,omitempty"`
}

type UsageSpendControl struct {
	Reached         bool                    `json:"reached"`
	IndividualLimit *UsageSpendControlLimit `json:"individual_limit,omitempty"`
}

type UsageSpendControlLimit struct {
	Source            string  `json:"source,omitempty"`
	Limit             string  `json:"limit,omitempty"`
	Used              string  `json:"used,omitempty"`
	Remaining         string  `json:"remaining,omitempty"`
	UsedPercent       float64 `json:"used_percent"`
	RemainingPercent  float64 `json:"remaining_percent"`
	ResetAfterSeconds int64   `json:"reset_after_seconds"`
	ResetAt           int64   `json:"reset_at"`
}

type UsageAdditionalRateLimit struct {
	LimitName      string          `json:"limit_name"`
	MeteredFeature string          `json:"metered_feature,omitempty"`
	RateLimit      *UsageRateLimit `json:"rate_limit,omitempty"`
}

type UsageRateLimitReached struct {
	Type string `json:"type"`
}

type UsageResetCredits struct {
	AvailableCount   int64              `json:"available_count"`
	Credits          []UsageResetCredit `json:"credits"`
	TotalEarnedCount *int64             `json:"total_earned_count,omitempty"`
	DetailsError     string             `json:"details_error,omitempty"`
}

type importedAuthJSON struct {
	UseSIWC      json.RawMessage `json:"use_siwc"`
	ClientID     string          `json:"client_id"`
	Type         string          `json:"type"`
	AuthMode     string          `json:"auth_mode"`
	OpenAIAPIKey any             `json:"OPENAI_API_KEY"`
	Tokens       *struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		AccountID    string `json:"account_id"`
	} `json:"tokens"`
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id"`
	Email        string `json:"email"`
	PlanType     string `json:"plan_type"`
	LastRefresh  string `json:"last_refresh"`
	Expired      string `json:"expired"`
}

// ParseAuthJSON accepts both the official $CODEX_HOME/auth.json shape and the
// flattened Codex credential JSON used by CLIProxyAPI. Only the token fields
// required by Ferry are retained; the uploaded document itself is never
// persisted.
func ParseAuthJSON(raw []byte, now time.Time) (*Token, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("auth.json is empty")
	}
	if len(raw) > 128<<10 {
		return nil, fmt.Errorf("auth.json exceeds the 128 KiB limit")
	}
	var input importedAuthJSON
	if err := json.Unmarshal(raw, &input); err != nil {
		return nil, fmt.Errorf("decode auth.json: %w", err)
	}
	if kind := strings.ToLower(strings.TrimSpace(input.Type)); kind != "" && kind != "codex" {
		return nil, fmt.Errorf("credential type %q is not a Codex auth file", input.Type)
	}

	// CLIProxyAPI v8 keeps legacy Codex credentials alongside an independent
	// SIWC credential. Never silently switch an explicitly selected SIWC file
	// back to its legacy tokens: the client identity and endpoints differ.
	if len(input.UseSIWC) > 0 {
		var enabled bool
		if err := json.Unmarshal(input.UseSIWC, &enabled); err != nil {
			var value string
			if json.Unmarshal(input.UseSIWC, &value) != nil {
				return nil, fmt.Errorf("auth.json use_siwc must be a boolean")
			}
			parsed, err := strconv.ParseBool(strings.TrimSpace(value))
			if err != nil {
				return nil, fmt.Errorf("auth.json use_siwc must be a boolean")
			}
			enabled = parsed
		}
		if enabled {
			return nil, fmt.Errorf("SIWC credentials are not supported; import a legacy Codex OAuth credential instead")
		}
	}
	if clientID := strings.TrimSpace(input.ClientID); clientID != "" && clientID != ClientID {
		return nil, fmt.Errorf("auth.json uses an unsupported OAuth client_id")
	}

	idToken, accessToken, refreshToken, accountID := input.IDToken, input.AccessToken, input.RefreshToken, input.AccountID
	if input.Tokens != nil {
		idToken = input.Tokens.IDToken
		accessToken = input.Tokens.AccessToken
		refreshToken = input.Tokens.RefreshToken
		accountID = input.Tokens.AccountID
	}
	idToken = strings.TrimSpace(idToken)
	accessToken = strings.TrimSpace(accessToken)
	refreshToken = strings.TrimSpace(refreshToken)
	accountID = strings.TrimSpace(accountID)
	if accessToken == "" || refreshToken == "" {
		return nil, fmt.Errorf("auth.json must contain ChatGPT access_token and refresh_token values")
	}
	for name, value := range map[string]string{
		"access_token": accessToken, "refresh_token": refreshToken, "id_token": idToken,
	} {
		if len(value) > 64<<10 || strings.ContainsAny(value, "\r\n") {
			return nil, fmt.Errorf("auth.json contains an invalid %s", name)
		}
	}
	if len(accountID) > 512 || strings.ContainsAny(accountID, "\r\n") {
		return nil, fmt.Errorf("auth.json contains an invalid account_id")
	}
	if now.IsZero() {
		now = time.Now()
	}
	token := &Token{AccessToken: accessToken, RefreshToken: refreshToken, IDToken: idToken}
	applyIDTokenClaims(token)
	if accountID != "" {
		token.AccountID = accountID
	}
	if email := strings.TrimSpace(input.Email); email != "" && len(email) <= 512 && !strings.ContainsAny(email, "\r\n") {
		token.Email = email
	}
	if plan := strings.TrimSpace(input.PlanType); plan != "" && len(plan) <= 128 && !strings.ContainsAny(plan, "\r\n") {
		token.PlanType = plan
	}
	if refreshed, ok := parseImportTime(input.LastRefresh); ok {
		token.ObtainedAt = refreshed
	}
	if expiresAt, ok := jwtExpiresAt(accessToken); ok {
		token.ExpiresAt = expiresAt
	} else if expiresAt, ok := parseImportTime(input.Expired); ok {
		token.ExpiresAt = expiresAt
	} else {
		// An opaque access token without an expiry is refreshed before first use.
		// This avoids guessing a lifetime while still accepting older exports.
		token.ExpiresAt = now.Add(-time.Second)
	}
	return token, nil
}

func parseImportTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	return parsed, err == nil
}

func jwtExpiresAt(value string) (time.Time, bool) {
	parts := strings.Split(value, ".")
	if len(parts) < 3 || parts[1] == "" {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
	}
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		ExpiresAt int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.ExpiresAt <= 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.ExpiresAt, 0), true
}

// ImportAuthJSON atomically replaces one managed account's credential with
// the validated token subset from an auth.json document.
func (m *Manager) ImportAuthJSON(account string, raw []byte) (AccountView, error) {
	return m.importAuthJSON(account, raw, false)
}

// ImportNewAuthJSON refuses to replace an existing account during migration.
func (m *Manager) ImportNewAuthJSON(account string, raw []byte) (AccountView, error) {
	if m.standby {
		return AccountView{}, ErrStandby
	}
	return m.importAuthJSON(account, raw, true)
}

func (m *Manager) importAuthJSON(account string, raw []byte, createOnly bool) (AccountView, error) {
	if err := validAccount(account); err != nil {
		return AccountView{}, err
	}
	token, err := ParseAuthJSON(raw, m.now())
	if err != nil {
		return AccountView{}, err
	}
	token.ImportedDormant = createOnly
	encoded, err := json.Marshal(token) // #nosec G117 -- OAuth token persistence boundary: the marshaled bytes go only to the private token store, never to logs or responses
	if err != nil {
		return AccountView{}, err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return AccountView{}, fmt.Errorf("OAuth manager is closed")
	}
	save := m.store.Set
	if createOnly {
		save = m.store.SetIfAbsent
	}
	if err := save(storeKey(account), string(encoded)); err != nil {
		m.mu.Unlock()
		return AccountView{}, fmt.Errorf("persist imported token: %w", err)
	}
	m.tokens[account] = token
	delete(m.usageCache, account)
	if err := m.store.Delete(usageStoreKey(account)); err != nil {
		log.Printf("oauth account %s: remove stale usage cache after import: %v", account, err)
	}
	delete(m.pending, account)
	m.clearAuthFailureLocked(account)
	cancel := m.pendingCancels[account]
	delete(m.pendingCancels, account)
	onChange := m.onChange
	cleanupErr := m.store.Delete(pendingStoreKey(account))
	view := AccountView{Name: account, Email: token.Email, PlanType: token.PlanType, AccountID: token.AccountID}
	if !token.ExpiresAt.IsZero() {
		view.ExpiresAt = token.ExpiresAt.Format(time.RFC3339)
		view.Expired = !m.now().Before(token.ExpiresAt)
	}
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if onChange != nil {
		onChange(account)
	}
	if cleanupErr != nil {
		return AccountView{}, fmt.Errorf("remove pending login: %w", cleanupErr)
	}
	return view, nil
}

// Usage queries the official Codex account usage endpoint. A stale access
// token gets one normal OAuth refresh/retry; redirects are refused so bearer
// credentials can never be forwarded to another origin.
func (m *Manager) Usage(ctx context.Context, account string) (*UsageSnapshot, error) {
	if err := validAccount(account); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	accessToken, accountID, err := m.Identity(ctx, account)
	if err != nil {
		return nil, err
	}
	snapshot, status, err := m.readUsage(ctx, accessToken, accountID)
	if status == http.StatusUnauthorized {
		var refreshErr error
		accessToken, accountID, refreshErr = m.RefreshIdentity(ctx, account, accessToken)
		if refreshErr != nil {
			return nil, refreshErr
		}
		snapshot, status, err = m.readUsage(ctx, accessToken, accountID)
	}
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("Codex usage endpoint returned HTTP %d", status)
	}
	if snapshot.AccountID != "" && snapshot.AccountID != accountID {
		return nil, fmt.Errorf("Codex usage account identity mismatch")
	}
	snapshot.AccountID = accountID
	if snapshot.RateLimitResetCredits != nil {
		details, detailErr := m.readResetCredits(ctx, accessToken, accountID)
		if detailErr == nil {
			snapshot.RateLimitResetCredits = details
		} else {
			snapshot.RateLimitResetCredits.Credits = nil
			snapshot.RateLimitResetCredits.DetailsError = "Credit details unavailable; refresh to retry."
		}
	}
	if snapshot.AdditionalRateLimits == nil {
		snapshot.AdditionalRateLimits = []UsageAdditionalRateLimit{}
	} else if len(snapshot.AdditionalRateLimits) > 64 {
		snapshot.AdditionalRateLimits = snapshot.AdditionalRateLimits[:64]
	}
	snapshot.ObservedAt = m.now()
	m.mu.Lock()
	if current := m.tokens[account]; current != nil && current.AccessToken == accessToken && current.AccountID == accountID {
		if previous := m.usageCache[account]; previous != nil && previous.AccountID == accountID && previous.RateLimit != nil && snapshot.RateLimit != nil {
			if snapshot.RateLimit.PrimaryWindow == nil && previous.RateLimit.PrimaryWindow != nil {
				snapshot.RateLimit.PrimaryWindow = &UsageWindow{missingUsedPercent: true}
			}
			if snapshot.RateLimit.SecondaryWindow == nil && previous.RateLimit.SecondaryWindow != nil {
				snapshot.RateLimit.SecondaryWindow = &UsageWindow{missingUsedPercent: true}
			}
			snapshot.RateLimit.PrimaryWindow = conservativeUsageWindow(previous.RateLimit.PrimaryWindow, snapshot.RateLimit.PrimaryWindow)
			snapshot.RateLimit.SecondaryWindow = conservativeUsageWindow(previous.RateLimit.SecondaryWindow, snapshot.RateLimit.SecondaryWindow)
		}
		encoded, err := json.Marshal(snapshot)
		if err != nil {
			m.mu.Unlock()
			return nil, fmt.Errorf("encode Codex usage cache: %w", err)
		}
		if err := m.store.Set(usageStoreKey(account), string(encoded)); err != nil {
			m.mu.Unlock()
			return nil, fmt.Errorf("persist Codex usage cache: %w", err)
		}
		m.usageCache[account] = snapshot
		if m.usageVerified == nil {
			m.usageVerified = map[string]time.Time{}
		}
		m.usageVerified[account] = snapshot.ObservedAt
	} else {
		m.mu.Unlock()
		return nil, fmt.Errorf("OAuth identity changed while reading usage; refresh again")
	}
	m.mu.Unlock()
	return snapshot, nil
}

// CachedUsage returns the latest successful usage snapshot without contacting
// the upstream service. The cached value contains no credential material and
// is immutable after publication.
func (m *Manager) CachedUsage(account string) (*UsageSnapshot, error) {
	if err := validAccount(account); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	snapshot := m.usageCache[account]
	if snapshot == nil {
		return nil, nil
	}
	copy := *snapshot
	return &copy, nil
}

func (m *Manager) readUsage(ctx context.Context, accessToken, accountID string) (*UsageSnapshot, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageURL, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("create Codex usage request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", "codex-cli")
	if accountID != "" {
		req.Header.Set("ChatGPT-Account-Id", accountID)
	}
	client := *m.hc
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("query Codex usage: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, resp.StatusCode, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (256<<10)+1))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read Codex usage response: %w", err)
	}
	if len(body) > 256<<10 {
		return nil, resp.StatusCode, fmt.Errorf("Codex usage response exceeds 256 KiB")
	}
	var snapshot UsageSnapshot
	if err := json.Unmarshal(body, &snapshot); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("decode Codex usage response: %w", err)
	}
	return &snapshot, resp.StatusCode, nil
}
