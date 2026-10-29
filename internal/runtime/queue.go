package runtime

import (
	"ferry/internal/domain"
	"sort"
	"time"
)

type QueueState struct {
	ConnectionID    string    `json:"connection_id"`
	InFlight        int64     `json:"in_flight"`
	Waiting         int       `json:"waiting"`
	MaxConcurrent   int       `json:"max_concurrent"`
	MaxQueue        int       `json:"max_queue"`
	MaxWaitSeconds  int       `json:"max_wait_seconds"`
	QueueWhenFull   bool      `json:"queue_when_full"`
	QueueOnThrottle bool      `json:"queue_on_throttle"`
	CoolingUntil    time.Time `json:"cooling_until"`
	Recovering      bool      `json:"recovering"`
}

// QueueStates reads the gate itself, including background jobs sharing capacity.
func (m *Manager) QueueStates() []QueueState {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]QueueState, 0, len(m.connections))
	for id, c := range m.connections {
		p := c.adm.policy
		out = append(out, QueueState{ConnectionID: id, InFlight: c.st.InFlight, Waiting: len(c.adm.queue),
			MaxConcurrent: p.Limit(), MaxQueue: p.EffectiveMaxQueue(), MaxWaitSeconds: int(p.EffectiveMaxWait() / time.Second),
			QueueWhenFull: p.QueuesWhenFull(), QueueOnThrottle: p.QueuesOnThrottle(), CoolingUntil: c.st.CoolingUntil, Recovering: c.adm.throttled})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ConnectionID < out[j].ConnectionID })
	return out
}

// EffectiveConcurrency exposes normalized default values for operator forms.
func EffectiveConcurrency(p *domain.ConcurrencyPolicy) domain.ConcurrencyPolicy {
	full, throttle := p.QueuesWhenFull(), p.QueuesOnThrottle()
	return domain.ConcurrencyPolicy{MaxConcurrent: p.Limit(), MaxQueue: p.EffectiveMaxQueue(), MaxWaitSeconds: int(p.EffectiveMaxWait() / time.Second), QueueWhenFull: &full, QueueOnThrottle: &throttle}
}
