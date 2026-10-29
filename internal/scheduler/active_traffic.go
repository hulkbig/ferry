package scheduler

import (
	"sort"
	"time"

	"ferry/internal/domain"
)

type ActiveTrafficPhase string

const (
	TrafficReceived   ActiveTrafficPhase = "received"
	TrafficWaiting    ActiveTrafficPhase = "waiting"
	TrafficActive     ActiveTrafficPhase = "active"
	TrafficRecovering ActiveTrafficPhase = "recovering"
	TrafficStreaming  ActiveTrafficPhase = "streaming"
	TrafficDelivering ActiveTrafficPhase = "delivering"
)

// ActiveTrafficRecord is a safe, ephemeral product view of one in-flight
// client request. It deliberately contains no private ResourceTarget ID.
type ActiveTrafficRecord struct {
	RequestID       string             `json:"request_id"`
	LogicalModelID  string             `json:"logical_model_id"`
	ProviderID      string             `json:"provider_id,omitempty"`
	ConnectionID    string             `json:"connection_id,omitempty"`
	UpstreamModel   string             `json:"upstream_model,omitempty"`
	Phase           ActiveTrafficPhase `json:"phase"`
	RuntimeRevision int64              `json:"runtime_revision"`
	StartedAt       time.Time          `json:"started_at"`
	ConsumerKey     string             `json:"consumer_key,omitempty"`
	APIKeySecretID  string             `json:"api_key_secret_id,omitempty"`
	Protocol        domain.Protocol    `json:"protocol,omitempty"`
	Stream          bool               `json:"stream"`
	ClientIP        string             `json:"client_ip,omitempty"`
	DurationMS      int64              `json:"duration_ms"`
	TTFTMS          int64              `json:"ttft_ms"`
}

// UnfinishedRequests includes the entire client lifetime; ActiveTraffic is
// intentionally still the upstream execution view used by capacity pages.
func (s *Scheduler) UnfinishedRequests() []ActiveTrafficRecord {
	out := []ActiveTrafficRecord{}
	if s == nil || s.led == nil {
		return out
	}
	for _, rec := range s.led.LiveRequests() {
		out = append(out, ActiveTrafficRecord{
			RequestID: rec.ID, LogicalModelID: rec.LogicalModelID,
			ProviderID: rec.ProviderID, ConnectionID: rec.ConnectionID, UpstreamModel: rec.UpstreamModel,
			Phase: ActiveTrafficPhase(rec.Status), RuntimeRevision: rec.RuntimeRevision, StartedAt: rec.Ts,
			ConsumerKey: rec.ConsumerKey, APIKeySecretID: rec.APIKeySecretID, Protocol: rec.Protocol,
			Stream: rec.Stream, ClientIP: rec.ClientIP, DurationMS: rec.DurationMS, TTFTMS: rec.TTFTMS,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RequestID < out[j].RequestID })
	return out
}

func (s *Scheduler) setActiveTraffic(d *decision, phase ActiveTrafficPhase, target *domain.CompiledTarget) {
	if s == nil || d == nil {
		return
	}
	record := ActiveTrafficRecord{
		RequestID: d.rec.ID, LogicalModelID: d.aliasName, Phase: phase,
		RuntimeRevision: d.rec.RuntimeRevision, StartedAt: d.rec.Ts,
	}
	if target != nil {
		record.ProviderID = target.ProviderID
		record.ConnectionID = target.ConnectionID
		record.UpstreamModel = target.Model
	}
	s.activeMu.Lock()
	if s.active == nil {
		s.active = map[string]ActiveTrafficRecord{}
	}
	s.active[record.RequestID] = record
	s.activeMu.Unlock()
	if s.led != nil {
		s.led.UpdateRequest(record.RequestID, func(rec *domain.RequestRecord) {
			rec.Status = domain.RequestStatus(phase)
			rec.ProviderID, rec.ConnectionID, rec.UpstreamModel = record.ProviderID, record.ConnectionID, record.UpstreamModel
			rec.DurationMS = time.Since(rec.Ts).Milliseconds()
		})
	}
}

func (s *Scheduler) clearActiveTraffic(requestID string) {
	if s == nil || requestID == "" {
		return
	}
	s.activeMu.Lock()
	delete(s.active, requestID)
	s.activeMu.Unlock()
}

// ActiveTraffic returns an immutable snapshot sorted by request ID for stable
// management responses and tests.
func (s *Scheduler) ActiveTraffic() []ActiveTrafficRecord {
	if s == nil {
		return []ActiveTrafficRecord{}
	}
	s.activeMu.Lock()
	out := make([]ActiveTrafficRecord, 0, len(s.active))
	for _, record := range s.active {
		out = append(out, record)
	}
	s.activeMu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].RequestID < out[j].RequestID })
	return out
}

func executionTrafficPhase(ex *execution) ActiveTrafficPhase {
	if ex != nil && ex.seq > 0 {
		return TrafficRecovering
	}
	return TrafficActive
}
