package management

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"ferry/internal/config"
	"ferry/internal/domain"
	"ferry/internal/ledger"
)

// handleState reports live resource state for every target (PRD §16.2).
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	snap := s.deps.Scheduler.Snapshot()
	states := s.deps.Manager.States(now)
	type targetView struct {
		ID                   string              `json:"id"`
		Model                string              `json:"model"`
		ClientModel          string              `json:"client_model"`
		EndpointID           string              `json:"endpoint_id"`
		Protocol             domain.Protocol     `json:"protocol"`
		Plan                 string              `json:"plan"`
		PlanKind             domain.PlanKind     `json:"plan_kind,omitempty"`
		Multiplier           float64             `json:"multiplier"` // current quota multiplier (F-P0-14)
		PoolIDs              []string            `json:"pools"`
		UpstreamCapabilities domain.Capabilities `json:"upstream_capabilities"`
		Capabilities         domain.Capabilities `json:"capabilities"`
		Live                 bool                `json:"live"` // has an egress caller (credential resolved)
		State                domain.TargetState  `json:"state"`
	}
	out := make([]targetView, 0, len(snap.TargetOrder))
	for _, id := range snap.TargetOrder {
		t := snap.Targets[id]
		if t == nil {
			continue
		}
		tv := targetView{
			ID: string(t.ID), Model: t.Model, ClientModel: t.ClientModel(),
			EndpointID: t.Endpoint.ID, Protocol: t.Endpoint.Protocol,
			Plan: func() string {
				if t.Plan != nil {
					return t.Plan.ID
				}
				return ""
			}(),
			PoolIDs: t.PoolIDs, UpstreamCapabilities: t.Capabilities,
			Capabilities: domain.EffectiveCapabilities(t.Capabilities),
			Live:         s.deps.Scheduler.HasCaller(id),
			State:        states[id],
		}
		if t.Plan != nil {
			tv.PlanKind = t.Plan.Kind
			tv.Multiplier = t.Plan.CurrentMultiplier(now)
		} else {
			tv.Multiplier = 1
		}
		out = append(out, tv)
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{
		"version": snap.Version, "created_at": snap.CreatedAt, "hash": snap.Hash,
		"now": now, "targets": out,
	})
}

// handleDashboard aggregates ledger usage over a window (PRD §16.3) plus the
// breakdowns the console pages need: compat mix, typed failures, per-consumer
// spend against budgets, and per policy version traffic.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	to := time.Now()
	from := to.Add(-24 * time.Hour)
	if v := r.URL.Query().Get("hours"); v != "" {
		if h, err := strconv.Atoi(v); err == nil && h > 0 {
			from = to.Add(-time.Duration(h) * time.Hour)
		}
	}
	sum, err := s.deps.Ledger.Summarize(from, to)
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	byTarget, err := s.deps.Ledger.ByTarget(from, to)
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp := map[string]any{
		"from": from, "to": to, "summary": sum, "by_target": byTarget,
	}
	// Extended blocks degrade independently — a partial dashboard beats a 500.
	if byConsumer, err := s.deps.Ledger.ByConsumer(from, to); err == nil {
		resp["by_consumer"] = byConsumer
	}
	if pv, err := s.deps.Ledger.ByPolicyVersion(from, to); err == nil {
		resp["policy_versions"] = pv
	}
	if cb, err := s.deps.Ledger.CompatBreakdown(from, to); err == nil {
		resp["compat"] = cb
	}
	if fb, err := s.deps.Ledger.FailureBreakdown(from, to); err == nil {
		resp["failures"] = fb
	}
	writeMgmtJSON(w, http.StatusOK, resp)
}

// handleRequests lists the request log with filters (PRD §16.4).
func (s *Server) handleRequests(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := domain.LogFilter{Limit: 100, Offset: 0}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			f.Limit = n
		}
	}
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			f.Offset = n
		}
	}
	str := func(k string) string { return q.Get(k) }
	f.ConsumerKey = str("consumer_key")
	f.Alias = str("alias")
	f.Objective = str("objective")
	f.Target = str("target")
	f.Model = str("model")
	f.Status = domain.RequestStatus(str("status"))
	f.FailureType = domain.FailureType(str("failure_type"))
	f.PolicyName = str("policy_name")
	if v := str("policy_version"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			f.PolicyVersion = n
		}
	}
	parseTime := func(v string) (time.Time, bool) {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t, true
		}
		if ms, err := strconv.ParseInt(v, 10, 64); err == nil {
			return time.UnixMilli(ms), true // epoch milliseconds convenience
		}
		return time.Time{}, false
	}
	if v := str("from"); v != "" {
		t, ok := parseTime(v)
		if !ok {
			writeMgmtErr(w, http.StatusBadRequest,
				"invalid from: use RFC3339 or epoch millis")
			return
		}
		f.From = t
	}
	if v := str("to"); v != "" {
		t, ok := parseTime(v)
		if !ok {
			writeMgmtErr(w, http.StatusBadRequest,
				"invalid to: use RFC3339 or epoch millis")
			return
		}
		f.To = t
	}
	recs, total, err := s.deps.Ledger.QueryRequests(f)
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{"total": total, "requests": recs})
}

// handleRequestDetail returns one request with its record, attempts, and
// decision trace — everything the detail page renders in one call.
func (s *Server) handleRequestDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec, err := s.deps.Ledger.QueryRequest(id)
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if rec == nil {
		writeMgmtErr(w, http.StatusNotFound, "unknown request "+id)
		return
	}
	attempts, err := s.deps.Ledger.QueryAttempts(id)
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	trace, err := s.deps.Ledger.QueryTrace(id)
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{
		"request_id": id, "request": rec, "attempts": attempts, "trace": trace,
	})
}

// handleConsumers lists configured consumer keys with window usage and
// monthly budget progress (PRD §16.7). Key values are never included.
func (s *Server) handleConsumers(w http.ResponseWriter, r *http.Request) {
	to := time.Now()
	from := to.Add(-24 * time.Hour)
	if v := r.URL.Query().Get("hours"); v != "" {
		if h, err := strconv.Atoi(v); err == nil && h > 0 {
			from = to.Add(-time.Duration(h) * time.Hour)
		}
	}
	usage, err := s.deps.Ledger.ByConsumer(from, to)
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	type consumerView struct {
		Name             string               `json:"name"`
		Enabled          bool                 `json:"enabled"`
		DefaultAlias     string               `json:"default_alias,omitempty"`
		DefaultObjective string               `json:"default_objective,omitempty"`
		MonthlyBudgetUSD float64              `json:"monthly_budget_usd,omitempty"`
		KeyPreview       string               `json:"key_preview"`
		Window           *ledger.UsageSummary `json:"window_usage,omitempty"`
	}
	cfg := s.currentConfig()
	previews := map[string]string{}
	if s.deps.APIKeys != nil {
		identities, secrets, err := s.deps.APIKeys.APIKeys()
		if err != nil {
			writeMgmtErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, identity := range identities {
			for _, secret := range secrets[identity.ID] {
				if secret.State == domain.APIKeySecretActive {
					previews[identity.Name] = secret.Preview
					break
				}
			}
		}
	}
	out := make([]consumerView, 0, len(cfg.ConsumerKeys))
	for _, ck := range cfg.ConsumerKeys {
		preview := previews[ck.Name]
		if preview == "" {
			preview = maskConsumerKey(ck.Key)
		}
		cv := consumerView{
			Name: ck.Name, Enabled: ck.Enabled == nil || *ck.Enabled,
			DefaultAlias: ck.DefaultAlias, DefaultObjective: ck.DefaultObjective,
			MonthlyBudgetUSD: ck.MonthlyBudgetUSD,
			KeyPreview:       preview,
		}
		if u := usage[ck.Name]; u != nil {
			cv.Window = u
		}
		out = append(out, cv)
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{
		"from": from, "to": to, "consumers": out,
	})
}

// handlePolicyVersions reports per policy+version traffic over a window so the
// policy page can show what each published version served (PRD §16.5).
func (s *Server) handlePolicyVersions(w http.ResponseWriter, r *http.Request) {
	to := time.Now()
	from := to.Add(-24 * time.Hour)
	if v := r.URL.Query().Get("hours"); v != "" {
		if h, err := strconv.Atoi(v); err == nil && h > 0 {
			from = to.Add(-time.Duration(h) * time.Hour)
		}
	}
	versions, err := s.deps.Ledger.ByPolicyVersion(from, to)
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	cfg := s.currentConfig()
	livePolicies := append([]domain.RoutingPolicySpec(nil), cfg.RoutingPolicies...)
	if len(livePolicies) == 0 {
		if candidate, cloneErr := cloneConfig(cfg); cloneErr == nil {
			if changed, _ := config.MigrateLegacyRoutingPolicies(candidate); changed {
				livePolicies = candidate.RoutingPolicies
			}
		}
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{
		"from": from, "to": to,
		"live_policies": livePolicies,
		"versions":      versions,
	})
}

// handleTargetUsage returns per-target usage rollups.
func (s *Server) handleTargetUsage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	to := time.Now()
	from := to.Add(-24 * time.Hour)
	rows, err := s.deps.Ledger.ByTarget(from, to)
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, row := range rows {
		if row.Target == id {
			writeMgmtJSON(w, http.StatusOK, map[string]any{"from": from, "to": to, "usage": row})
			return
		}
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{
		"from": from, "to": to,
		"usage": map[string]any{"target": id, "requests": 0},
	})
}

// handleConfig returns the active config with secret values redacted.
func (s *Server) handleConfig(w http.ResponseWriter, _ *http.Request) {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	version, err := s.controlVersionLocked()
	if err != nil {
		writeMgmtErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	cfg := s.deps.Config
	redacted := redactedConfig(cfg)
	writeControlVersionHeaders(w, version)
	writeMgmtJSON(w, http.StatusOK, map[string]any{
		"control_version":  version,
		"consumer_keys":    redacted.ConsumerKeys,
		"aliases":          cfg.Aliases,
		"objectives":       cfg.Objectives,
		"policies":         policyNames(cfg),
		"routing_policies": cfg.RoutingPolicies,
	})
}

// handleConfigRaw serves structured JSON for the visual editors. Editable
// YAML has been retired; consumer bearer values are removed from this broad
// configuration view and require the explicit per-consumer reveal endpoint.
func (s *Server) handleConfigRaw(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("format") != "json" {
		writeMgmtErr(w, http.StatusGone, "YAML management has been retired; use structured JSON")
		return
	}
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	version, err := s.controlVersionLocked()
	if err != nil {
		writeMgmtErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	redacted := redactedConfig(s.deps.Config)
	writeControlVersionHeaders(w, version)
	if r.URL.Query().Get("versioned") == "1" {
		writeMgmtJSON(w, http.StatusOK, map[string]any{
			"control_version": version,
			"config":          redacted,
		})
		return
	}
	// Keep the historical body shape for API consumers that only need the
	// document. New editors use versioned=1 (or the response headers) so the
	// config and its CAS token are captured atomically.
	writeMgmtJSON(w, http.StatusOK, redacted)
}

func writeControlVersionHeaders(w http.ResponseWriter, version int64) {
	w.Header().Set("X-SmartAPI-Control-Version", strconv.FormatInt(version, 10))
	w.Header().Set("X-Ferry-Control-Version", strconv.FormatInt(version, 10))
	w.Header().Set("ETag", `"control-`+strconv.FormatInt(version, 10)+`"`)
}

func redactedConfig(f *config.File) *config.File {
	if f == nil {
		return &config.File{}
	}
	b, _ := json.Marshal(f)
	var out config.File
	_ = json.Unmarshal(b, &out)
	for i := range out.ConsumerKeys {
		out.ConsumerKeys[i].Key = ""
	}
	return &out
}

func policyNames(f *config.File) []string {
	names := make([]string, 0, len(f.Policies))
	for name := range f.Policies {
		names = append(names, name)
	}
	return names
}

// decodeBody reads a JSON request body into v.
func decodeMgmtBody(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 8<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
