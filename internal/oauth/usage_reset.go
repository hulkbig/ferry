package oauth

// Wire contract: official Codex backend-client client/rate_limit_resets.rs.
// This module never invokes scheduler resets, cooldown clears or local quotas.
import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

type UsageResetCredit struct {
	ID              string  `json:"id"`
	ResetType       string  `json:"reset_type"`
	Status          string  `json:"status"`
	GrantedAt       string  `json:"granted_at"`
	ExpiresAt       *string `json:"expires_at"`
	RedeemStartedAt *string `json:"redeem_started_at,omitempty"`
	RedeemedAt      *string `json:"redeemed_at,omitempty"`
	Title           string  `json:"title,omitempty"`
	Description     string  `json:"description,omitempty"`
}

type UsageResetBinding struct {
	ProviderID   string `json:"provider_id"`
	ConnectionID string `json:"connection_id"`
	Account      string `json:"account"`
}

type UsageResetRequest struct {
	ID string `json:"id"`
	UsageResetBinding
	AccountID    string           `json:"account_id"`
	Email        string           `json:"email,omitempty"`
	Credit       UsageResetCredit `json:"credit"`
	RateLimit    *UsageRateLimit  `json:"rate_limit,omitempty"`
	CreatedBy    string           `json:"created_by"`
	CreatedAt    time.Time        `json:"created_at"`
	ExpiresAt    time.Time        `json:"expires_at"`
	Status       string           `json:"status"`
	WindowsReset int64            `json:"windows_reset"`
	Message      string           `json:"message,omitempty"`
}

type usageResetRecord struct {
	Request  UsageResetRequest `json:"request"`
	Identity string            `json:"identity"`
}

const usageResetPrefix = "oauth:usage-reset:"

func resetUnresolved(status string) bool {
	return status == "pending" || status == "in_flight" || status == "uncertain"
}

func (m *Manager) readResetCredits(ctx context.Context, token, accountID string) (*UsageResetCredits, error) {
	var credits UsageResetCredits
	if err := m.resetHTTP(ctx, http.MethodGet, "", token, accountID, nil, &credits); err != nil {
		return nil, err
	}
	// Count-only responses must never become an empty complete list.
	if credits.Credits == nil || credits.AvailableCount < 0 {
		return nil, fmt.Errorf("credit detail list not reported")
	}
	return &credits, nil
}

func (m *Manager) resetHTTP(ctx context.Context, method, suffix, token, accountID string, payload, out any) error {
	if m.standby && method != http.MethodGet {
		return ErrStandby
	}
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	endpoint := strings.TrimSuffix(usageURL, "/usage") + "/rate-limit-reset-credits" + suffix
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return fmt.Errorf("create usage-reset request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("ChatGPT-Account-Id", accountID)
	req.Header.Set("User-Agent", "codex-cli")
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := *m.hc
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("usage-reset upstream request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("usage-reset upstream HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (256<<10)+1))
	if err != nil || len(raw) > 256<<10 {
		return fmt.Errorf("invalid usage-reset response size")
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("invalid usage-reset response")
	}
	return nil
}

// Bind workspace AND user: different users may share a workspace.
// Opaque credentials use an exact token fingerprint, failing closed on refresh.
func resetIdentity(t *Token) string {
	subject := ""
	for _, jwt := range []string{t.AccessToken, t.IDToken} {
		parts := strings.Split(jwt, ".")
		if len(parts) < 2 {
			continue
		}
		raw, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			continue
		}
		var claims struct {
			Sub string `json:"sub"`
		}
		if json.Unmarshal(raw, &claims) == nil && claims.Sub != "" {
			subject = claims.Sub
			break
		}
	}
	if subject == "" {
		subject = t.AccessToken
	}
	sum := sha256.Sum256([]byte(t.AccountID + "\x00" + subject))
	return hex.EncodeToString(sum[:])
}

func (m *Manager) resetToken(ctx context.Context, account string) (*Token, error) {
	token, accountID, err := m.Identity(ctx, account)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.tokens[account]
	if current == nil || current.AccessToken != token || current.AccountID != accountID || accountID == "" {
		return nil, fmt.Errorf("OAuth identity changed or account ID unavailable")
	}
	copy := *current
	return &copy, nil
}

func (m *Manager) checkResetCredit(ctx context.Context, token *Token, creditID string) (UsageResetCredit, *UsageRateLimit, error) {
	usage, status, err := m.readUsage(ctx, token.AccessToken, token.AccountID)
	if err != nil || status != http.StatusOK {
		return UsageResetCredit{}, nil, fmt.Errorf("fresh Codex usage unavailable")
	}
	if usage.AccountID != "" && usage.AccountID != token.AccountID {
		return UsageResetCredit{}, nil, fmt.Errorf("usage identity mismatch")
	}
	// Usage is shown for review, not a local redemption threshold. The upstream
	// decides whether there is anything to reset after explicit confirmation.
	details, err := m.readResetCredits(ctx, token.AccessToken, token.AccountID)
	if err != nil {
		return UsageResetCredit{}, nil, err
	}
	var found *UsageResetCredit
	for i := range details.Credits {
		c := &details.Credits[i]
		if c.ID == creditID {
			if found != nil {
				return UsageResetCredit{}, nil, fmt.Errorf("ambiguous credit ID")
			}
			found = c
		}
	}
	if found == nil || found.Status != "available" || found.ResetType != "codex_rate_limits" || details.AvailableCount < 1 {
		return UsageResetCredit{}, nil, fmt.Errorf("selected Codex usage-reset credit is not available")
	}
	if found.ExpiresAt != nil {
		expiry, err := time.Parse(time.RFC3339Nano, *found.ExpiresAt)
		if err != nil || !m.now().Before(expiry) {
			return UsageResetCredit{}, nil, fmt.Errorf("selected credit expired or has an invalid expiration")
		}
	}
	return *found, usage.RateLimit, nil
}

func (m *Manager) resetRecords() ([]usageResetRecord, error) {
	records := []usageResetRecord{}
	entries, err := m.store.EntriesWithPrefix(usageResetPrefix)
	if err != nil {
		return nil, fmt.Errorf("usage-reset journal unavailable")
	}
	for key, raw := range entries {
		var record usageResetRecord
		if json.Unmarshal([]byte(raw), &record) != nil || record.Request.ID == "" || record.Identity == "" || key != usageResetPrefix+record.Request.ID || record.Request.AccountID == "" || record.Request.Credit.ID == "" {
			return nil, fmt.Errorf("usage-reset journal unreadable; consumption disabled")
		}
		switch record.Request.Status {
		case "pending", "in_flight", "uncertain", "expired", "canceled", "reset", "already_redeemed", "nothing_to_reset", "no_credit":
		default:
			return nil, fmt.Errorf("usage-reset journal status unreadable; consumption disabled")
		}
		// Interrupted POSTs stay uncertain after restart; they can only reuse their ID.
		if record.Request.Status == "in_flight" {
			record.Request.Status = "uncertain"
		}
		if record.Request.Status == "pending" && !m.now().Before(record.Request.ExpiresAt) {
			record.Request.Status = "expired"
		}
		records = append(records, record)
	}
	return records, nil
}

func (m *Manager) saveReset(record usageResetRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return m.store.Set(usageResetPrefix+record.Request.ID, string(raw))
}

// UsageResetRequests is a local journal read; it never contacts ChatGPT.
func (m *Manager) UsageResetRequests() ([]UsageResetRequest, error) {
	m.resetMu.Lock()
	defer m.resetMu.Unlock()
	records, err := m.resetRecords()
	if err != nil {
		return nil, err
	}
	requests := make([]UsageResetRequest, 0, len(records))
	for _, record := range records {
		requests = append(requests, record.Request)
	}
	sort.Slice(requests, func(i, j int) bool { return requests[i].CreatedAt.After(requests[j].CreatedAt) })
	return requests, nil
}

// PrepareUsageReset only reads upstream state and persists a pending intent.
func (m *Manager) PrepareUsageReset(ctx context.Context, binding UsageResetBinding, creditID, expectedAccountID, actor string) (*UsageResetRequest, error) {
	if m.standby {
		return nil, ErrStandby
	}
	if creditID == "" || len(creditID) > 256 || expectedAccountID == "" {
		return nil, fmt.Errorf("explicit credit_id and account_id are required")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	token, err := m.resetToken(ctx, binding.Account)
	if err != nil {
		return nil, err
	}
	if token.AccountID != expectedAccountID {
		return nil, fmt.Errorf("account changed; refresh usage before preparing a reset")
	}
	m.resetMu.Lock()
	defer m.resetMu.Unlock()
	records, err := m.resetRecords()
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		r := record.Request
		if r.AccountID != token.AccountID {
			continue
		}
		if r.Credit.ID == creditID && (r.Status == "reset" || r.Status == "already_redeemed") {
			return nil, fmt.Errorf("selected credit was already redeemed; refresh the credit list")
		}
		if !resetUnresolved(r.Status) {
			continue
		}
		if r.UsageResetBinding == binding && r.Credit.ID == creditID && record.Identity == resetIdentity(token) {
			return &r, nil
		}
		return nil, fmt.Errorf("this account already has an unresolved usage-reset request; resolve it before selecting another credit")
	}
	credit, windows, err := m.checkResetCredit(ctx, token, creditID)
	if err != nil {
		return nil, err
	}
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return nil, err
	}
	idBytes[6] = (idBytes[6] & 0x0f) | 0x40
	idBytes[8] = (idBytes[8] & 0x3f) | 0x80
	id := fmt.Sprintf("%x-%x-%x-%x-%x", idBytes[:4], idBytes[4:6], idBytes[6:8], idBytes[8:10], idBytes[10:])
	request := UsageResetRequest{ID: id, UsageResetBinding: binding, AccountID: token.AccountID, Email: token.Email, Credit: credit, RateLimit: windows, CreatedBy: actor, CreatedAt: m.now(), ExpiresAt: m.now().Add(10 * time.Minute), Status: "pending"}
	if err := m.saveReset(usageResetRecord{Request: request, Identity: resetIdentity(token)}); err != nil {
		return nil, fmt.Errorf("persist usage-reset intent: %w", err)
	}
	return &request, nil
}

// Unknown outcomes cannot be canceled into attempts with new idempotency keys.
func (m *Manager) CancelUsageReset(binding UsageResetBinding, id string) (*UsageResetRequest, error) {
	m.resetMu.Lock()
	defer m.resetMu.Unlock()
	records, err := m.resetRecords()
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		r := &record.Request
		if r.ID != id {
			continue
		}
		if r.UsageResetBinding != binding {
			return nil, fmt.Errorf("usage-reset target mismatch")
		}
		if r.Status != "pending" && r.Status != "canceled" && r.Status != "expired" {
			return nil, fmt.Errorf("a submitted usage reset cannot be canceled")
		}
		r.Status = "canceled"
		if err := m.saveReset(record); err != nil {
			return nil, err
		}
		return r, nil
	}
	return nil, fmt.Errorf("usage-reset request not found")
}

// ConfirmUsageReset is the explicit administrator-session path.
// The scheduled path shares dispatch and journal protections below.
func (m *Manager) ConfirmUsageReset(ctx context.Context, binding UsageResetBinding, id, accountID, creditID string, guard func() (func(), error)) (*UsageResetRequest, error) {
	return m.confirmUsageReset(ctx, binding, id, accountID, creditID, guard, 0)
}

func (m *Manager) confirmUsageReset(ctx context.Context, binding UsageResetBinding, id, accountID, creditID string, guard func() (func(), error), autoThreshold time.Duration) (*UsageResetRequest, error) {
	if m.standby {
		return nil, ErrStandby
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	token, err := m.resetToken(ctx, binding.Account)
	if err != nil {
		return nil, err
	}
	m.resetMu.Lock()
	defer m.resetMu.Unlock()
	records, err := m.resetRecords()
	if err != nil {
		return nil, err
	}
	var record *usageResetRecord
	for i := range records {
		if records[i].Request.ID == id {
			record = &records[i]
			break
		}
	}
	if record == nil {
		return nil, fmt.Errorf("usage-reset request not found")
	}
	r := &record.Request
	if r.UsageResetBinding != binding || r.AccountID != accountID || r.Credit.ID != creditID || token.AccountID != r.AccountID || resetIdentity(token) != record.Identity {
		return nil, fmt.Errorf("usage-reset target or OAuth identity changed")
	}
	if autoThreshold > 0 && (r.CreatedBy != usageResetAutoActor || r.Status != "pending") {
		return nil, fmt.Errorf("automatic usage reset requires its own unsubmitted intent")
	}
	if !resetUnresolved(r.Status) {
		if r.Status == "reset" || r.Status == "already_redeemed" || r.Status == "nothing_to_reset" || r.Status == "no_credit" {
			return r, nil
		}
		return nil, fmt.Errorf("usage-reset confirmation expired or canceled")
	}
	if r.Status == "pending" {
		credit, _, err := m.checkResetCredit(ctx, token, r.Credit.ID)
		if err != nil {
			return nil, err
		}
		if autoThreshold > 0 && !resetCreditDue(credit, m.now(), autoThreshold) {
			return nil, fmt.Errorf("credit is no longer within the automatic reset threshold")
		}
		r.Credit = credit
		if !m.now().Before(r.ExpiresAt) {
			return nil, fmt.Errorf("usage-reset confirmation expired")
		}
	}
	// Pin the exact credential through dispatch, including concurrent re-login.
	if guard == nil {
		return nil, fmt.Errorf("usage-reset target guard required")
	}
	release, err := guard()
	if err != nil {
		return nil, err
	}
	defer release()
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.tokens[binding.Account]
	if current == nil || current.AccessToken != token.AccessToken || current.AccountID != r.AccountID {
		return nil, fmt.Errorf("OAuth credential changed; review the request again")
	}
	if autoThreshold > 0 && !resetCreditDue(r.Credit, m.now(), autoThreshold) {
		return nil, fmt.Errorf("credit expired before automatic dispatch")
	}
	r.Status = "in_flight"
	r.Message = ""
	if err := m.saveReset(*record); err != nil {
		return nil, fmt.Errorf("persist attempt before dispatch: %w", err)
	}
	var result struct {
		Code         string `json:"code"`
		WindowsReset int64  `json:"windows_reset"`
	}
	err = m.resetHTTP(ctx, http.MethodPost, "/consume", token.AccessToken, r.AccountID, map[string]string{"redeem_request_id": r.ID, "credit_id": r.Credit.ID}, &result)
	switch result.Code {
	case "reset", "already_redeemed", "nothing_to_reset", "no_credit":
	default:
		if err == nil {
			err = fmt.Errorf("unknown upstream outcome")
		}
	}
	if err != nil {
		r.Status = "uncertain"
		r.Message = "Outcome unknown. Review and retry this same request only; no other credit can be selected."
	} else {
		r.Status = result.Code
		r.WindowsReset = result.WindowsReset
	}
	// Invalidate this account's display cache; never clear local cooldowns.
	delete(m.usageCache, binding.Account)
	_ = m.store.Delete(usageStoreKey(binding.Account))
	if err := m.saveReset(*record); err != nil {
		r.Status = "uncertain"
		r.Message = "Outcome could not be persisted. The original request ID remains reserved; review it before retrying."
	}
	return r, nil
}

const usageResetAutoActor = "system:expiring-codex-credit"

func resetCreditDue(credit UsageResetCredit, now time.Time, threshold time.Duration) bool {
	if threshold <= 0 || credit.ID == "" || credit.Status != "available" || credit.ResetType != "codex_rate_limits" || credit.ExpiresAt == nil {
		return false
	}
	expires, err := time.Parse(time.RFC3339Nano, *credit.ExpiresAt)
	return err == nil && expires.After(now) && expires.Sub(now) <= threshold
}

// AutoUsageReset consumes at most one expiring credit per scan. Unknown outcomes
// and manual intents require review; the scheduler never retries a submitted POST.
func (m *Manager) AutoUsageReset(ctx context.Context, binding UsageResetBinding, threshold time.Duration, guard func() (func(), error)) (*UsageResetRequest, error) {
	if m.standby {
		return nil, ErrStandby
	}
	if threshold <= 0 {
		return nil, nil
	}
	m.autoResetMu.Lock()
	defer m.autoResetMu.Unlock()
	token, err := m.resetToken(ctx, binding.Account)
	if err != nil {
		return nil, err
	}
	requests, err := m.UsageResetRequests()
	if err != nil {
		return nil, err
	}
	attempted := map[string]bool{}
	for _, request := range requests {
		if request.AccountID != token.AccountID {
			continue
		}
		if resetUnresolved(request.Status) {
			if request.Status == "pending" && request.CreatedBy == usageResetAutoActor && request.UsageResetBinding == binding {
				return m.confirmUsageReset(ctx, binding, request.ID, request.AccountID, request.Credit.ID, guard, threshold)
			}
			return nil, nil
		}
		if (request.CreatedBy == usageResetAutoActor && request.Status != "expired") || request.Status == "reset" || request.Status == "already_redeemed" {
			attempted[request.Credit.ID] = true
		}
	}
	credits, err := m.readResetCredits(ctx, token.AccessToken, token.AccountID)
	if err != nil {
		return nil, err
	}
	if credits.AvailableCount < 1 {
		return nil, nil
	}
	var selected *UsageResetCredit
	for i := range credits.Credits {
		credit := &credits.Credits[i]
		if attempted[credit.ID] || !resetCreditDue(*credit, m.now(), threshold) {
			continue
		}
		if selected == nil {
			selected = credit
			continue
		}
		a, _ := time.Parse(time.RFC3339Nano, *credit.ExpiresAt)
		b, _ := time.Parse(time.RFC3339Nano, *selected.ExpiresAt)
		if a.Before(b) {
			selected = credit
		}
	}
	if selected == nil {
		return nil, nil
	}
	request, err := m.PrepareUsageReset(ctx, binding, selected.ID, token.AccountID, usageResetAutoActor)
	if err != nil {
		return nil, err
	}
	return m.confirmUsageReset(ctx, binding, request.ID, request.AccountID, request.Credit.ID, guard, threshold)
}
