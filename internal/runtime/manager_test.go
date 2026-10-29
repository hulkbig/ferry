package runtime

import (
	"context"
	"testing"
	"time"

	"ferry/internal/domain"
)

var idA = domain.ResourceTargetID("a")

func cfg() HealthConfig {
	return DefaultHealthConfig()
}

func TestRecordFailureOpensBreakerAfterThreshold(t *testing.T) {
	m := NewManager(cfg())
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	f := &domain.Failure{Type: domain.FailConnect, Message: "refused"}

	for i := 1; i <= 2; i++ {
		m.RecordFailure(idA, f, now)
	}
	st := m.States(now)[idA]
	if st.Breaker != domain.BreakerClosed || st.ConsecFails != 2 {
		t.Fatalf("before threshold: breaker=%s consec=%d", st.Breaker, st.ConsecFails)
	}

	m.RecordFailure(idA, f, now) // 3rd consecutive -> open
	st = m.States(now)[idA]
	if st.Breaker != domain.BreakerOpen {
		t.Fatalf("3 failures must open the breaker, got %s", st.Breaker)
	}
	if !st.OpenUntil.Equal(now.Add(30 * time.Second)) {
		t.Errorf("OpenUntil=%v want %v", st.OpenUntil, now.Add(30*time.Second))
	}
	if st.LastFailure != f {
		t.Error("LastFailure must reference the failure")
	}
	if st.ErrorRate1m == 0 {
		t.Error("error rate should have decayed upward from 0")
	}
	wantRate := 0.2*1 + (1-0.2)*(0.2*1+(1-0.2)*(0.2*1))
	if abs(st.ErrorRate1m-wantRate) > 1e-9 {
		t.Errorf("ErrorRate1m=%v want %v", st.ErrorRate1m, wantRate)
	}
}

func TestSnapshotLazilyFlipsOpenToHalfOpen(t *testing.T) {
	m := NewManager(cfg())
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	f := &domain.Failure{Type: domain.FailConnect}
	for i := 0; i < 3; i++ {
		m.RecordFailure(idA, f, now)
	}

	before := m.Snapshot(now.Add(time.Second)).Get(idA)
	if before.Breaker != domain.BreakerOpen {
		t.Fatalf("still inside open window: %s", before.Breaker)
	}

	after := m.Snapshot(now.Add(31 * time.Second)).Get(idA)
	if after.Breaker != domain.BreakerHalfOpen {
		t.Fatalf("after OpenUntil expiry must flip to half_open, got %s", after.Breaker)
	}
	// the flip persists in manager state
	if again := m.States(now.Add(32 * time.Second))[idA]; again.Breaker != domain.BreakerHalfOpen {
		t.Errorf("flip not persisted: %s", again.Breaker)
	}
}

func TestHalfOpenProbesCloseBreaker(t *testing.T) {
	m := NewManager(cfg())
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	fail := &domain.Failure{Type: domain.FailConnect}
	for i := 0; i < 3; i++ {
		m.RecordFailure(idA, fail, now)
	}
	probeAt := now.Add(31 * time.Second)
	m.Snapshot(probeAt) // flips to half-open

	// HalfOpenMax=2 successful probes close it.
	ok := domain.Usage{OutputTokens: 1}
	m.RecordSuccess(idA, 100, 50, ok, 0, 0, probeAt)
	if st := m.States(probeAt)[idA]; st.Breaker != domain.BreakerHalfOpen {
		t.Fatalf("one probe success must keep half_open: %s", st.Breaker)
	}
	m.RecordSuccess(idA, 120, 60, ok, 0, 0, probeAt)
	st := m.States(probeAt)[idA]
	if st.Breaker != domain.BreakerClosed {
		t.Fatalf("two probe successes must close: %s", st.Breaker)
	}
	if st.ConsecFails != 0 {
		t.Errorf("consec fails reset on close, got %d", st.ConsecFails)
	}
	if st.LastFailure != nil {
		t.Error("LastFailure cleared on success")
	}
}

func TestFailedProbeReopensWithDoubleDuration(t *testing.T) {
	m := NewManager(cfg())
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	fail := &domain.Failure{Type: domain.FailProvider5xx}
	for i := 0; i < 3; i++ {
		m.RecordFailure(idA, fail, now)
	}
	probeAt := now.Add(31 * time.Second)
	m.Snapshot(probeAt)

	m.RecordFailure(idA, fail, probeAt)
	st := m.States(probeAt)[idA]
	if st.Breaker != domain.BreakerOpen {
		t.Fatalf("failed probe must reopen: %s", st.Breaker)
	}
	if !st.OpenUntil.Equal(probeAt.Add(60 * time.Second)) {
		t.Errorf("reopen duration doubles: got %v want %v", st.OpenUntil, probeAt.Add(60*time.Second))
	}
}

func TestRateLimitCooldownDefaultAndRetryAfter(t *testing.T) {
	m := NewManager(cfg())
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

	// default cooldown when no Retry-After
	m.RecordFailure(idA, &domain.Failure{Type: domain.FailRateLimit}, now)
	if got := m.States(now)[idA].CooldownUntil; !got.Equal(now.Add(20 * time.Second)) {
		t.Errorf("default 429 cooldown: got %v want +20s", got)
	}

	// Retry-After overrides when positive
	m.ClearCooldown(idA)
	m.RecordFailure(idA, &domain.Failure{Type: domain.FailRateLimit, RetryAfter: 90 * time.Second}, now)
	if got := m.States(now)[idA].CooldownUntil; !got.Equal(now.Add(90 * time.Second)) {
		t.Errorf("RetryAfter cooldown: got %v want +90s", got)
	}

	// non-rate-limit failures set no cooldown
	m.ClearCooldown(idA)
	m.RecordFailure(idA, &domain.Failure{Type: domain.FailConnect}, now)
	if got := m.States(now)[idA].CooldownUntil; !got.IsZero() {
		t.Errorf("connect failure must not cool down: got %v", got)
	}
}

func TestQuotaExhaustedFailureParksUntilReset(t *testing.T) {
	m := NewManager(cfg())
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	reset := now.Add(3 * time.Hour)

	m.SetQuota(idA, 100, 100, now, "local_ledger", domain.ConfHigh)
	rt := m.targets[idA]
	rt.st.QuotaResetAt = reset

	// quota-exhausted is circuit-neutral: no ConsecFails growth
	m.RecordFailure(idA, &domain.Failure{Type: domain.FailQuotaExhausted}, now)
	st := m.States(now)[idA]
	if st.ConsecFails != 0 {
		t.Errorf("quota exhaustion is circuit-neutral: consec=%d", st.ConsecFails)
	}
	if !st.CooldownUntil.Equal(reset) {
		t.Errorf("cooldown parked until quota reset: got %v want %v", st.CooldownUntil, reset)
	}
}

func TestLazyQuotaRefreshOnSnapshot(t *testing.T) {
	bucket := &domain.QuotaBucket{ID: "q", Unit: domain.QuotaUnitPrompts, LimitPerCycle: 10,
		Refresh: domain.RefreshSpec{Period: "daily", At: "04:00", Timezone: "UTC"}}
	m := NewManager(cfg())
	m.RegisterTarget(idA, bucket, nil)

	first := time.Date(2026, 8, 21, 5, 0, 0, 0, time.UTC) // past today's 04:00 reset
	snap := m.Snapshot(first)
	st := snap.Get(idA)
	if st.QuotaResetAt.IsZero() {
		t.Fatal("first observation seeds QuotaResetAt from bucket config")
	}
	if !st.QuotaResetAt.Equal(time.Date(2026, 8, 22, 4, 0, 0, 0, time.UTC)) {
		t.Errorf("next daily reset = %v", st.QuotaResetAt)
	}

	// accumulate usage, then cross the reset boundary
	m.RecordSuccess(idA, 10, 5, domain.Usage{}, 0, 7, first)
	if got := m.States(first)[idA].QuotaUsed; got != 7 {
		t.Fatalf("usage recorded, got %v", got)
	}
	after := time.Date(2026, 8, 22, 4, 30, 0, 0, time.UTC)
	snap2 := m.Snapshot(after)
	st2 := snap2.Get(idA)
	if st2.QuotaUsed != 0 {
		t.Errorf("quota resets lazily on snapshot crossing ResetAt: used=%v", st2.QuotaUsed)
	}
	if st2.QuotaConf != domain.ConfHigh {
		t.Errorf("confidence restored to high, got %s", st2.QuotaConf)
	}
	if st2.QuotaResetAt.IsZero() || !st2.QuotaResetAt.After(after) {
		t.Errorf("next reset seeded forward: %v", st2.QuotaResetAt)
	}
}

func TestRecordSuccessUpdatesEWMA(t *testing.T) {
	m := NewManager(cfg())
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

	m.RecordSuccess(idA, 100, 40, domain.Usage{}, 0, 0, now)
	if got := m.States(now)[idA].LatencyEWMAms; got != 100 {
		t.Errorf("first sample seeds EWMA directly: %v", got)
	}
	m.RecordSuccess(idA, 200, 80, domain.Usage{}, 0, 0, now)
	// alpha=0.3: 0.3*200 + 0.7*100 = 130
	if got := m.States(now)[idA].LatencyEWMAms; abs(got-130) > 1e-9 {
		t.Errorf("EWMA=%v want 130", got)
	}
	if got := m.States(now)[idA].TTFT_EWMAms; abs(got-52) > 1e-9 { // .3*80+.7*40
		t.Errorf("TTFT EWMA=%v want 52", got)
	}

	// error rate decays toward zero on success: prev=0.2*1 -> 0.2*0+0.8*prev
	m.RecordFailure(idA, &domain.Failure{Type: domain.FailConnect}, now)
	rateAfterFail := m.States(now)[idA].ErrorRate1m
	m.RecordSuccess(idA, 150, 0, domain.Usage{}, 0, 0, now) // ttft=0 ignored
	rate := m.States(now)[idA].ErrorRate1m
	if abs(rate-(0.8*rateAfterFail)) > 1e-9 {
		t.Errorf("success decays error rate: got %v want %v", rate, 0.8*rateAfterFail)
	}
	if ttft := m.States(now)[idA].TTFT_EWMAms; abs(ttft-52) > 1e-9 {
		t.Errorf("ttft=0 sample must be ignored, got %v", ttft)
	}
}

func TestEnterExitInFlight(t *testing.T) {
	m := NewManager(cfg())
	now := time.Now()

	m.Enter(idA)
	m.Enter(idA)
	if got := m.States(now)[idA].InFlight; got != 2 {
		t.Fatalf("in-flight=%d want 2", got)
	}
	m.Exit(idA)
	m.Exit(idA)
	m.Exit(idA) // over-release guarded
	if got := m.States(now)[idA].InFlight; got != 0 {
		t.Errorf("in-flight floor at 0, got %d", got)
	}
}

func TestSetQuota(t *testing.T) {
	m := NewManager(cfg())
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	m.SetQuota(idA, 42, 100, now, "manual", domain.ConfMedium)
	st := m.States(now)[idA]
	if st.QuotaUsed != 42 || st.QuotaLimit != 100 || st.QuotaSource != "manual" || st.QuotaConf != domain.ConfMedium {
		t.Errorf("SetQuota fields wrong: %+v", st)
	}
	if abs(st.QuotaRemainingFrac()-0.58) > 1e-9 {
		t.Errorf("frac=%v want 0.58", st.QuotaRemainingFrac())
	}
}

func TestClearCooldownResetsEverything(t *testing.T) {
	m := NewManager(cfg())
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	fail := &domain.Failure{Type: domain.FailConnect}
	for i := 0; i < 3; i++ {
		m.RecordFailure(idA, fail, now)
	}
	m.ClearCooldown(idA)
	st := m.States(now)[idA]
	if st.Breaker != domain.BreakerClosed || st.ConsecFails != 0 || !st.CooldownUntil.IsZero() {
		t.Errorf("clear left residue: %+v", st)
	}
}

func TestStatesReturnsCopies(t *testing.T) {
	m := NewManager(cfg())
	now := time.Now()
	states := m.States(now)
	states[idA] = domain.TargetState{ConsecFails: 99} // mutate returned copy
	if got := m.States(now)[idA].ConsecFails; got != 0 {
		t.Error("States must return copies, not live state")
	}
}

func TestNilFailureIgnored(t *testing.T) {
	m := NewManager(cfg())
	now := time.Now()
	m.RecordFailure(idA, nil, now) // must not panic or count
	if st := m.States(now)[idA]; st.ConsecFails != 0 {
		t.Errorf("nil failure recorded: %+v", st)
	}
}

func TestConnectionStateIsSharedWhileTargetTelemetryStaysPerModel(t *testing.T) {
	m := NewManager(cfg())
	now := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
	modelA := domain.ResourceTargetID("account.model-a")
	modelB := domain.ResourceTargetID("account.model-b")
	m.RegisterExecutionTarget(modelA, "account", true, true, nil, nil)
	m.RegisterExecutionTarget(modelB, "account", true, true, nil, nil)

	authFailure := &domain.Failure{Type: domain.FailAuth, Scope: domain.FailureScopeConnection}
	m.RecordFailure(modelA, authFailure, now)
	connection := m.ConnectionStates(now)["account"]
	if connection.Auth != domain.ConnectionAuthError || connection.LastFailure != authFailure {
		t.Fatalf("connection auth state = %+v", connection)
	}
	if got := m.States(now)[modelB]; got.ConsecFails != 0 || got.LastFailure != nil {
		t.Fatalf("other model telemetry was polluted: %+v", got)
	}

	m.RecordFailure(modelB, &domain.Failure{
		Type: domain.FailRateLimit, Scope: domain.FailureScopeConnection, RetryAfter: time.Minute,
	}, now)
	connection = m.ConnectionStates(now)["account"]
	if !connection.CoolingUntil.Equal(now.Add(time.Minute)) || connection.RecentThrottleRate == 0 {
		t.Fatalf("connection throttle state = %+v", connection)
	}

	m.SetQuota(modelA, 100, 100, now, "provider", domain.ConfHigh)
	connection = m.ConnectionStates(now)["account"]
	if len(connection.QuotaWindows) != 1 || connection.QuotaWindows[0].Availability != domain.QuotaExhausted {
		t.Fatalf("connection quota state = %+v", connection.QuotaWindows)
	}
}

func TestConnectionReachabilityRechecksAndRecoversAfterConnectFailure(t *testing.T) {
	m := NewManager(cfg())
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	m.RegisterExecutionTarget(idA, "account", true, true, nil, nil)
	m.RecordFailure(idA, &domain.Failure{Type: domain.FailConnect, Scope: domain.FailureScopeConnection}, now)

	if state := m.ConnectionStates(now)["account"]; state.Reachability != domain.ConnectionOffline {
		t.Fatalf("connect failure state = %+v", state)
	}
	recheckAt := now.Add(cfg().OpenDuration)
	if state := m.ConnectionStates(recheckAt)["account"]; state.Reachability != domain.ConnectionReachabilityUnknown {
		t.Fatalf("expired connect failure did not enter recheck state: %+v", state)
	}
	m.RecordSuccess(idA, 1, 0, domain.Usage{}, 0, 0, recheckAt)
	if state := m.ConnectionStates(recheckAt)["account"]; state.Reachability != domain.ConnectionReachable {
		t.Fatalf("successful probe did not restore reachability: %+v", state)
	}
}

func TestConnectionConnectFailureUsesBoundedBackoffAndSerializedProbe(t *testing.T) {
	m := NewManager(cfg())
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	m.RegisterExecutionTarget(idA, "account", true, true, nil, nil)

	wants := []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second}
	for i, want := range wants {
		m.RecordFailure(idA, &domain.Failure{Type: domain.FailConnect, Scope: domain.FailureScopeConnection, Message: "EOF"}, now)
		state := m.ConnectionStates(now)["account"]
		if state.RecoveryState != "backing_off" || state.RecoveryReason != "EOF" {
			t.Fatalf("failure %d recovery state = %+v", i+1, state)
		}
		if got := state.RetryAt.Sub(now); got != want {
			t.Fatalf("failure %d retry delay = %s, want %s", i+1, got, want)
		}
		now = state.RetryAt
		if state = m.ConnectionStates(now)["account"]; state.RecoveryState != "ready_to_probe" {
			t.Fatalf("failure %d did not enter probe-ready state: %+v", i+1, state)
		}
	}

	release, failure := m.Admit(context.Background(), idA)
	if failure != nil {
		t.Fatalf("probe admission failed: %v", failure)
	}
	if state := m.ConnectionStates(now)["account"]; state.RecoveryState != "probing" {
		t.Fatalf("probe state = %+v", state)
	}
	probeCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, failure = m.Admit(probeCtx, idA); failure == nil {
		t.Fatal("a second recovery probe was admitted concurrently")
	}
	release()
	m.RecordSuccess(idA, 1, 0, domain.Usage{}, 0, 0, now)
	state := m.ConnectionStates(now)["account"]
	if state.RecoveryState != "" || state.Reachability != domain.ConnectionReachable {
		t.Fatalf("successful probe did not restore connection: %+v", state)
	}
}

func TestConnectionProbeSuccessPreservesCapacityCooldown(t *testing.T) {
	m := NewManager(cfg())
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	m.RegisterExecutionTarget(idA, "account", true, true, nil, nil)
	m.RecordFailure(idA, &domain.Failure{
		Type: domain.FailRateLimit, Scope: domain.FailureScopeConnection,
		Message: "slow down", RetryAfter: time.Minute,
	}, now)

	m.RecordConnectionProbeSuccess(idA, 12, now.Add(time.Second))
	state := m.ConnectionStates(now.Add(time.Second))["account"]
	if state.RecoveryState != "cooling" || !state.CoolingUntil.Equal(now.Add(time.Minute)) {
		t.Fatalf("probe success cleared capacity evidence: %+v", state)
	}
	if state.Auth != domain.ConnectionAuthUsable || state.Reachability != domain.ConnectionReachable {
		t.Fatalf("probe success did not update credential and transport evidence: %+v", state)
	}
}

func TestConnectionKnownQuotaResetAutomaticallyRecovers(t *testing.T) {
	m := NewManager(cfg())
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(5 * time.Minute)
	m.RegisterExecutionTarget(idA, "account", true, true, nil, nil)
	m.SetQuota(idA, 100, 100, now, "provider", domain.ConfHigh)
	m.targets[idA].st.QuotaResetAt = resetAt
	m.RecordFailure(idA, &domain.Failure{Type: domain.FailQuotaExhausted, Scope: domain.FailureScopeConnection}, now)

	before := m.ConnectionStates(resetAt.Add(-time.Nanosecond))["account"]
	if len(before.QuotaWindows) != 1 || before.QuotaWindows[0].Availability != domain.QuotaExhausted {
		t.Fatalf("known quota recovered before reset: %+v", before.QuotaWindows)
	}
	after := m.ConnectionStates(resetAt)["account"]
	if len(after.QuotaWindows) != 1 || after.QuotaWindows[0].Availability != domain.QuotaAvailable {
		t.Fatalf("known quota did not recover at reset: %+v", after.QuotaWindows)
	}
}

func TestConnectionUnknownQuotaResetAgesToBoundedRecheck(t *testing.T) {
	m := NewManager(cfg())
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	m.RegisterExecutionTarget(idA, "account", true, true, nil, nil)
	m.RecordFailure(idA, &domain.Failure{Type: domain.FailQuotaExhausted, Scope: domain.FailureScopeConnection}, now)

	before := m.ConnectionStates(now.Add(cfg().OpenDuration - time.Nanosecond))["account"]
	if len(before.QuotaWindows) != 1 || before.QuotaWindows[0].Availability != domain.QuotaExhausted {
		t.Fatalf("unknown quota recovered before recheck bound: %+v", before.QuotaWindows)
	}
	recheckAt := now.Add(cfg().OpenDuration)
	after := m.ConnectionStates(recheckAt)["account"]
	if len(after.QuotaWindows) != 1 || after.QuotaWindows[0].Availability != domain.QuotaUnknown {
		t.Fatalf("unknown quota remained permanently exhausted: %+v", after.QuotaWindows)
	}
	if state := m.States(recheckAt)[idA]; state.CooldownUntil.After(recheckAt) {
		t.Fatalf("target remained parked beyond bounded quota recheck: %+v", state)
	}
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
