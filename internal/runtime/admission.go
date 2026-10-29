package runtime

import (
	"context"
	"fmt"
	"time"

	"ferry/internal/domain"
)

// Admission is a strict-FIFO gate in front of one Connection's upstream.
//
// It exists for two reasons. The first is plain concurrency control: an
// account has a finite number of simultaneous requests it can make without
// looking abnormal, and that bound belongs on the connection, not on the
// caller. The second is throttle behavior: when the upstream rate limits us,
// failing the waiting requests turns a slowdown into an outage. Instead the
// gate holds them in arrival order, waits out the cooldown, and then releases
// them one at a time until the connection proves healthy again.
type admissionTicket struct {
	target  domain.ResourceTargetID
	ready   chan struct{}
	failure *domain.Failure
}

type connectionAdmission struct {
	policy domain.ConcurrencyPolicy
	queue  []*admissionTicket
	// throttled drains the queue serially after a rate limit or quota
	// rejection. It clears on the next success, so a connection returns to its
	// configured concurrency only once the upstream has accepted work again.
	throttled bool
	timer     *time.Timer
}

// SetConnectionConcurrency installs the admission policy for one connection.
// Applied on every snapshot publish; waiters already queued keep their place.
func (m *Manager) SetConnectionConcurrency(connectionID string, policy *domain.ConcurrencyPolicy) {
	if connectionID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	connection := m.connections[connectionID]
	if connection == nil {
		connection = &connectionRuntime{st: domain.ConnectionState{
			ConnectionID: connectionID, Enabled: true,
			Auth:         domain.ConnectionAuthUnknown,
			Reachability: domain.ConnectionReachabilityUnknown,
		}}
		m.connections[connectionID] = connection
	}
	if policy != nil {
		connection.adm.policy = *policy
	} else {
		connection.adm.policy = domain.ConcurrencyPolicy{}
	}
	// A widened limit must wake waiters that the old one was holding back.
	m.pumpLocked(connection, time.Now())
}

// Admit blocks until this attempt may contact the upstream, then returns the
// release function that must be called when the attempt finishes. A returned
// failure means the request never took a slot and release must not be called.
func (m *Manager) Admit(ctx context.Context, id domain.ResourceTargetID) (func(), *domain.Failure) {
	m.mu.Lock()
	if m.managed && m.targets[id] == nil {
		m.mu.Unlock()
		return nil, &domain.Failure{Type: domain.FailConnect, Scope: domain.FailureScopeRequest, Message: "connection configuration changed before admission"}
	}
	connection := m.connectionForTarget(id)
	now := time.Now()
	if len(connection.adm.queue) == 0 && connection.adm.inFlightCapacityLocked(connection, now) > connection.st.InFlight {
		m.grantLocked(connection, id)
		m.mu.Unlock()
		return m.releaseFunc(connection, id), nil
	}
	throttleConstrained := connection.adm.throttleConstrainedLocked(connection, now)
	if (throttleConstrained && !connection.adm.policy.QueuesOnThrottle()) ||
		(!throttleConstrained && !connection.adm.policy.QueuesWhenFull()) {
		connectionID := connection.st.ConnectionID
		reason := "concurrency limit reached"
		if throttleConstrained {
			reason = "upstream throttle cooldown is active"
		}
		m.mu.Unlock()
		return nil, &domain.Failure{
			Type: domain.FailRateLimit, Scope: domain.FailureScopeConnection,
			HTTPStatus: 429,
			Message:    fmt.Sprintf("connection %s %s; admission is configured to fail immediately", connectionID, reason),
		}
	}
	if limit := connection.adm.policy.EffectiveMaxQueue(); len(connection.adm.queue) >= limit {
		connectionID := connection.st.ConnectionID
		m.mu.Unlock()
		return nil, &domain.Failure{
			Type: domain.FailRateLimit, Scope: domain.FailureScopeConnection,
			HTTPStatus: 429,
			Message: fmt.Sprintf("connection %s admission queue is full (%d requests already waiting)",
				connectionID, limit),
		}
	}
	ticket := &admissionTicket{target: id, ready: make(chan struct{})}
	connection.adm.queue = append(connection.adm.queue, ticket)
	m.armCooldownTimerLocked(connection, now)
	wait := connection.adm.policy.EffectiveMaxWait()
	m.mu.Unlock()

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ticket.ready:
		if ticket.failure != nil {
			return nil, ticket.failure
		}
		return m.releaseFunc(connection, id), nil
	case <-ctx.Done():
		return nil, m.abandon(connection, ticket, &domain.Failure{
			Type: domain.FailClientCancel, Scope: domain.FailureScopeRequest,
			Message: "client canceled while waiting for a connection slot"})
	case <-timer.C:
		return nil, m.abandon(connection, ticket, &domain.Failure{
			Type: domain.FailRateLimit, Scope: domain.FailureScopeConnection, HTTPStatus: 429,
			Message: fmt.Sprintf("waited %s for a slot on connection %s without being admitted",
				wait, connection.st.ConnectionID)})
	}
}

// abandon removes a waiter that gave up. It loses a race with a concurrent
// grant, in which case the slot is already held and must be handed straight
// back rather than leaked.
func (m *Manager) abandon(connection *connectionRuntime, ticket *admissionTicket, failure *domain.Failure) *domain.Failure {
	m.mu.Lock()
	if ticket.failure != nil {
		m.mu.Unlock()
		return failure
	}
	for i, queued := range connection.adm.queue {
		if queued == ticket {
			connection.adm.queue = append(connection.adm.queue[:i], connection.adm.queue[i+1:]...)
			m.mu.Unlock()
			return failure
		}
	}
	// Already granted between the wake-up and this lock: return the slot.
	m.releaseLocked(connection, ticket.target, time.Now())
	m.mu.Unlock()
	return failure
}

func (m *Manager) releaseFunc(connection *connectionRuntime, id domain.ResourceTargetID) func() {
	var once bool
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if once {
			return
		}
		once = true
		m.releaseLocked(connection, id, time.Now())
	}
}

func (m *Manager) grantLocked(connection *connectionRuntime, id domain.ResourceTargetID) {
	if connection.st.RecoveryState == "ready_to_probe" {
		connection.st.RecoveryState = "probing"
	}
	if target, ok := m.targets[id]; ok {
		target.st.InFlight++
	} else {
		m.get(id).st.InFlight++
	}
	connection.st.InFlight++
}

func (m *Manager) releaseLocked(connection *connectionRuntime, id domain.ResourceTargetID, now time.Time) {
	if target, ok := m.targets[id]; ok && target.st.InFlight > 0 {
		target.st.InFlight--
	}
	if connection.st.InFlight > 0 {
		connection.st.InFlight--
	}
	m.pumpLocked(connection, now)
}

// inFlightCapacityLocked reports how many attempts this connection may have
// running right now. Zero while cooling: the queue holds and nothing is sent.
func (a *connectionAdmission) inFlightCapacityLocked(connection *connectionRuntime, now time.Time) int64 {
	if now.Before(connection.st.CoolingUntil) {
		return 0
	}
	if a.throttled {
		// Recovering: drip one request at a time so a connection that just
		// hit a limit is probed rather than slammed.
		return 1
	}
	if limit := a.policy.Limit(); limit > 0 {
		return int64(limit)
	}
	return int64(1) << 62
}

func (a *connectionAdmission) throttleConstrainedLocked(connection *connectionRuntime, now time.Time) bool {
	return now.Before(connection.st.CoolingUntil) || a.throttled
}

// pumpLocked grants the queue head for as long as capacity allows, preserving
// strict arrival order. When the only thing blocking the head is an unexpired
// cooldown it arms a timer so the queue resumes on its own.
func (m *Manager) pumpLocked(connection *connectionRuntime, now time.Time) {
	for len(connection.adm.queue) > 0 &&
		connection.adm.inFlightCapacityLocked(connection, now) > connection.st.InFlight {
		ticket := connection.adm.queue[0]
		connection.adm.queue = connection.adm.queue[1:]
		m.grantLocked(connection, ticket.target)
		close(ticket.ready)
	}
	m.armCooldownTimerLocked(connection, now)
}

func (m *Manager) armCooldownTimerLocked(connection *connectionRuntime, now time.Time) {
	if len(connection.adm.queue) == 0 || !now.Before(connection.st.CoolingUntil) {
		return
	}
	delay := connection.st.CoolingUntil.Sub(now)
	if connection.adm.timer != nil {
		connection.adm.timer.Stop()
	}
	connection.adm.timer = time.AfterFunc(delay, func() {
		m.mu.Lock()
		m.pumpLocked(connection, time.Now())
		m.mu.Unlock()
	})
}

// noteThrottleLocked puts the connection into serialized drain mode.
func (m *Manager) noteThrottleLocked(connection *connectionRuntime, throttled bool, now time.Time) {
	if connection == nil {
		return
	}
	connection.adm.throttled = throttled
	m.markCooldownDirtyLocked()
	m.pumpLocked(connection, now)
}
