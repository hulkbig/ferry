package runtime

import (
	"testing"
	"time"

	"ferry/internal/domain"
)

func TestStackedBucketsRollingRefreshAndExhaustion(t *testing.T) {
	m := NewManager(DefaultHealthConfig())
	fiveHour := &domain.QuotaBucket{
		ID: "claude-5h", Unit: domain.QuotaUnitPrompts, LimitPerCycle: 10,
		Refresh: domain.RefreshSpec{Period: "5h", Rolling: true},
	}
	weekly := &domain.QuotaBucket{
		ID: "claude-weekly", Unit: domain.QuotaUnitPrompts, LimitPerCycle: 100,
		Refresh: domain.RefreshSpec{Period: "weekly"},
	}
	m.RegisterExecutionTargetBuckets("t1", "c1", true, true,
		[]*domain.QuotaBucket{fiveHour, weekly}, nil)

	now := time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC)
	// 10 prompts on the 5h window: exhausted even though weekly has room.
	m.RecordSuccessMulti("t1", 10, 5, domain.Usage{}, 0, []domain.QuotaUsage{
		{BucketID: "claude-5h", Units: 10},
		{BucketID: "claude-weekly", Units: 10},
	}, now)

	st := m.Snapshot(now).Get("t1")
	if !st.QuotaExhausted() {
		t.Fatalf("5h window should be exhausted, used=%v limit=%v", st.QuotaUsed, st.QuotaLimit)
	}
	if st.QuotaLimit != 10 {
		t.Fatalf("primary mirrors most restrictive bucket, limit=%v", st.QuotaLimit)
	}
	if !st.CooldownUntil.After(now) {
		t.Fatal("exhaustion must park the cooldown until the window resets")
	}

	// After the rolling window passes, usage resets and the weekly remainder
	// becomes the governing constraint.
	after := now.Add(5*time.Hour + time.Minute)
	st2 := m.Snapshot(after).Get("t1")
	if st2.QuotaExhausted() {
		t.Fatalf("5h window must have refreshed, used=%v", st2.QuotaUsed)
	}
	if st2.QuotaLimit != 100 {
		t.Fatalf("weekly bucket should govern after 5h refresh, limit=%v", st2.QuotaLimit)
	}
}

func TestConnectionWindowsPerBucket(t *testing.T) {
	m := NewManager(DefaultHealthConfig())
	b1 := &domain.QuotaBucket{ID: "b1", Unit: domain.QuotaUnitTokens, LimitPerCycle: 100,
		Refresh: domain.RefreshSpec{Period: "daily"}}
	b2 := &domain.QuotaBucket{ID: "b2", Unit: domain.QuotaUnitTokens, LimitPerCycle: 50,
		Refresh: domain.RefreshSpec{Period: "daily"}}
	m.RegisterExecutionTargetBuckets("t1", "c1", true, true, []*domain.QuotaBucket{b1, b2}, nil)
	now := time.Now()
	m.RecordSuccessMulti("t1", 5, 2, domain.Usage{InputTokens: 10, OutputTokens: 5}, 0,
		[]domain.QuotaUsage{{BucketID: "b1", Units: 15}, {BucketID: "b2", Units: 15}}, now)

	states := m.ConnectionStates(now)
	c := states["c1"]
	if c == nil {
		t.Fatal("connection state missing")
	}
	if len(c.QuotaWindows) != 2 {
		t.Fatalf("want one window per bucket, got %+v", c.QuotaWindows)
	}
	byType := map[string]domain.QuotaWindowState{}
	for _, w := range c.QuotaWindows {
		byType[w.Type] = w
	}
	if w := byType["b1"]; w.RemainingFraction == nil || *w.RemainingFraction != 0.85 {
		t.Fatalf("b1 window = %+v, want 0.85 remaining", w)
	}
	if w := byType["b2"]; w.RemainingFraction == nil || *w.RemainingFraction != 0.7 {
		t.Fatalf("b2 window = %+v, want 0.7 remaining", w)
	}
}

func TestApplyProviderQuotaMirrorsWorstWindow(t *testing.T) {
	m := NewManager(DefaultHealthConfig())
	m.RegisterExecutionTargetBuckets("t1", "c1", true, true, nil, nil)
	now := time.Now()
	primary := 0.05
	secondary := 0.6
	m.ApplyProviderQuota("c1", []domain.QuotaWindowState{
		{Type: "primary", Availability: domain.QuotaAvailable, RemainingFraction: &primary,
			ResetAt: now.Add(time.Hour), ObservedAt: now, Confidence: domain.ConfMedium},
		{Type: "secondary", Availability: domain.QuotaAvailable, RemainingFraction: &secondary,
			ResetAt: now.Add(48 * time.Hour), ObservedAt: now, Confidence: domain.ConfMedium},
	}, now)

	st := m.Snapshot(now).Get("t1")
	if st.QuotaSource != "provider_api" {
		t.Fatalf("quota source = %q, want provider_api", st.QuotaSource)
	}
	if got := st.QuotaRemainingFrac(); got > 0.06 || got < 0.04 {
		t.Fatalf("remaining = %v, want ~0.05 (worst window governs)", got)
	}

	// An exhausted provider window parks the cooldown until its reset.
	zero := 0.0
	m.ApplyProviderQuota("c1", []domain.QuotaWindowState{
		{Type: "primary", Availability: domain.QuotaExhausted, RemainingFraction: &zero,
			ResetAt: now.Add(2 * time.Hour), ObservedAt: now, Confidence: domain.ConfMedium},
	}, now)
	st2 := m.Snapshot(now.Add(time.Second)).Get("t1")
	if !st2.CooldownUntil.After(now) {
		t.Fatal("exhausted provider window must park cooldown")
	}
}
