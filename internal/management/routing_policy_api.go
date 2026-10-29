package management

import (
	"fmt"
	"net/http"

	"ferry/internal/config"
	"ferry/internal/domain"
)

type routingPolicyCreateInput struct {
	ID              string                    `json:"id"`
	Name            string                    `json:"name"`
	Description     string                    `json:"description"`
	Match           domain.RoutingPolicyMatch `json:"match"`
	Route           domain.RoutingPolicyRoute `json:"route"`
	ExpectedVersion *int64                    `json:"expected_version,omitempty"`
}

type routingPolicyUpdateInput struct {
	Name             *string                    `json:"name,omitempty"`
	Description      *string                    `json:"description,omitempty"`
	Match            *domain.RoutingPolicyMatch `json:"match,omitempty"`
	Route            *domain.RoutingPolicyRoute `json:"route,omitempty"`
	ExpectedRevision *int64                     `json:"expected_revision,omitempty"`
	ExpectedVersion  *int64                     `json:"expected_version,omitempty"`
}

func routingPolicyIndex(f *config.File, id string) int {
	for i := range f.RoutingPolicies {
		if f.RoutingPolicies[i].ID == id {
			return i
		}
	}
	return -1
}

func (s *Server) orderedPolicyConfigLocked() (*config.File, error) {
	next, err := cloneConfig(s.deps.Config)
	if err != nil {
		return nil, err
	}
	if _, err := config.MigrateLegacyRoutingPolicies(next); err != nil {
		return nil, err
	}
	return next, nil
}

func (s *Server) requireExpectedControlVersionLocked(expected *int64) error {
	if expected == nil {
		return nil
	}
	current, err := s.controlVersionLocked()
	if err != nil {
		return err
	}
	if *expected != current {
		return fmt.Errorf("stale control version: expected %d, current %d", *expected, current)
	}
	return nil
}

func (s *Server) writeRoutingPolicyMutation(w http.ResponseWriter, status int, spec domain.RoutingPolicySpec) {
	response := map[string]any{"saved": true, "policy": spec}
	if snap := s.deps.Scheduler.Snapshot(); snap != nil {
		response["runtime_version"] = snap.Version
		response["policy_index_hash"] = snap.PolicyIndexHash
	}
	if version, err := s.controlVersionLocked(); err == nil {
		response["control_version"] = version
		writeControlVersionHeaders(w, version)
	}
	writeMgmtJSON(w, status, response)
}

func (s *Server) handleRoutingPoliciesList(w http.ResponseWriter, _ *http.Request) {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	policies := append([]domain.RoutingPolicySpec(nil), s.deps.Config.RoutingPolicies...)
	// Read-only compatibility for a process started by an older embedder. The
	// real application migrates its active SQLite row during controlstore.Open.
	if len(policies) == 0 {
		if candidate, err := cloneConfig(s.deps.Config); err == nil {
			if changed, _ := config.MigrateLegacyRoutingPolicies(candidate); changed {
				policies = candidate.RoutingPolicies
			}
		}
	}
	response := map[string]any{"policies": policies}
	if snap := s.deps.Scheduler.Snapshot(); snap != nil {
		response["policy_index_hash"] = snap.PolicyIndexHash
	}
	if version, err := s.controlVersionLocked(); err == nil {
		response["control_version"] = version
		writeControlVersionHeaders(w, version)
	}
	writeMgmtJSON(w, http.StatusOK, response)
}

func (s *Server) handleRoutingPolicyDetail(w http.ResponseWriter, r *http.Request) {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	next, err := s.orderedPolicyConfigLocked()
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	idx := routingPolicyIndex(next, r.PathValue("id"))
	if idx < 0 {
		writeMgmtErr(w, http.StatusNotFound, "unknown policy "+r.PathValue("id"))
		return
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{"policy": next.RoutingPolicies[idx]})
}

func (s *Server) handleRoutingPolicyCreate(w http.ResponseWriter, r *http.Request) {
	var in routingPolicyCreateInput
	if err := decodeMgmtBody(r, &in); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, "decode body: "+err.Error())
		return
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if err := s.requireExpectedControlVersionLocked(in.ExpectedVersion); err != nil {
		writeMgmtErr(w, http.StatusConflict, err.Error())
		return
	}
	next, err := s.orderedPolicyConfigLocked()
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if routingPolicyIndex(next, in.ID) >= 0 {
		writeMgmtErr(w, http.StatusConflict, "policy "+in.ID+" already exists")
		return
	}
	priority := 10
	if n := len(next.RoutingPolicies); n > 0 {
		priority = next.RoutingPolicies[n-1].Priority + 10
	}
	name := in.Name
	if name == "" {
		name = in.ID
	}
	spec := domain.RoutingPolicySpec{
		ID: in.ID, Name: name, Description: in.Description,
		Priority: priority, Enabled: false, Match: in.Match, Route: in.Route,
	}
	next.RoutingPolicies = append(next.RoutingPolicies, spec)
	if err := s.publishConfig(next); err != nil {
		writeMgmtErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.writeRoutingPolicyMutation(w, http.StatusCreated, next.RoutingPolicies[len(next.RoutingPolicies)-1])
}

func (s *Server) handleRoutingPolicyUpdate(w http.ResponseWriter, r *http.Request) {
	var in routingPolicyUpdateInput
	if err := decodeMgmtBody(r, &in); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, "decode body: "+err.Error())
		return
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if err := s.requireExpectedControlVersionLocked(in.ExpectedVersion); err != nil {
		writeMgmtErr(w, http.StatusConflict, err.Error())
		return
	}
	next, err := s.orderedPolicyConfigLocked()
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	idx := routingPolicyIndex(next, r.PathValue("id"))
	if idx < 0 {
		writeMgmtErr(w, http.StatusNotFound, "unknown policy "+r.PathValue("id"))
		return
	}
	spec := &next.RoutingPolicies[idx]
	if in.ExpectedRevision != nil && *in.ExpectedRevision != spec.Revision {
		writeMgmtErr(w, http.StatusConflict, "stale policy revision")
		return
	}
	if in.Name != nil {
		spec.Name = *in.Name
	}
	if in.Description != nil {
		spec.Description = *in.Description
	}
	if in.Match != nil {
		spec.Match = *in.Match
	}
	if in.Route != nil {
		spec.Route = *in.Route
	}
	if err := s.publishConfig(next); err != nil {
		writeMgmtErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.writeRoutingPolicyMutation(w, http.StatusOK, *spec)
}

func (s *Server) handleRoutingPolicyEnabled(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Enabled         bool   `json:"enabled"`
		ExpectedVersion *int64 `json:"expected_version,omitempty"`
	}
	if err := decodeMgmtBody(r, &in); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, "decode body: "+err.Error())
		return
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if err := s.requireExpectedControlVersionLocked(in.ExpectedVersion); err != nil {
		writeMgmtErr(w, http.StatusConflict, err.Error())
		return
	}
	next, err := s.orderedPolicyConfigLocked()
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	idx := routingPolicyIndex(next, r.PathValue("id"))
	if idx < 0 {
		writeMgmtErr(w, http.StatusNotFound, "unknown policy "+r.PathValue("id"))
		return
	}
	next.RoutingPolicies[idx].Enabled = in.Enabled
	if err := s.publishConfig(next); err != nil {
		writeMgmtErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.writeRoutingPolicyMutation(w, http.StatusOK, next.RoutingPolicies[idx])
}

func (s *Server) handleRoutingPoliciesReorder(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs             []string `json:"ids"`
		ExpectedVersion *int64   `json:"expected_version,omitempty"`
	}
	if err := decodeMgmtBody(r, &in); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, "decode body: "+err.Error())
		return
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if err := s.requireExpectedControlVersionLocked(in.ExpectedVersion); err != nil {
		writeMgmtErr(w, http.StatusConflict, err.Error())
		return
	}
	next, err := s.orderedPolicyConfigLocked()
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(in.IDs) != len(next.RoutingPolicies) {
		writeMgmtErr(w, http.StatusBadRequest, "reorder must include every policy exactly once")
		return
	}
	byID := make(map[string]domain.RoutingPolicySpec, len(next.RoutingPolicies))
	for _, spec := range next.RoutingPolicies {
		byID[spec.ID] = spec
	}
	ordered := make([]domain.RoutingPolicySpec, 0, len(in.IDs))
	for i, id := range in.IDs {
		spec, ok := byID[id]
		if !ok {
			writeMgmtErr(w, http.StatusBadRequest, "unknown or duplicate policy "+id)
			return
		}
		delete(byID, id)
		spec.Priority = (i + 1) * 10
		ordered = append(ordered, spec)
	}
	next.RoutingPolicies = ordered
	if err := s.publishConfig(next); err != nil {
		writeMgmtErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	response := map[string]any{"saved": true, "policies": next.RoutingPolicies}
	if snap := s.deps.Scheduler.Snapshot(); snap != nil {
		response["policy_index_hash"] = snap.PolicyIndexHash
	}
	writeMgmtJSON(w, http.StatusOK, response)
}

func (s *Server) handleRoutingPolicyDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.configMu.Lock()
	defer s.configMu.Unlock()
	next, err := s.orderedPolicyConfigLocked()
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	idx := routingPolicyIndex(next, id)
	if idx < 0 {
		writeMgmtErr(w, http.StatusNotFound, "unknown policy "+id)
		return
	}
	if len(next.RoutingPolicies) == 1 {
		writeMgmtErr(w, http.StatusUnprocessableEntity, "cannot delete the final routing policy")
		return
	}
	next.RoutingPolicies = append(next.RoutingPolicies[:idx], next.RoutingPolicies[idx+1:]...)
	if err := s.publishConfig(next); err != nil {
		writeMgmtErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{"deleted": id, "saved": true})
}

func (s *Server) handleRoutingPolicyRevisions(w http.ResponseWriter, r *http.Request) {
	if s.deps.PolicyHistory == nil {
		writeMgmtErr(w, http.StatusServiceUnavailable, "policy history store not configured")
		return
	}
	revisions, err := s.deps.PolicyHistory.PolicyRevisions(r.PathValue("id"))
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{"revisions": revisions})
}

func (s *Server) handleRoutingPolicyRollback(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Revision        int64  `json:"revision"`
		ExpectedVersion *int64 `json:"expected_version,omitempty"`
	}
	if err := decodeMgmtBody(r, &in); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, "decode body: "+err.Error())
		return
	}
	if in.Revision <= 0 {
		writeMgmtErr(w, http.StatusBadRequest, "revision must be positive")
		return
	}
	if s.deps.PolicyHistory == nil {
		writeMgmtErr(w, http.StatusServiceUnavailable, "policy history store not configured")
		return
	}
	revision, err := s.deps.PolicyHistory.PolicyRevision(r.PathValue("id"), in.Revision)
	if err != nil {
		writeMgmtErr(w, http.StatusNotFound, err.Error())
		return
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if err := s.requireExpectedControlVersionLocked(in.ExpectedVersion); err != nil {
		writeMgmtErr(w, http.StatusConflict, err.Error())
		return
	}
	next, err := s.orderedPolicyConfigLocked()
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	idx := routingPolicyIndex(next, r.PathValue("id"))
	if idx < 0 {
		writeMgmtErr(w, http.StatusNotFound, "unknown policy "+r.PathValue("id"))
		return
	}
	next.RoutingPolicies[idx].Match = revision.Match
	next.RoutingPolicies[idx].Route = revision.Route
	next.RoutingPolicies[idx].Revision = revision.Revision
	next.RoutingPolicies[idx].ContentHash = revision.ContentHash
	if err := s.publishConfig(next); err != nil {
		writeMgmtErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{
		"rolled_back": true, "policy": next.RoutingPolicies[idx],
	})
}
