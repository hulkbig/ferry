package runtime

import (
	"ferry/internal/domain"
	"testing"
	"time"
)

func TestProviderProtectionSurvivesInFlightSuccessAndIndependentResets(t *testing.T) {
	m := NewManager(DefaultHealthConfig())
	m.RegisterExecutionTargetBuckets("t1", "c1", true, true, nil, nil)
	m.RegisterExecutionTargetBuckets("t2", "c1", true, true, nil, nil)
	now := time.Now()
	a, b := 0.02, 0.01
	windows := []domain.QuotaWindowState{{Type: "primary", Availability: domain.QuotaExhausted, RemainingFraction: &a, ResetAt: now.Add(time.Hour), ObservedAt: now}, {Type: "secondary", Availability: domain.QuotaExhausted, RemainingFraction: &b, ResetAt: now.Add(2 * time.Hour), ObservedAt: now}}
	m.ApplyProviderQuota("c1", windows, now)
	m.RecordSuccessMulti("t2", 1, 1, domain.Usage{}, 0, nil, now.Add(time.Second))
	m.RecordFailure("t1", &domain.Failure{Type: domain.FailQuotaExhausted, Scope: domain.FailureScopeConnection, LocalQuotaProtection: true}, now.Add(2*time.Second))
	got := m.ConnectionStates(now.Add(time.Minute))["c1"].QuotaWindows
	if len(got) != 2 || got[0].Availability != domain.QuotaExhausted || got[1].Availability != domain.QuotaExhausted {
		t.Fatalf("lost provider protection: %+v", got)
	}
	got = m.ConnectionStates(now.Add(time.Hour + time.Second))["c1"].QuotaWindows
	if got[0].Availability == domain.QuotaExhausted || got[1].Availability != domain.QuotaExhausted {
		t.Fatalf("wrong independent reset: %+v", got)
	}
	got = m.ConnectionStates(now.Add(2*time.Hour + time.Second))["c1"].QuotaWindows
	if got[1].Availability == domain.QuotaExhausted {
		t.Fatal("weekly window did not reset")
	}
	// Paid overflow re-evaluation clears only quota-derived target parking.
	for i := range windows {
		windows[i].Availability = domain.QuotaAvailable
	}
	m.ApplyProviderQuota("c1", windows, now)
	if m.Snapshot(now).Get("t1").CooldownUntil.After(now) {
		t.Fatal("opt-in left quota cooldown behind")
	}
}
