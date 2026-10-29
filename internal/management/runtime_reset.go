package management

import (
	"net/http"
	"strings"

	"ferry/internal/domain"
)

// Empty filters select the entire runtime. Filters always intersect; typos
// must fail rather than accidentally widen a reset to all connections.
func (s *Server) handleRuntimeClearCooldown(w http.ResponseWriter, r *http.Request) {
	if s.deps.Manager == nil || s.deps.Scheduler == nil || s.deps.Scheduler.Snapshot() == nil {
		writeMgmtErr(w, http.StatusServiceUnavailable, "runtime state manager is not configured")
		return
	}
	var filter struct {
		ProviderID    string `json:"provider_id"`
		ConnectionID  string `json:"connection_id"`
		UpstreamModel string `json:"upstream_model"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := decodeMgmtBody(r, &filter); err != nil {
			writeMgmtErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	logicalID := ""
	if strings.HasPrefix(r.URL.Path, "/api/models/") {
		logicalID = r.PathValue("id")
	} else if id := r.PathValue("id"); id != "" {
		if filter.ProviderID != "" && filter.ProviderID != id {
			writeMgmtErr(w, http.StatusBadRequest, "provider filter conflicts with path")
			return
		}
		filter.ProviderID = id
	}
	if id := r.PathValue("connection_id"); id != "" {
		if filter.ConnectionID != "" && filter.ConnectionID != id {
			writeMgmtErr(w, http.StatusBadRequest, "connection filter conflicts with path")
			return
		}
		filter.ConnectionID = id
	}
	snap := s.deps.Scheduler.Snapshot()
	models := map[string]bool{}
	if logicalID != "" {
		model := snap.LogicalModels[logicalID]
		if model == nil {
			writeMgmtErr(w, http.StatusNotFound, "logical model not found")
			return
		}
		add := func(route domain.RouteSpec) {
			for _, step := range route.Steps {
				models[domain.ProviderModelKey(step.ProviderID, step.UpstreamModel)] = true
			}
		}
		add(model.DefaultRoute)
		for _, override := range model.Overrides {
			if override.Route != nil {
				add(*override.Route)
			}
		}
	}
	ids := []domain.ResourceTargetID{}
	connections := map[string]bool{}
	for id, target := range snap.Targets {
		if target == nil || (filter.ProviderID != "" && target.ProviderID != filter.ProviderID) ||
			(filter.ConnectionID != "" && target.ConnectionID != filter.ConnectionID) ||
			(filter.UpstreamModel != "" && target.Model != filter.UpstreamModel) ||
			(logicalID != "" && !models[domain.ProviderModelKey(target.ProviderID, target.Model)]) {
			continue
		}
		ids = append(ids, id)
		connections[target.ConnectionID] = true
	}
	global := filter.ProviderID == "" && filter.ConnectionID == "" && filter.UpstreamModel == "" && logicalID == ""
	if !global && len(ids) == 0 {
		writeMgmtErr(w, http.StatusNotFound, "no runtime models match the reset scope")
		return
	}
	count := len(ids)
	if global {
		ids = nil
	}
	if err := s.deps.Manager.ResetCooldowns(ids); err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, "runtime reset applied in memory but persistence failed: "+err.Error())
		return
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{
		"provider_id": filter.ProviderID, "connection_id": filter.ConnectionID,
		"upstream_model": filter.UpstreamModel, "model_id": logicalID,
		"cleared_models": count, "cleared_connections": len(connections), "persisted": true,
	})
}
