// Package runtime maintains live resource state: passive health, circuit
// breakers, cooldowns, latency EWMA, and local quota accounting
// (F-P0-18, F-P1-02, F-P1-03).
package runtime

import (
	"sync"
	"time"

	"ferry/internal/domain"
)

// Config knobs for health machinery.
type HealthConfig struct {
	FailThreshold int           // consecutive failures to open breaker
	OpenDuration  time.Duration // initial open period
	HalfOpenMax   int           // probe successes required to close
	EWMAAlpha     float64       // latency smoothing factor
	CooldownOn429 time.Duration // default cooldown when no Retry-After
}

func DefaultHealthConfig() HealthConfig {
	return HealthConfig{
		FailThreshold: 3,
		OpenDuration:  30 * time.Second,
		HalfOpenMax:   2,
		EWMAAlpha:     0.3,
		CooldownOn429: 20 * time.Second,
	}
}

// Manager owns all mutable target state. The data plane reads consistent
// snapshots per request; writes come from attempt outcomes and quota events.
type Manager struct {
	mu                sync.Mutex
	cooldownSaveMu    sync.Mutex
	managed           bool
	identities        map[string]string
	cfg               HealthConfig
	targets           map[domain.ResourceTargetID]*targetRuntime
	connections       map[string]*connectionRuntime
	targetConnections map[domain.ResourceTargetID]string
	// meta for quota refresh computation (all stacked buckets; index 0 = primary)
	buckets map[domain.ResourceTargetID][]*domain.QuotaBucket
	plans   map[domain.ResourceTargetID]*domain.Plan
	// cooldown state survives restarts so a parked connection is not
	// immediately retried against an upstream that already refused it.
	cooldownPath  string
	cooldownDirty chan struct{}
}

type targetRuntime struct {
	quotaCooldownUntil time.Time // identifies quota parking independently of quota freshness
	st                 domain.TargetState
	halfOK             int // consecutive half-open probes succeeded
	halfN              int // probes attempted while open
	// quota tracks per-bucket consumption for stacked plans (5h+weekly etc.);
	// the most restrictive bucket is mirrored into st for policy conditions.
	quota map[string]*bucketUse
}

// bucketUse is the locally-accounted consumption of one quota bucket.
type bucketUse struct {
	used    float64
	resetAt time.Time
}

type connectionRuntime struct {
	st                    domain.ConnectionState
	reachabilityRecheckAt time.Time
	connectFailures       int
	adm                   connectionAdmission
}

func connectBackoff(failures int) time.Duration {
	switch failures {
	case 0, 1:
		return 2 * time.Second
	case 2:
		return 5 * time.Second
	case 3:
		return 10 * time.Second
	default:
		return 30 * time.Second
	}
}

func (m *Manager) connectionRecheckInterval() time.Duration {
	if m.cfg.OpenDuration > 0 {
		return m.cfg.OpenDuration
	}
	return time.Second
}

// newTargetRuntime seeds the breaker closed; the zero value "" would never
// satisfy the RecordFailure open-transition check.
func newTargetRuntime() *targetRuntime {
	return &targetRuntime{st: domain.TargetState{Breaker: domain.BreakerClosed}}
}

// NewManager creates the runtime manager.
func NewManager(cfg HealthConfig) *Manager {
	return &Manager{
		cfg:               cfg,
		targets:           map[domain.ResourceTargetID]*targetRuntime{},
		connections:       map[string]*connectionRuntime{},
		targetConnections: map[domain.ResourceTargetID]string{},
		buckets:           map[domain.ResourceTargetID][]*domain.QuotaBucket{},
		plans:             map[domain.ResourceTargetID]*domain.Plan{},
		identities:        map[string]string{},
	}
}

// RegisterTarget initializes tracking for a compiled target.
func (m *Manager) RegisterTarget(id domain.ResourceTargetID, bucket *domain.QuotaBucket, plan *domain.Plan) {
	var buckets []*domain.QuotaBucket
	if bucket != nil {
		buckets = []*domain.QuotaBucket{bucket}
	}
	m.RegisterExecutionTargetBuckets(id, string(id), true, true, buckets, plan)
}

// RegisterExecutionTarget links a Connection×Model runtime leaf to its
// account-wide Connection state (single-bucket compat wrapper).
func (m *Manager) RegisterExecutionTarget(id domain.ResourceTargetID, connectionID string,
	enabled, credentialUsable bool, bucket *domain.QuotaBucket, plan *domain.Plan) {
	var buckets []*domain.QuotaBucket
	if bucket != nil {
		buckets = []*domain.QuotaBucket{bucket}
	}
	m.RegisterExecutionTargetBuckets(id, connectionID, enabled, credentialUsable, buckets, plan)
}

// RegisterExecutionTargetBuckets is the stacked-bucket variant of
// RegisterExecutionTarget for plans carrying multiple quota windows.
func (m *Manager) RegisterExecutionTargetBuckets(id domain.ResourceTargetID, connectionID string,
	enabled, credentialUsable bool, buckets []*domain.QuotaBucket, plan *domain.Plan) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.targets[id]; !ok {
		m.targets[id] = newTargetRuntime()
	}
	m.buckets[id] = buckets
	m.plans[id] = plan
	if connectionID == "" {
		connectionID = string(id)
	}
	m.targetConnections[id] = connectionID
	connection := m.connections[connectionID]
	if connection == nil {
		connection = &connectionRuntime{st: domain.ConnectionState{
			ConnectionID: connectionID, Enabled: enabled,
			Auth:         domain.ConnectionAuthUnknown,
			Reachability: domain.ConnectionReachabilityUnknown,
		}}
		m.connections[connectionID] = connection
	}
	// A connection can own multiple compiled targets, including compatibility
	// projections with different enabled states. Aggregate the registration
	// generation instead of letting map iteration order choose the final state.
	connection.st.Enabled = connection.st.Enabled || enabled
	if credentialUsable {
		connection.st.Auth = domain.ConnectionAuthUsable
	} else if enabled && connection.st.Auth != domain.ConnectionAuthUsable {
		connection.st.Auth = domain.ConnectionAuthError
	}
}

func (m *Manager) get(id domain.ResourceTargetID) *targetRuntime {
	t, ok := m.targets[id]
	if !ok {
		t = newTargetRuntime()
		m.targets[id] = t
	}
	return t
}

// refreshBucketsLocked advances per-bucket windows past their reset time and
// mirrors the most restrictive bucket into TargetState (single-window policy
// surface). Callers hold m.mu.
func (m *Manager) refreshBucketsLocked(id domain.ResourceTargetID, rt *targetRuntime, now time.Time) {
	buckets := m.buckets[id]
	if rt.quota == nil {
		rt.quota = map[string]*bucketUse{}
	}
	for _, b := range buckets {
		bu := rt.quota[b.ID]
		if bu == nil {
			rt.quota[b.ID] = &bucketUse{resetAt: b.NextRefresh(now)}
			continue
		}
		if !bu.resetAt.IsZero() && !bu.resetAt.After(now) {
			bu.used = 0
			bu.resetAt = b.NextRefresh(now)
		}
	}
	if len(buckets) == 0 {
		// legacy single-window path (manual / provider_api quota)
		st := &rt.st
		if st.QuotaResetAt.IsZero() {
			return
		}
		if !st.QuotaResetAt.After(now) {
			st.QuotaUsed = 0
			st.QuotaResetAt = now.Add(time.Hour)
			st.QuotaConf = domain.ConfHigh
		}
		return
	}
	m.mirrorPrimaryLocked(rt, buckets, now)
}

// mirrorPrimaryLocked reflects the most restrictive stacked bucket (lowest
// remaining fraction; exhausted wins) into the single-window TargetState and
// parks the cooldown on the earliest exhausted reset. Callers hold m.mu.
func (m *Manager) mirrorPrimaryLocked(rt *targetRuntime, buckets []*domain.QuotaBucket, now time.Time) {
	st := &rt.st
	st.QuotaLimit = 0
	worstFrac := 1.0
	for _, b := range buckets {
		if b.LimitPerCycle <= 0 {
			continue
		}
		bu := rt.quota[b.ID]
		if bu == nil {
			bu = &bucketUse{}
		}
		frac := 1 - bu.used/b.LimitPerCycle
		if frac < 0 {
			frac = 0
		}
		if frac <= worstFrac || st.QuotaLimit == 0 {
			worstFrac = frac
			st.QuotaUsed = bu.used
			st.QuotaLimit = b.LimitPerCycle
			st.QuotaResetAt = bu.resetAt
		}
		if frac <= 0 && bu.resetAt.After(now) && bu.resetAt.After(st.CooldownUntil) {
			st.CooldownUntil = bu.resetAt
		}
	}
	if st.QuotaLimit > 0 && st.QuotaSource == "" {
		st.QuotaSource = "local_ledger"
		st.QuotaConf = domain.ConfHigh
	}
}

// Snapshot captures an immutable view for one scheduling decision.
func (m *Manager) Snapshot(now time.Time) *domain.Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	states := make(map[domain.ResourceTargetID]*domain.TargetState, len(m.targets))
	for id, rt := range m.targets {
		st := rt.st // copy

		// advance breaker state machine lazily
		if st.Breaker == domain.BreakerOpen && !st.OpenUntil.After(now) {
			st.Breaker = domain.BreakerHalfOpen
			rt.st.Breaker = domain.BreakerHalfOpen
			rt.halfN = 0
			rt.halfOK = 0
		}
		// lazy quota refresh: if a bucket's reset time passed, zero its usage.
		// Rolling windows restart at the observation moment (5h rolling etc.);
		// fixed-cadence buckets recompute their wall-clock reset.
		m.refreshBucketsLocked(id, rt, now)
		st = rt.st
		states[id] = &st
	}
	return domain.NewSnapshot(now, states)
}

// RecordSuccess feeds a successful attempt into health + quota.
// isRequest marks that this attempt consumed a request slot (prompts units).
func (m *Manager) RecordSuccess(id domain.ResourceTargetID, latencyMS, ttftMS int64,
	u domain.Usage, cashCost float64, quotaUnits float64, now time.Time) {
	var quotas []domain.QuotaUsage
	if quotaUnits != 0 {
		if buckets := m.bucketList(id); len(buckets) > 0 {
			quotas = []domain.QuotaUsage{{BucketID: buckets[0].ID, Units: quotaUnits}}
		} else {
			quotas = []domain.QuotaUsage{{BucketID: "legacy", Units: quotaUnits}}
		}
	}
	m.RecordSuccessMulti(id, latencyMS, ttftMS, u, cashCost, quotas, now)
}

// RecordConnectionProbeSuccess records only what a management connection test
// proves: the tested target accepted a request and the shared credential and
// transport are usable. It deliberately leaves quota windows and rate-limit
// cooldowns intact because a single model probe cannot clear account-wide
// capacity evidence.
func (m *Manager) RecordConnectionProbeSuccess(id domain.ResourceTargetID, latencyMS int64, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.managed && m.targets[id] == nil {
		return
	}
	defer m.markCooldownDirtyLocked()

	rt := m.get(id)
	st := &rt.st
	st.ConsecFails = 0
	st.ErrorRate1m = decayRate(st.ErrorRate1m, 0)
	st.LastFailure = nil
	st.LatencyEWMAms = ewma(st.LatencyEWMAms, float64(latencyMS), m.cfg.EWMAAlpha)
	if st.Breaker != domain.BreakerClosed {
		st.Breaker = domain.BreakerClosed
		st.OpenUntil = time.Time{}
		rt.halfOK, rt.halfN = 0, 0
	}

	connection := m.connectionForTarget(id)
	connection.st.Auth = domain.ConnectionAuthUsable
	connection.st.Reachability = domain.ConnectionReachable
	connection.reachabilityRecheckAt = time.Time{}
	connection.connectFailures = 0
	connection.st.LastFailure = nil
	switch connection.st.RecoveryState {
	case "backing_off", "ready_to_probe", "probing", "needs_attention":
		connection.st.RecoveryState = ""
		connection.st.RecoveryReason = ""
		connection.st.RetryAt = time.Time{}
		connection.st.CoolingUntil = time.Time{}
		m.noteThrottleLocked(connection, false, now)
	}
}

// bucketList returns the stacked buckets registered for a target (unlocked read
// is only safe while m.mu is held; this helper acquires it for ad-hoc callers).
func (m *Manager) bucketList(id domain.ResourceTargetID) []*domain.QuotaBucket {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.buckets[id]
}

// RecordSuccessMulti feeds a successful attempt into health + per-bucket quota.
// quotas carries the consumption of each stacked bucket of the target's plan.
func (m *Manager) RecordSuccessMulti(id domain.ResourceTargetID, latencyMS, ttftMS int64,
	u domain.Usage, cashCost float64, quotas []domain.QuotaUsage, now time.Time) {

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.managed && m.targets[id] == nil {
		return // outcome from a removed or rebound account
	}
	defer m.markCooldownDirtyLocked()
	rt := m.get(id)
	st := &rt.st

	switch st.Breaker {
	case domain.BreakerHalfOpen:
		rt.halfOK++
		if rt.halfOK >= m.cfg.HalfOpenMax {
			st.Breaker = domain.BreakerClosed
			st.ConsecFails = 0
			rt.halfOK, rt.halfN = 0, 0
		}
	case domain.BreakerOpen:
		// success during open (shouldn't happen) — treat as probe
		st.Breaker = domain.BreakerHalfOpen
	default:
		st.ConsecFails = 0
	}
	st.LastFailure = nil
	st.ErrorRate1m = decayRate(st.ErrorRate1m, 0)
	// The upstream accepted work again: restore the connection's configured
	// concurrency instead of continuing to drip one request at a time.
	m.noteThrottleLocked(m.connectionForTarget(id), false, now)
	st.LatencyEWMAms = ewma(st.LatencyEWMAms, float64(latencyMS), m.cfg.EWMAAlpha)
	if ttftMS > 0 {
		st.TTFT_EWMAms = ewma(st.TTFT_EWMAms, float64(ttftMS), m.cfg.EWMAAlpha)
	}
	providerQuota := st.QuotaSource == "provider_api"
	m.refreshBucketsLocked(id, rt, now)
	if rt.quota == nil {
		rt.quota = map[string]*bucketUse{}
	}
	if len(quotas) > 0 {
		st.QuotaAsOf = now
		st.QuotaSource = "local_ledger"
	}
	for _, q := range quotas {
		if q.Units == 0 {
			continue
		}
		bu := rt.quota[q.BucketID]
		if bu == nil {
			bu = &bucketUse{resetAt: now.Add(time.Hour)}
			rt.quota[q.BucketID] = bu
		}
		bu.used += q.Units
	}
	if buckets := m.buckets[id]; len(buckets) > 0 {
		m.mirrorPrimaryLocked(rt, buckets, now)
	} else {
		for _, q := range quotas {
			st.QuotaUsed += q.Units
		}
	}
	connection := m.connectionForTarget(id)
	connection.st.Auth = domain.ConnectionAuthUsable
	connection.st.Reachability = domain.ConnectionReachable
	connection.reachabilityRecheckAt = time.Time{}
	connection.connectFailures = 0
	connection.st.RecoveryState = ""
	connection.st.RecoveryReason = ""
	connection.st.RetryAt = time.Time{}
	connection.st.CoolingUntil = time.Time{}
	connection.st.RecentThrottleRate = decayRate(connection.st.RecentThrottleRate, 0)
	connection.st.LastFailure = nil
	// An earlier in-flight success must not erase a newer provider observation.
	if providerQuota {
		return
	}
	if buckets := m.buckets[id]; len(buckets) > 0 {
		windows := make([]domain.QuotaWindowState, 0, len(buckets))
		for _, b := range buckets {
			w := domain.QuotaWindowState{
				Type: b.ID, Availability: domain.QuotaUnknown,
				ObservedAt: now, Confidence: st.QuotaConf,
			}
			if bu := rt.quota[b.ID]; bu != nil {
				w.ResetAt = bu.resetAt
			}
			if b.LimitPerCycle > 0 {
				used := 0.0
				if bu := rt.quota[b.ID]; bu != nil {
					used = bu.used
				}
				remaining := 1 - used/b.LimitPerCycle
				if remaining < 0 {
					remaining = 0
				}
				w.RemainingFraction = &remaining
				w.Availability = domain.QuotaAvailable
				if remaining <= 0 {
					w.Availability = domain.QuotaExhausted
				}
			}
			windows = append(windows, w)
		}
		connection.st.QuotaWindows = windows
	} else if st.QuotaLimit > 0 {
		remaining := st.QuotaRemainingFrac()
		availability := domain.QuotaAvailable
		if remaining <= 0 {
			availability = domain.QuotaExhausted
		}
		connection.st.QuotaWindows = []domain.QuotaWindowState{{
			Type: "legacy", Availability: availability, RemainingFraction: &remaining,
			ResetAt: st.QuotaResetAt, ObservedAt: now, Confidence: st.QuotaConf,
		}}
	} else {
		for i := range connection.st.QuotaWindows {
			connection.st.QuotaWindows[i].Availability = domain.QuotaAvailable
			connection.st.QuotaWindows[i].RemainingFraction = nil
			connection.st.QuotaWindows[i].ObservedAt = now
		}
	}
}

// RecordFailure feeds a typed failure into health/cooldown/breaker.
func (m *Manager) RecordFailure(id domain.ResourceTargetID, f *domain.Failure, now time.Time) {
	if f == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.managed && m.targets[id] == nil {
		return
	}
	defer m.markCooldownDirtyLocked()
	rt := m.get(id)
	st := &rt.st
	connection := m.connectionForTarget(id)
	if f.EffectiveScope() == domain.FailureScopeConnection {
		connection.st.LastFailure = f
		switch f.Type {
		case domain.FailAuth:
			connection.st.Auth = domain.ConnectionAuthError
			connection.st.RecoveryState = "needs_attention"
			connection.st.RecoveryReason = f.Message
			connection.st.RetryAt = time.Time{}
		case domain.FailConnect:
			connection.connectFailures++
			retryAt := now.Add(connectBackoff(connection.connectFailures))
			connection.st.Reachability = domain.ConnectionOffline
			connection.reachabilityRecheckAt = retryAt
			connection.st.RecoveryState = "backing_off"
			connection.st.RecoveryReason = f.Message
			connection.st.RetryAt = retryAt
			connection.st.CoolingUntil = retryAt
			m.noteThrottleLocked(connection, true, now)
		case domain.FailRateLimit:
			connection.st.RecentThrottleRate = decayRate(connection.st.RecentThrottleRate, 1)
			cooldown := m.cfg.CooldownOn429
			if f.RetryAfter > 0 {
				cooldown = f.RetryAfter
			}
			connection.st.CoolingUntil = now.Add(cooldown)
			connection.st.RecoveryState = "cooling"
			connection.st.RecoveryReason = f.Message
			connection.st.RetryAt = connection.st.CoolingUntil
			// Hold the admission queue for the cooldown, then drain it one at
			// a time, rather than failing everyone who is already waiting.
			m.noteThrottleLocked(connection, true, now)
		case domain.FailQuotaExhausted:
			if f.LocalQuotaProtection {
				connection.st.RecoveryState = "quota_exhausted"
				connection.st.RecoveryReason = f.Message
				connection.st.RetryAt = time.Time{}
				// Preserve independently resetting windows published by the guard.
				for _, window := range connection.st.QuotaWindows {
					if window.Availability == domain.QuotaExhausted {
						return
					}
				}
				connection.st.QuotaWindows = []domain.QuotaWindowState{{Type: "codex_usage_unknown", Availability: domain.QuotaExhausted, ObservedAt: now}}
				return
			}
			resetAt := st.QuotaResetAt
			if f.RetryAfter > 0 {
				resetAt = now.Add(f.RetryAfter)
				st.QuotaResetAt = resetAt
			}
			connection.st.QuotaWindows = []domain.QuotaWindowState{{
				Type: "legacy", Availability: domain.QuotaExhausted,
				ResetAt: resetAt, ObservedAt: now, Confidence: st.QuotaConf,
			}}
			connection.st.RecoveryState = "quota_exhausted"
			connection.st.RecoveryReason = f.Message
			connection.st.RetryAt = resetAt
			// Plan exhaustion is per-model; the account's other models keep
			// running but are serialized until one of them succeeds again.
			m.noteThrottleLocked(connection, true, now)
		}
	}

	switch f.Type.CircuitAction() {
	case domain.CircuitNeutral:
		if f.Type == domain.FailQuotaExhausted {
			// park until quota refresh if we know it
			if !st.QuotaResetAt.IsZero() && st.QuotaResetAt.After(now) {
				st.CooldownUntil = st.QuotaResetAt
			} else {
				st.CooldownUntil = now.Add(m.connectionRecheckInterval())
			}
			rt.quotaCooldownUntil = st.CooldownUntil
		}
		return
	}

	st.ConsecFails++
	st.ErrorRate1m = decayRate(st.ErrorRate1m, 1)
	st.LastFailure = f

	cooldown := time.Duration(0)
	if f.Type == domain.FailRateLimit {
		cooldown = m.cfg.CooldownOn429
		if f.RetryAfter > 0 {
			cooldown = f.RetryAfter
		}
	}
	if cooldown > 0 {
		st.CooldownUntil = now.Add(cooldown)
		rt.quotaCooldownUntil = time.Time{}
	}

	if st.Breaker == domain.BreakerHalfOpen {
		// failed probe: reopen with backoff doubling handled by caller cfg
		st.Breaker = domain.BreakerOpen
		st.OpenUntil = now.Add(m.cfg.OpenDuration * 2)
		return
	}
	if st.ConsecFails >= m.cfg.FailThreshold && st.Breaker == domain.BreakerClosed {
		st.Breaker = domain.BreakerOpen
		st.OpenUntil = now.Add(m.cfg.OpenDuration)
	}
}

// SetQuota manually overrides locally-known quota (console / provider API).
func (m *Manager) SetQuota(id domain.ResourceTargetID, used, limit float64, asOf time.Time, src string, conf domain.Confidence) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rt := m.get(id)
	rt.st.QuotaUsed = used
	rt.st.QuotaLimit = limit
	rt.st.QuotaAsOf = asOf
	rt.st.QuotaSource = src
	rt.st.QuotaConf = conf
	availability := domain.QuotaUnknown
	var remaining *float64
	if limit > 0 {
		value := 1 - used/limit
		if value < 0 {
			value = 0
		}
		remaining = &value
		availability = domain.QuotaAvailable
		if value == 0 {
			availability = domain.QuotaExhausted
		}
	}
	connection := m.connectionForTarget(id)
	connection.st.QuotaWindows = []domain.QuotaWindowState{{
		Type: src, Availability: availability, RemainingFraction: remaining,
		ObservedAt: asOf, Confidence: conf, ResetAt: rt.st.QuotaResetAt,
	}}
}

// ApplyProviderQuota publishes provider-reported quota windows onto a
// connection and mirrors the most restrictive one into each target's
// single-window policy surface (QuotaSource "provider_api"). It never touches
// the attempt ledger or local per-bucket accounting: provider fractions are
// authoritative while fresh, local buckets resume after the reset passes.
func (m *Manager) ApplyProviderQuota(connectionID string, windows []domain.QuotaWindowState, now time.Time) {
	m.ApplyProviderQuotaForIdentity(connectionID, "", windows, now)
}

// ApplyProviderQuotaForIdentity rejects observations fetched before a rebind.
func (m *Manager) ApplyProviderQuotaForIdentity(connectionID, identity string, windows []domain.QuotaWindowState, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if identity != "" && m.identities[connectionID] != identity {
		return
	}
	defer m.markCooldownDirtyLocked()
	connection := m.connections[connectionID]
	if connection == nil {
		return
	}
	connection.st.QuotaWindows = append([]domain.QuotaWindowState(nil), windows...)
	worstFrac := 1.0
	var worst *domain.QuotaWindowState
	for i := range windows {
		w := &windows[i]
		if w.RemainingFraction == nil {
			continue
		}
		frac := *w.RemainingFraction
		if frac < 0 {
			frac = 0
		}
		if frac < worstFrac || worst == nil {
			worstFrac = frac
			worst = w
		}
	}
	for id, cid := range m.targetConnections {
		if cid != connectionID {
			continue
		}
		rt := m.targets[id]
		if rt == nil {
			continue
		}
		st := &rt.st
		// A fresh quota observation replaces the previous quota parking. Keep
		// unrelated 429/backoff deadlines intact.
		if !rt.quotaCooldownUntil.IsZero() && st.CooldownUntil.Equal(rt.quotaCooldownUntil) {
			st.CooldownUntil = time.Time{}
			rt.quotaCooldownUntil = time.Time{}
		}
		if worst == nil {
			st.QuotaLimit = 0
			continue
		}
		st.QuotaUsed = 1 - worstFrac
		st.QuotaLimit = 1
		if worst.Availability == domain.QuotaAvailable && worstFrac == 0 {
			st.QuotaLimit = 0
		} // explicit paid overflow
		st.QuotaResetAt = worst.ResetAt
		st.QuotaAsOf = now
		st.QuotaSource = "provider_api"
		st.QuotaConf = domain.ConfMedium
		if worst.Availability == domain.QuotaExhausted && worst.ResetAt.After(now) {
			st.CooldownUntil = worst.ResetAt
			rt.quotaCooldownUntil = worst.ResetAt
		}
	}
}

// ClearCooldown force-clears cooldown and breaker (manual probe).
func (m *Manager) ClearCooldown(id domain.ResourceTargetID) {
	_ = m.ResetCooldowns([]domain.ResourceTargetID{id})
}

// Enter marks a target as serving one more in-flight request (least_used
// ranking input). Exit releases it. Always paired via defer.
func (m *Manager) Enter(id domain.ResourceTargetID) {
	m.mu.Lock()
	m.get(id).st.InFlight++
	m.connectionForTarget(id).st.InFlight++
	m.mu.Unlock()
}

// Exit releases one in-flight slot previously taken with Enter.
func (m *Manager) Exit(id domain.ResourceTargetID) {
	m.mu.Lock()
	if rt, ok := m.targets[id]; ok && rt.st.InFlight > 0 {
		rt.st.InFlight--
	}
	if connectionID := m.targetConnections[id]; connectionID != "" {
		if connection := m.connections[connectionID]; connection != nil && connection.st.InFlight > 0 {
			connection.st.InFlight--
		}
	}
	m.mu.Unlock()
}

func (m *Manager) connectionForTarget(id domain.ResourceTargetID) *connectionRuntime {
	connectionID := m.targetConnections[id]
	if connectionID == "" {
		connectionID = string(id)
		m.targetConnections[id] = connectionID
	}
	connection := m.connections[connectionID]
	if connection == nil {
		connection = &connectionRuntime{st: domain.ConnectionState{
			ConnectionID: connectionID, Enabled: true,
			Auth:         domain.ConnectionAuthUnknown,
			Reachability: domain.ConnectionReachabilityUnknown,
		}}
		m.connections[connectionID] = connection
	}
	return connection
}

// ConnectionStates returns account-wide runtime state independently from
// Connection×Model target telemetry.
func (m *Manager) ConnectionStates(now time.Time) map[string]domain.ConnectionState {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]domain.ConnectionState, len(m.connections))
	for id, runtime := range m.connections {
		state := &runtime.st
		if !state.CoolingUntil.After(now) {
			state.CoolingUntil = time.Time{}
		}
		// A connect failure marks one bounded unavailable interval. Expiry
		// enters the existing unknown state, which is the half-open/recheck
		// phase: the next real attempt proves reachable or starts a new bound.
		if state.Reachability == domain.ConnectionOffline &&
			!runtime.reachabilityRecheckAt.IsZero() && !runtime.reachabilityRecheckAt.After(now) {
			state.Reachability = domain.ConnectionReachabilityUnknown
			runtime.reachabilityRecheckAt = time.Time{}
			state.RecoveryState = "ready_to_probe"
			state.RetryAt = time.Time{}
		}
		if state.RecoveryState == "cooling" && !state.CoolingUntil.After(now) {
			state.RecoveryState = "ready_to_probe"
			state.RetryAt = time.Time{}
		}
		for i := range state.QuotaWindows {
			window := &state.QuotaWindows[i]
			if window.Availability != domain.QuotaExhausted {
				continue
			}
			switch {
			case !window.ResetAt.IsZero() && !window.ResetAt.After(now):
				// A known reset boundary is positive recovery evidence.
				window.Availability = domain.QuotaAvailable
				window.RemainingFraction = nil
				window.ResetAt = time.Time{}
				window.ObservedAt = now
			case window.ResetAt.IsZero():
				if window.ObservedAt.IsZero() {
					window.ObservedAt = now
				} else if !window.ObservedAt.Add(m.connectionRecheckInterval()).After(now) {
					// Without a provider reset hint, bounded aging returns to
					// Unknown so a probe can refresh the real quota state.
					window.Availability = domain.QuotaUnknown
					window.RemainingFraction = nil
					window.ObservedAt = now
				}
			}
		}
		copyState := *state
		copyState.QuotaWindows = append([]domain.QuotaWindowState(nil), state.QuotaWindows...)
		out[id] = copyState
	}
	return out
}

// States returns a copy of all raw states for console display.
func (m *Manager) States(now time.Time) map[domain.ResourceTargetID]domain.TargetState {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[domain.ResourceTargetID]domain.TargetState, len(m.targets))
	for id, rt := range m.targets {
		st := rt.st
		if st.Breaker == domain.BreakerOpen && !st.OpenUntil.After(now) {
			st.Breaker = domain.BreakerHalfOpen
		}
		out[id] = st
	}
	return out
}

func ewma(prev, cur, alpha float64) float64 {
	if prev <= 0 {
		return cur
	}
	return alpha*cur + (1-alpha)*prev
}

// decayRate blends a new binary outcome into a smoothed error rate.
func decayRate(prev float64, outcome float64) float64 {
	const a = 0.2
	return a*outcome + (1-a)*prev
}
