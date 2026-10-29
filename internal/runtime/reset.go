package runtime

import (
	"time"

	"ferry/internal/domain"
)

// ReconcileTargets retires removed resources and invalidates state whose
// credential owner changed. Called before publishing each compiled world.
func (m *Manager) ReconcileTargets(snap *domain.CompiledSnapshot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.managed = true
	identities := make(map[string]string, len(snap.Connections))
	for id, c := range snap.Connections {
		identities[id] = c.RuntimeIdentity()
	}
	for id, rt := range m.targets {
		cid := m.targetConnections[id]
		if _, exists := snap.Targets[id]; !exists {
			delete(m.targets, id)
			delete(m.targetConnections, id)
			delete(m.buckets, id)
			delete(m.plans, id)
		} else if m.identities[cid] != identities[cid] {
			resetTargetLocked(rt)
		}
	}
	for id, connection := range m.connections {
		// RegisterExecutionTargetBuckets rebuilds these two configuration-derived
		// fields by aggregating every target in the new world. Clear the previous
		// generation first so a disabled or credential-less world cannot inherit
		// an earlier usable value.
		connection.st.Enabled = false
		connection.st.Auth = domain.ConnectionAuthUnknown
		queue := connection.adm.queue[:0]
		for _, ticket := range connection.adm.queue {
			if m.targets[ticket.target] == nil {
				ticket.failure = &domain.Failure{Type: domain.FailConnect, Scope: domain.FailureScopeRequest, Message: "connection configuration changed while queued"}
				close(ticket.ready)
			} else {
				queue = append(queue, ticket)
			}
		}
		connection.adm.queue = queue
		_, exists := identities[id]
		// Legacy targets have no V2 connection projection.
		if !exists {
			for tid, target := range snap.Targets {
				if (target != nil && target.ConnectionID == id) || string(tid) == id {
					exists = true
					break
				}
			}
		}
		if !exists || m.identities[id] != identities[id] {
			m.resetConnectionLocked(connection)
		}
		if !exists {
			delete(m.connections, id)
		}
	}
	m.identities = identities
	m.markCooldownDirtyLocked()
}

func resetTargetLocked(rt *targetRuntime) {
	// Usage in the durable attempt ledger is untouched. This clears only the
	// runtime observations which otherwise immediately re-park the target.
	inFlight, latency, ttft := rt.st.InFlight, rt.st.LatencyEWMAms, rt.st.TTFT_EWMAms
	*rt = *newTargetRuntime()
	rt.st.InFlight, rt.st.LatencyEWMAms, rt.st.TTFT_EWMAms = inFlight, latency, ttft
}

func (m *Manager) resetConnectionLocked(c *connectionRuntime) {
	c.st.CoolingUntil = time.Time{}
	c.st.QuotaWindows = nil
	c.st.LastFailure = nil
	c.st.RecentThrottleRate = 0
	c.st.Reachability = domain.ConnectionReachabilityUnknown
	c.st.Auth = domain.ConnectionAuthUnknown
	c.reachabilityRecheckAt = time.Time{}
	c.connectFailures = 0
	c.st.RecoveryState = ""
	c.st.RecoveryReason = ""
	c.st.RetryAt = time.Time{}
	c.adm.throttled = false
	if c.adm.timer != nil {
		c.adm.timer.Stop()
		c.adm.timer = nil
	}
}

// ResetCooldowns clears selected model state plus the shared connection gates
// needed to retry them. Sibling model cooldowns remain intact. Nil selects all.
// A successful return guarantees the reset snapshot has been written.
func (m *Manager) ResetCooldowns(ids []domain.ResourceTargetID) error {
	m.mu.Lock()
	connections := map[string]bool{}
	if ids == nil {
		for id := range m.targets {
			ids = append(ids, id)
		}
		for id := range m.connections {
			connections[id] = true
		}
	}
	for _, id := range ids {
		if rt := m.targets[id]; rt != nil {
			resetTargetLocked(rt)
			connections[m.targetConnections[id]] = true
		}
	}
	for id := range connections {
		if c := m.connections[id]; c != nil {
			m.resetConnectionLocked(c)
			m.pumpLocked(c, time.Now())
		}
	}
	m.markCooldownDirtyLocked()
	m.mu.Unlock()
	return m.saveCooldowns()
}
