package management

import (
	"ferry/internal/domain"
	"ferry/internal/runtime"
	"net/http"
	"sort"
	"time"
)

func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	if s.deps.Manager == nil || s.deps.Scheduler == nil {
		writeMgmtErr(w, http.StatusServiceUnavailable, "runtime is not configured")
		return
	}
	snapshot := s.deps.Scheduler.Snapshot()
	connections := make([]map[string]any, 0)
	waiting, running := 0, int64(0)
	gates := make(map[string]runtime.QueueState)
	for _, gate := range s.deps.Manager.QueueStates() {
		gates[gate.ConnectionID] = gate
	}
	var global *domain.ConcurrencyPolicy
	ids := []string{}
	if snapshot != nil {
		global = snapshot.Concurrency
		for id := range snapshot.Connections {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		connection := snapshot.Connections[id]
		if connection == nil {
			continue
		}
		provider := snapshot.Providers[connection.ProviderID]
		source := "global"
		configured := global
		if provider != nil && provider.Concurrency != nil {
			source = "provider"
			configured = provider.Concurrency
		}
		if connection.Concurrency != nil {
			source = "connection"
			configured = connection.Concurrency
		}
		gate, exists := gates[id]
		if !exists {
			p := runtime.EffectiveConcurrency(configured)
			gate = runtime.QueueState{ConnectionID: id, MaxConcurrent: p.MaxConcurrent, MaxQueue: p.MaxQueue,
				MaxWaitSeconds: p.MaxWaitSeconds, QueueWhenFull: *p.QueueWhenFull, QueueOnThrottle: *p.QueueOnThrottle}
		}
		connections = append(connections, map[string]any{"connection_id": gate.ConnectionID, "provider_id": connection.ProviderID,
			"identity": connection.Identity, "enabled": connection.Enabled, "policy_source": source, "concurrency": connection.Concurrency, "runtime": gate})
		waiting += gate.Waiting
		running += gate.InFlight
	}
	policy := runtime.EffectiveConcurrency(global)
	requests := s.deps.Scheduler.UnfinishedRequests()
	writeMgmtJSON(w, http.StatusOK, map[string]any{"generated_at": time.Now().UTC(), "waiting": waiting, "running": running,
		"open_requests": len(requests),
		"connections":   connections, "requests": requests, "defaults": policy})
}
