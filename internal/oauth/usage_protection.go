package oauth

import (
	"context"
	"fmt"
	"math"
	"time"

	"ferry/internal/domain"
)

// UsageProtection is independent of reset coupons. Paid overflow is opt-in.
type UsageProtection struct {
	MaxUsedPercent float64 `json:"max_used_percent"`
	AllowCredits   bool    `json:"allow_credits"`
}

func DefaultUsageProtection() UsageProtection { return UsageProtection{MaxUsedPercent: 98} }

func (p UsageProtection) Validate() error {
	if math.IsNaN(p.MaxUsedPercent) || math.IsInf(p.MaxUsedPercent, 0) || p.MaxUsedPercent <= 0 || p.MaxUsedPercent > 100 {
		return fmt.Errorf("codex_usage_protection.max_used_percent must be greater than 0 and at most 100")
	}
	return nil
}

// ProtectionWindows keeps real remaining fractions for display. Availability
// expresses the routing policy, including the explicit paid-overflow opt-in.
func ProtectionWindows(usage *UsageSnapshot, policy UsageProtection, now time.Time) []domain.QuotaWindowState {
	unknown := func() []domain.QuotaWindowState {
		return []domain.QuotaWindowState{{Type: "codex_usage_unknown", Availability: domain.QuotaExhausted, ObservedAt: now}}
	}
	if usage == nil || policy.Validate() != nil || usage.RateLimit == nil ||
		(usage.RateLimit.PrimaryWindow == nil && usage.RateLimit.SecondaryWindow == nil) {
		return unknown()
	}
	credits := policy.AllowCredits && usage.Credits != nil && (usage.Credits.HasCredits || usage.Credits.Unlimited)
	var windows []domain.QuotaWindowState
	add := func(name string, w *UsageWindow, subscription bool) bool {
		if w == nil {
			return false
		}
		if w.missingUsedPercent || math.IsNaN(w.UsedPercent) || math.IsInf(w.UsedPercent, 0) || w.UsedPercent < 0 {
			windows = append(windows, unknown()...)
			return true
		}
		remaining := math.Max(0, 1-w.UsedPercent/100)
		reset := time.Time{}
		if w.ResetAt > 0 {
			reset = time.Unix(w.ResetAt, 0)
		} else if w.ResetAfterSeconds > 0 {
			base := usage.ObservedAt
			if base.IsZero() {
				base = now
			}
			reset = base.Add(time.Duration(w.ResetAfterSeconds) * time.Second)
		}
		blocked := w.UsedPercent >= 100
		if subscription {
			if policy.AllowCredits {
				blocked = blocked && !credits
			} else {
				blocked = w.UsedPercent >= policy.MaxUsedPercent
			}
		}
		availability := domain.QuotaAvailable
		if blocked {
			availability = domain.QuotaExhausted
		}
		windows = append(windows, domain.QuotaWindowState{Type: name, Availability: availability,
			RemainingFraction: &remaining, ResetAt: reset, ObservedAt: usage.ObservedAt, Confidence: domain.ConfMedium})
		return blocked
	}
	primary := add("primary", usage.RateLimit.PrimaryWindow, true)
	secondary := add("secondary", usage.RateLimit.SecondaryWindow, true)
	// An upstream limit flag without a matching exhausted window still blocks;
	// never assign the short window's reset to an unknown/shared restriction.
	if usage.RateLimit.LimitReached && !credits && !primary && !secondary {
		windows = append(windows, domain.QuotaWindowState{Type: "subscription", Availability: domain.QuotaExhausted, ObservedAt: usage.ObservedAt})
	}
	for _, extra := range usage.AdditionalRateLimits {
		if extra.RateLimit == nil {
			continue
		}
		a := add(extra.LimitName+"/primary", extra.RateLimit.PrimaryWindow, false)
		b := add(extra.LimitName+"/secondary", extra.RateLimit.SecondaryWindow, false)
		if extra.RateLimit.LimitReached && !a && !b {
			windows = append(windows, domain.QuotaWindowState{Type: extra.LimitName, Availability: domain.QuotaExhausted, ObservedAt: usage.ObservedAt})
		}
	}
	if usage.SpendControl != nil && usage.SpendControl.Reached {
		reset := time.Time{}
		if limit := usage.SpendControl.IndividualLimit; limit != nil && limit.ResetAt > 0 {
			reset = time.Unix(limit.ResetAt, 0)
		}
		windows = append(windows, domain.QuotaWindowState{Type: "spend_control", Availability: domain.QuotaExhausted, ResetAt: reset, ObservedAt: usage.ObservedAt})
	}
	return windows
}

// SetUsageProtection connects durable settings and the scheduler. Callbacks are
// copied under the token mutex and executed without holding it.
func (m *Manager) SetUsageProtection(read func() (UsageProtection, error), observe func(string, *UsageSnapshot, []domain.QuotaWindowState)) {
	m.mu.Lock()
	m.usagePolicy, m.usageObserver = read, observe
	m.mu.Unlock()
}

// CheckUsage runs immediately before each inference HTTP send (including
// retries, probes and images). Recent response evidence avoids another usage
// request; startup, missing/expired evidence and periodic reconciliation fetch
// the usage API. Missing/failed evidence fails closed. In-flight work and upstream
// accounting lag can still overshoot: this is not an upstream spending cap.
func (m *Manager) CheckUsage(ctx context.Context, account, expectedID string) *domain.Failure {
	failure := func(message string) *domain.Failure {
		return &domain.Failure{LocalQuotaProtection: true, Type: domain.FailQuotaExhausted, Scope: domain.FailureScopeConnection, HTTPStatus: 429, Message: message}
	}
	m.mu.Lock()
	usage := m.usageCache[account]
	verified := m.usageVerified[account]
	now := m.now()
	fresh := usage != nil && usage.AccountID == expectedID && !verified.IsZero() &&
		!now.Before(usage.ObservedAt) && now.Sub(usage.ObservedAt) < 30*time.Second && now.Sub(verified) < 5*time.Minute

	if fresh && usage.RateLimit != nil {
		for _, window := range []*UsageWindow{usage.RateLimit.PrimaryWindow, usage.RateLimit.SecondaryWindow} {
			if window != nil && window.ResetAt > 0 && window.ResetAt <= now.Unix() {
				fresh = false
			}
		}
	}
	m.mu.Unlock()
	var err error
	if !fresh {
		usage, err = m.Usage(ctx, account)
	}
	if err != nil || usage == nil || usage.AccountID != expectedID {
		return failure("Codex usage protection: current account quota unavailable; inference was not sent")
	}
	m.mu.Lock()
	// A response may have tightened quota while a full usage read or policy
	// lookup was in flight. Prefer the currently published immutable snapshot.
	if latest := m.usageCache[account]; latest != nil && latest.AccountID == expectedID {
		usage = latest
	}
	read, observe := m.usagePolicy, m.usageObserver
	m.mu.Unlock()
	policy := DefaultUsageProtection()
	if read != nil {
		policy, err = read()
	}
	if err != nil || policy.Validate() != nil {
		return failure("Codex usage protection settings unavailable; inference was not sent")
	}
	now = m.now()
	windows := ProtectionWindows(usage, policy, now)
	if observe != nil {
		observe(account, usage, windows)
	}
	var blocked bool
	var reset time.Time
	for _, w := range windows {
		if w.Availability != domain.QuotaExhausted {
			continue
		}
		blocked = true
		if w.ResetAt.After(reset) {
			reset = w.ResetAt
		}
	}
	if !blocked {
		return nil
	}
	f := failure("Codex usage protection: quota threshold or upstream limit reached; waiting for reset")
	if reset.After(now) {
		f.RetryAfter = reset.Sub(now)
	}
	return f
}
