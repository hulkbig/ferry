package oauth

import (
	"math"
	"net/http"
	"strconv"
	"time"
)

// Header windows are optional. Reject incomplete/non-finite fields rather than
// interpreting a missing percentage as zero. Only the account-wide codex bucket
// is merged here; model-specific buckets must not replace subscription quota.
func usageHeaderWindow(headers http.Header, name string) *UsageWindow {
	prefix := "x-codex-" + name + "-"
	used, err := strconv.ParseFloat(headers.Get(prefix+"used-percent"), 64)
	if err != nil || math.IsNaN(used) || math.IsInf(used, 0) || used < 0 {
		return nil
	}
	reset, err := strconv.ParseInt(headers.Get(prefix+"reset-at"), 10, 64)
	if err != nil || reset <= 0 {
		return nil
	}
	minutes, err := strconv.ParseInt(headers.Get(prefix+"window-minutes"), 10, 64)
	if err != nil || minutes <= 0 || minutes > 525600 {
		return nil
	}
	return &UsageWindow{UsedPercent: used, ResetAt: reset, LimitWindowSeconds: minutes * 60}
}

// ObserveUsageHeaders never treats absent fields as recovery. A partial update
// can tighten an existing window but cannot extend the freshness of unrelated
// windows or credit evidence. Complete headers are reconciled against the full
// usage endpoint at least every five minutes (including spend-control state).
func (m *Manager) ObserveUsageHeaders(account, expectedID string, headers http.Header) {
	primary := usageHeaderWindow(headers, "primary")
	secondary := usageHeaderWindow(headers, "secondary")
	if primary == nil && secondary == nil {
		return
	}
	has, hasErr := strconv.ParseBool(headers.Get("x-codex-credits-has-credits"))
	unlimited, unlimitedErr := strconv.ParseBool(headers.Get("x-codex-credits-unlimited"))
	m.mu.Lock()
	current := m.tokens[account]
	old := m.usageCache[account]
	if current == nil || current.AccountID != expectedID || old == nil || old.AccountID != expectedID || old.RateLimit == nil {
		m.mu.Unlock()
		return
	}
	snapshot := *old
	limits := *old.RateLimit
	snapshot.RateLimit = &limits
	limits.PrimaryWindow = conservativeUsageWindow(limits.PrimaryWindow, primary)
	limits.SecondaryWindow = conservativeUsageWindow(limits.SecondaryWindow, secondary)
	if primary != nil && secondary != nil {
		limits.LimitReached = limits.PrimaryWindow.UsedPercent >= 100 || limits.SecondaryWindow.UsedPercent >= 100
	}
	if hasErr == nil && unlimitedErr == nil {
		snapshot.Credits = &UsageCredits{HasCredits: has, Unlimited: unlimited, Balance: headers.Get("x-codex-credits-balance")}
	}
	if primary != nil && secondary != nil && hasErr == nil && unlimitedErr == nil {
		snapshot.ObservedAt = m.now()
	}
	m.usageCache[account] = &snapshot
	read, observe := m.usagePolicy, m.usageObserver
	m.mu.Unlock()
	policy := DefaultUsageProtection()
	if read != nil {
		var err error
		policy, err = read()
		if err != nil {
			return
		}
	}
	if observe != nil {
		observe(account, &snapshot, ProtectionWindows(&snapshot, policy, time.Now()))
	}
}

// Keep the stricter observation when usage and inference responses race.
// Explicit resets invalidate the cache; a new reset epoch starts a new window.
func conservativeUsageWindow(previous, next *UsageWindow) *UsageWindow {
	if next == nil {
		return previous
	}
	if next.missingUsedPercent {
		return next
	}
	if previous != nil && previous.ResetAt > 0 && next.ResetAt > 0 &&
		(previous.ResetAt > next.ResetAt || previous.ResetAt == next.ResetAt && previous.UsedPercent > next.UsedPercent) {
		return previous
	}
	return next
}
