package runtime

import (
	"testing"
	"time"

	"ferry/internal/domain"
)

func TestCooldownStateSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	id := domain.ResourceTargetID("conn-a:model")

	first := NewManager(DefaultHealthConfig())
	first.RegisterExecutionTarget(id, "conn-a", true, true, nil, nil)
	first.cooldownPath = dir + "/" + cooldownStateFile
	first.RecordFailure(id, &domain.Failure{
		Type: domain.FailRateLimit, Scope: domain.FailureScopeConnection, RetryAfter: 20 * time.Minute,
	}, time.Now())
	if err := first.saveCooldowns(); err != nil {
		t.Fatalf("save: %v", err)
	}

	second := NewManager(DefaultHealthConfig())
	if err := second.loadCooldowns(dir + "/" + cooldownStateFile); err != nil {
		t.Fatalf("load: %v", err)
	}
	second.RegisterExecutionTarget(id, "conn-a", true, true, nil, nil)

	second.mu.Lock()
	connection := second.connections["conn-a"]
	cooling := connection.st.CoolingUntil
	throttled := connection.adm.throttled
	second.mu.Unlock()
	if !cooling.After(time.Now().Add(10 * time.Minute)) {
		t.Fatalf("cooldown not restored: cooling_until=%s", cooling)
	}
	if !throttled {
		t.Fatal("serialized-drain state not restored")
	}
}

func TestCooldownLoadDiscardsExpiredState(t *testing.T) {
	dir := t.TempDir()
	id := domain.ResourceTargetID("conn-b:model")

	first := NewManager(DefaultHealthConfig())
	first.RegisterExecutionTarget(id, "conn-b", true, true, nil, nil)
	first.cooldownPath = dir + "/" + cooldownStateFile
	first.RecordFailure(id, &domain.Failure{
		Type: domain.FailRateLimit, Scope: domain.FailureScopeConnection, RetryAfter: time.Millisecond,
	}, time.Now())
	if err := first.saveCooldowns(); err != nil {
		t.Fatalf("save: %v", err)
	}
	time.Sleep(5 * time.Millisecond)

	second := NewManager(DefaultHealthConfig())
	if err := second.loadCooldowns(dir + "/" + cooldownStateFile); err != nil {
		t.Fatalf("load: %v", err)
	}
	second.mu.Lock()
	connection := second.connections["conn-b"]
	second.mu.Unlock()
	if connection != nil && connection.st.CoolingUntil.After(time.Now()) {
		t.Fatal("an already-expired cooldown was resurrected")
	}
}

func TestCooldownSaveIsANoOpWithoutAPath(t *testing.T) {
	m := NewManager(DefaultHealthConfig())
	if err := m.saveCooldowns(); err != nil {
		t.Fatalf("save without a configured path: %v", err)
	}
}
