package runtime

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"ferry/internal/domain"
)

func resetWorld(m *Manager, cs ...domain.Connection) *domain.CompiledSnapshot {
	s := &domain.CompiledSnapshot{Connections: map[string]*domain.Connection{}, Targets: map[domain.ResourceTargetID]*domain.CompiledTarget{}}
	for i := range cs {
		c := &cs[i]
		s.Connections[c.ID] = c
		for _, model := range []string{"a", "b"} {
			id := c.RuntimeTargetID(model)
			s.Targets[id] = &domain.CompiledTarget{ID: id, ConnectionID: c.ID, ProviderID: c.ProviderID, Model: model}
		}
	}
	m.ReconcileTargets(s)
	for id, target := range s.Targets {
		m.RegisterExecutionTarget(id, target.ConnectionID, true, true, nil, nil)
	}
	return s
}

func resetTestConnection(id string) domain.Connection {
	return domain.Connection{ID: id, RuntimeID: "uuid-" + id, ProviderID: "provider", Credential: domain.CredentialRef{SecretRef: "store:old"}}
}

func TestScopedResetSurvivesConcurrentSaveAndRestart(t *testing.T) {
	m := NewManager(DefaultHealthConfig())
	c, other := resetTestConnection("same-name"), resetTestConnection("other")
	resetWorld(m, c, other)
	m.cooldownPath = filepath.Join(t.TempDir(), cooldownStateFile)
	now := time.Now()
	for _, connection := range []domain.Connection{c, other} {
		for _, model := range []string{"a", "b"} {
			m.RecordFailure(connection.RuntimeTargetID(model), &domain.Failure{Type: domain.FailQuotaExhausted, Scope: domain.FailureScopeConnection, RetryAfter: time.Hour}, now)
		}
	}
	var writes sync.WaitGroup
	writes.Go(func() {
		for range 10 {
			if err := m.saveCooldowns(); err != nil {
				t.Error(err)
			}
		}
	})
	if err := m.ResetCooldowns([]domain.ResourceTargetID{c.RuntimeTargetID("a")}); err != nil {
		t.Fatal(err)
	}
	writes.Wait()
	second := NewManager(DefaultHealthConfig())
	resetWorld(second, c, other)
	if err := second.loadCooldowns(m.cooldownPath); err != nil {
		t.Fatal(err)
	}
	if st := second.Snapshot(now).Get(c.RuntimeTargetID("a")); st.CooldownUntil.After(now) || st.QuotaUsed != 0 {
		t.Fatalf("reset model parked again: %+v", st)
	}
	for _, id := range []domain.ResourceTargetID{c.RuntimeTargetID("b"), other.RuntimeTargetID("a")} {
		if !second.States(now)[id].CooldownUntil.After(now) {
			t.Fatalf("unselected model lost cooldown: %s", id)
		}
	}
	if len(second.ConnectionStates(now)[c.ID].QuotaWindows) != 0 {
		t.Fatal("shared exhausted quota survived reset")
	}
	if err := second.ResetCooldowns(nil); err != nil {
		t.Fatal(err)
	}
	for _, state := range second.Snapshot(now).Targets {
		if state.CooldownUntil.After(now) || state.Breaker != domain.BreakerClosed {
			t.Fatal("global reset left a blocking model")
		}
	}
}

func TestRebindRetiresOldOutcomesAndPersistedOwnership(t *testing.T) {
	m := NewManager(DefaultHealthConfig())
	c := resetTestConnection("same-name")
	resetWorld(m, c)
	id, oldIdentity := c.RuntimeTargetID("a"), c.RuntimeIdentity()
	m.cooldownPath = filepath.Join(t.TempDir(), cooldownStateFile)
	failure := &domain.Failure{Type: domain.FailRateLimit, Scope: domain.FailureScopeConnection, RetryAfter: time.Hour}
	m.RecordFailure(id, failure, time.Now())
	if err := m.saveCooldowns(); err != nil {
		t.Fatal(err)
	}
	c.Credential.SecretRef = "store:healthy"
	resetWorld(m, c)
	m.RecordFailure(id, failure, time.Now())
	m.RecordSuccess(id, 1, 1, domain.Usage{}, 0, 0, time.Now())
	m.ApplyProviderQuotaForIdentity(c.ID, oldIdentity, []domain.QuotaWindowState{{Availability: domain.QuotaExhausted, ResetAt: time.Now().Add(time.Hour)}}, time.Now())
	if _, exists := m.States(time.Now())[id]; exists {
		t.Fatal("late outcome resurrected old target")
	}
	if err := m.loadCooldowns(m.cooldownPath); err != nil {
		t.Fatal(err)
	}
	if st := m.ConnectionStates(time.Now())[c.ID]; st.CoolingUntil.After(time.Now()) || len(st.QuotaWindows) != 0 {
		t.Fatalf("old account restored onto new binding: %+v", st)
	}
	if st := m.States(time.Now())[c.RuntimeTargetID("a")]; st.CooldownUntil.After(time.Now()) {
		t.Fatal("new binding inherited target cooldown")
	}
	resetWorld(m)
	resetWorld(m, c)
	if len(m.targets) != 2 || len(m.connections) != 1 {
		t.Fatal("deleted resources were not pruned")
	}
}

func TestConnectionRegistrationAggregatesMixedTargetsAcrossReconcile(t *testing.T) {
	m := NewManager(DefaultHealthConfig())
	c := resetTestConnection("mixed")
	enabledID := c.RuntimeTargetID("enabled")
	disabledID := c.RuntimeTargetID("disabled")
	snapshot := &domain.CompiledSnapshot{
		Connections: map[string]*domain.Connection{c.ID: &c},
		Targets: map[domain.ResourceTargetID]*domain.CompiledTarget{
			enabledID:  {ID: enabledID, ConnectionID: c.ID},
			disabledID: {ID: disabledID, ConnectionID: c.ID, Disabled: true},
		},
	}

	for _, registrations := range [][]struct {
		id                domain.ResourceTargetID
		enabled, callable bool
	}{
		{{disabledID, false, false}, {enabledID, true, true}},
		{{enabledID, true, true}, {disabledID, false, false}},
	} {
		m.ReconcileTargets(snapshot)
		for _, registration := range registrations {
			m.RegisterExecutionTarget(registration.id, c.ID, registration.enabled, registration.callable, nil, nil)
		}
		state := m.ConnectionStates(time.Now())[c.ID]
		if !state.Enabled || state.Auth != domain.ConnectionAuthUsable {
			t.Fatalf("mixed connection depended on registration order: %+v", state)
		}
	}

	disabledOnly := &domain.CompiledSnapshot{
		Connections: snapshot.Connections,
		Targets: map[domain.ResourceTargetID]*domain.CompiledTarget{
			disabledID: {ID: disabledID, ConnectionID: c.ID, Disabled: true},
		},
	}
	m.ReconcileTargets(disabledOnly)
	m.RegisterExecutionTarget(disabledID, c.ID, false, false, nil, nil)
	state := m.ConnectionStates(time.Now())[c.ID]
	if state.Enabled || state.Auth != domain.ConnectionAuthUnknown {
		t.Fatalf("disabled generation inherited prior availability: %+v", state)
	}
}

func TestResetWakesQueuedRequestsAndPreservesInFlight(t *testing.T) {
	m := NewManager(DefaultHealthConfig())
	c := resetTestConnection("c")
	resetWorld(m, c)
	id := c.RuntimeTargetID("a")
	m.SetConnectionConcurrency(c.ID, &domain.ConcurrencyPolicy{MaxConcurrent: 2})
	release, failure := m.Admit(context.Background(), id)
	if failure != nil {
		t.Fatal(failure)
	}
	defer release()
	m.RecordFailure(id, &domain.Failure{Type: domain.FailRateLimit, Scope: domain.FailureScopeConnection, RetryAfter: time.Hour}, time.Now())
	done := make(chan *domain.Failure, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		release, failure := m.Admit(ctx, id)
		if release != nil {
			release()
		}
		done <- failure
	}()
	deadline := time.Now().Add(time.Second)
	for {
		m.mu.Lock()
		queued := len(m.connections[c.ID].adm.queue) > 0
		m.mu.Unlock()
		if queued {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("request never queued")
		}
		time.Sleep(time.Millisecond)
	}
	if err := m.ResetCooldowns([]domain.ResourceTargetID{id}); err != nil {
		t.Fatal(err)
	}
	if failure := <-done; failure != nil {
		t.Fatal(failure)
	}
	if m.ConnectionStates(time.Now())[c.ID].InFlight != 1 {
		t.Fatal("reset corrupted in-flight accounting")
	}
}

func TestHealthyProviderQuotaClearsOnlyQuotaParking(t *testing.T) {
	m := NewManager(DefaultHealthConfig())
	c := resetTestConnection("c")
	resetWorld(m, c)
	now := time.Now()
	zero, half := 0.0, 0.5
	m.ApplyProviderQuota(c.ID, []domain.QuotaWindowState{{Availability: domain.QuotaExhausted, RemainingFraction: &zero, ResetAt: now.Add(4 * time.Hour)}}, now)
	m.RecordFailure(c.RuntimeTargetID("b"), &domain.Failure{Type: domain.FailRateLimit, RetryAfter: time.Minute}, now)
	// The cooldown cause must survive restart, even though quota source is not
	// part of the older persisted target projection.
	m.cooldownPath = filepath.Join(t.TempDir(), cooldownStateFile)
	if err := m.saveCooldowns(); err != nil {
		t.Fatal(err)
	}
	restored := NewManager(DefaultHealthConfig())
	resetWorld(restored, c)
	if err := restored.loadCooldowns(m.cooldownPath); err != nil {
		t.Fatal(err)
	}
	m = restored
	m.ApplyProviderQuota(c.ID, []domain.QuotaWindowState{{Availability: domain.QuotaAvailable, RemainingFraction: &half, ResetAt: now.Add(time.Hour)}}, now)
	if m.States(now)[c.RuntimeTargetID("a")].CooldownUntil.After(now) {
		t.Fatal("healthy observation retained old quota parking")
	}
	if !m.States(now)[c.RuntimeTargetID("b")].CooldownUntil.After(now) {
		t.Fatal("healthy quota erased unrelated rate-limit backoff")
	}
}

func TestResetReportsPersistenceFailure(t *testing.T) {
	m := NewManager(DefaultHealthConfig())
	c := resetTestConnection("c")
	resetWorld(m, c)
	m.cooldownPath = filepath.Join(t.TempDir(), "missing", cooldownStateFile)
	m.RecordFailure(c.RuntimeTargetID("a"), &domain.Failure{Type: domain.FailRateLimit, RetryAfter: time.Hour}, time.Now())
	if err := m.ResetCooldowns(nil); err == nil {
		t.Fatal("reset acknowledged a failed write")
	}
	if m.States(time.Now())[c.RuntimeTargetID("a")].CooldownUntil.After(time.Now()) {
		t.Fatal("memory reset not applied")
	}
}

func TestLegacyCooldownWithoutOwnershipIsDiscarded(t *testing.T) {
	m := NewManager(DefaultHealthConfig())
	c := resetTestConnection("c")
	resetWorld(m, c)
	path := filepath.Join(t.TempDir(), cooldownStateFile)
	if err := os.WriteFile(path, []byte(`{"connections":{"c":{"throttled":true}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.loadCooldowns(path); err != nil {
		t.Fatal(err)
	}
	if m.connections[c.ID].adm.throttled {
		t.Fatal("unowned legacy state was restored")
	}
}
