package management

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"ferry/internal/config"
	"ferry/internal/controlstore"
	"ferry/internal/domain"
)

type providerCapabilityDivergence struct {
	ProviderID    string
	ModelID       string
	ConnectionIDs []string
}

// configuredProviderCapabilityDivergences compares catalog declarations, not
// runtime probe evidence. Protocol-scoped probes may legitimately differ;
// sibling declarations for the same text model should not drift silently.
func configuredProviderCapabilityDivergences(file *config.File) []providerCapabilityDivergence {
	if file == nil {
		return nil
	}
	divergences := []providerCapabilityDivergence{}
	for providerIndex := range file.Providers {
		provider := &file.Providers[providerIndex]
		if !provider.Enabled {
			continue
		}
		byModel := map[string]map[string]domain.Capabilities{}
		for _, ref := range provider.Connections {
			connection := providerConnection(file, provider.ID, ref.ID)
			if connection == nil || !connection.Enabled {
				continue
			}
			for _, model := range connection.Models {
				textModel := false
				for _, protocol := range model.Protocols {
					if protocol != domain.ProtocolOpenAIImages {
						textModel = true
						break
					}
				}
				if !textModel {
					continue
				}
				if byModel[model.UpstreamModel] == nil {
					byModel[model.UpstreamModel] = map[string]domain.Capabilities{}
				}
				byModel[model.UpstreamModel][connection.ID] = model.Capabilities
			}
		}
		for modelID, byConnection := range byModel {
			if len(byConnection) < 2 {
				continue
			}
			connectionIDs := make([]string, 0, len(byConnection))
			var baseline domain.Capabilities
			baselineSet, divergent := false, false
			for connectionID, capabilities := range byConnection {
				connectionIDs = append(connectionIDs, connectionID)
				if !baselineSet {
					baseline, baselineSet = capabilities, true
				} else if capabilities != baseline {
					divergent = true
				}
			}
			if divergent {
				sort.Strings(connectionIDs)
				divergences = append(divergences, providerCapabilityDivergence{
					ProviderID: provider.ID, ModelID: modelID, ConnectionIDs: connectionIDs,
				})
			}
		}
	}
	sort.Slice(divergences, func(i, j int) bool {
		if divergences[i].ProviderID != divergences[j].ProviderID {
			return divergences[i].ProviderID < divergences[j].ProviderID
		}
		return divergences[i].ModelID < divergences[j].ModelID
	})
	return divergences
}

type allIneligibleRequestAnomaly struct {
	ModelID         string
	Count           int
	LatestRequestID string
	LatestAt        time.Time
}

type providerDataPolicyRejection struct {
	ProviderID      string
	ModelID         string
	Count           int
	LatestRequestID string
	LatestAt        time.Time
}

func providerDataPolicyRejections(records []domain.RequestRecord) []providerDataPolicyRejection {
	byProvider := map[string]providerDataPolicyRejection{}
	for _, record := range records {
		message := strings.ToLower(strings.TrimSpace(record.FailureMsg))
		if !strings.Contains(message, "data policy") ||
			(!strings.Contains(message, "training") && !strings.Contains(message, "privacy")) {
			continue
		}
		providerID := strings.TrimSpace(record.ProviderID)
		if providerID == "" {
			providerID = "unknown"
		}
		modelID := strings.TrimSpace(record.UpstreamModel)
		if modelID == "" {
			modelID = strings.TrimSpace(record.ModelSelected)
		}
		if modelID == "" {
			modelID = strings.TrimSpace(record.ModelRequested)
		}
		rejection := byProvider[providerID]
		rejection.ProviderID, rejection.Count = providerID, rejection.Count+1
		if rejection.ModelID == "" {
			rejection.ModelID = modelID
		}
		if rejection.LatestAt.IsZero() || record.Ts.After(rejection.LatestAt) {
			rejection.LatestAt, rejection.LatestRequestID = record.Ts, record.ID
			rejection.ModelID = modelID
		}
		byProvider[providerID] = rejection
	}
	out := make([]providerDataPolicyRejection, 0, len(byProvider))
	for _, rejection := range byProvider {
		out = append(out, rejection)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProviderID < out[j].ProviderID })
	return out
}

// allIneligibleRequestAnomalies finds real client requests that failed before
// any upstream attempt after the scheduler excluded every candidate. This is
// the request-shaped gap that static route-health alerts cannot observe.
func allIneligibleRequestAnomalies(records []domain.RequestRecord) []allIneligibleRequestAnomaly {
	byModel := map[string]allIneligibleRequestAnomaly{}
	for _, record := range records {
		if record.Attempts != 0 || len(record.Exclusions) == 0 ||
			(record.Status != domain.ReqFailed && record.Outcome != domain.OutcomeFailed) {
			continue
		}
		modelID := strings.TrimSpace(record.LogicalModelID)
		if modelID == "" {
			modelID = strings.TrimSpace(record.Alias)
		}
		if modelID == "" {
			modelID = strings.TrimSpace(record.ModelRequested)
		}
		if modelID == "" {
			modelID = "unknown"
		}
		anomaly := byModel[modelID]
		anomaly.ModelID, anomaly.Count = modelID, anomaly.Count+1
		if anomaly.LatestAt.IsZero() || record.Ts.After(anomaly.LatestAt) {
			anomaly.LatestAt, anomaly.LatestRequestID = record.Ts, record.ID
		}
		byModel[modelID] = anomaly
	}
	out := make([]allIneligibleRequestAnomaly, 0, len(byModel))
	for _, anomaly := range byModel {
		out = append(out, anomaly)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModelID < out[j].ModelID })
	return out
}

func (s *Server) observeAlert(spec controlstore.ManagementAlertSpec, active bool, threshold time.Duration, now time.Time) {
	if s.deps.Control == nil {
		return
	}
	if err := s.deps.Control.ObserveManagementAlert(spec, active, threshold, now); err != nil {
		log.Printf("management alert %s: %v", spec.Key, err)
	}
}

func (s *Server) evaluateAlerts(now time.Time) {
	if s.deps.Control == nil || s.deps.Scheduler == nil || s.deps.Ledger == nil {
		return
	}
	seen := map[string]bool{}
	observe := func(spec controlstore.ManagementAlertSpec, active bool, threshold time.Duration) {
		spec.Key = "eval:" + spec.Key
		seen[spec.Key] = true
		s.observeAlert(spec, active, threshold, now)
	}
	snapshotUnavailable := s.deps.Scheduler.Snapshot() == nil
	observe(controlstore.ManagementAlertSpec{Key: "runtime-unavailable", Type: "runtime_unavailable", ResourceKind: "runtime", ResourceID: "local", Title: "Runtime API is unavailable", Severity: "critical"}, snapshotUnavailable, time.Minute)
	for _, divergence := range configuredProviderCapabilityDivergences(s.currentConfig()) {
		observe(controlstore.ManagementAlertSpec{
			Key:  "capability-divergence:" + divergence.ProviderID + ":" + divergence.ModelID,
			Type: "capability_divergence", ResourceKind: "provider", ResourceID: divergence.ProviderID,
			Title:  "Sibling connection capabilities diverged: " + divergence.ModelID,
			Detail: "Provider " + divergence.ProviderID + " declares different capabilities across " + strings.Join(divergence.ConnectionIDs, ", ") + ".",
		}, true, 0)
	}
	if !snapshotUnavailable {
		from := now.Add(-5 * time.Minute)
		routeTraffic, _ := s.deps.Ledger.TrafficByRoute(from, now)
		modelTraffic, _ := s.deps.Ledger.TrafficByLogicalModel(from, now)
		models := s.v2ModelViews(routeTraffic)
		enrichV2Models(models, modelTraffic)
		for _, model := range models {
			inUse := model.Traffic != nil && model.Traffic.Requests > 0
			observe(controlstore.ManagementAlertSpec{Key: "model-unavailable:" + model.ID, Type: "model_unavailable", ResourceKind: "model", ResourceID: model.ID,
				Title: "Model unavailable: " + model.ID, Detail: model.RuntimeSentence, Severity: "critical"}, inUse && model.State == "unavailable", 2*time.Minute)
			noRoute := len(model.DefaultRoute) > 0
			for _, route := range model.DefaultRoute {
				if route.EligibleConnections > 0 {
					noRoute = false
					break
				}
			}
			observe(controlstore.ManagementAlertSpec{Key: "routes-exhausted:" + model.ID, Type: "routes_exhausted", ResourceKind: "model", ResourceID: model.ID,
				Title: "All provider routes are exhausted: " + model.ID, Severity: "critical"}, model.Enabled && noRoute, 2*time.Minute)
		}
		providers := s.v2ProviderViews(nil)
		for _, provider := range providers {
			observe(controlstore.ManagementAlertSpec{Key: "provider-exhausted:" + provider.ID, Type: "provider_exhausted", ResourceKind: "provider", ResourceID: provider.ID,
				Title: "Provider exhausted: " + provider.Name, Severity: "critical"}, provider.Enabled && provider.State == "exhausted", 2*time.Minute)
			for _, connection := range provider.Connections {
				lowQuota := false
				for _, window := range connection.Runtime.QuotaWindows {
					if window.RemainingFraction != nil && *window.RemainingFraction < .10 {
						lowQuota = true
						break
					}
				}
				cooling := connection.Runtime.CoolingUntil.After(now)
				observe(controlstore.ManagementAlertSpec{Key: "quota:" + connection.ID, Type: "quota_or_cooling", ResourceKind: "connection", ResourceID: connection.ID,
					Title: "Connection capacity is low: " + connection.ID, Detail: "Quota is below 10% or the connection entered cooling."}, lowQuota || cooling, 0)
			}
		}
		recentFailures, _, err := s.deps.Ledger.QueryRequests(domain.LogFilter{
			From: now.Add(-5 * time.Minute), To: now, Outcome: domain.OutcomeFailed, Limit: 1000,
		})
		if err == nil {
			for _, anomaly := range allIneligibleRequestAnomalies(recentFailures) {
				observe(controlstore.ManagementAlertSpec{
					Key:  "all-ineligible:" + anomaly.ModelID,
					Type: "all_connections_ineligible", ResourceKind: "model", ResourceID: anomaly.ModelID,
					Title:    "All provider connections were ineligible: " + anomaly.ModelID,
					Detail:   fmt.Sprintf("%d request(s) failed before any upstream attempt in five minutes; latest request %s.", anomaly.Count, anomaly.LatestRequestID),
					Severity: "critical",
				}, true, 0)
			}
			for _, rejection := range providerDataPolicyRejections(recentFailures) {
				modelDetail := ""
				if rejection.ModelID != "" {
					modelDetail = " for " + rejection.ModelID
				}
				observe(controlstore.ManagementAlertSpec{
					Key:  "provider-data-policy:" + rejection.ProviderID,
					Type: "provider_data_policy_rejected", ResourceKind: "provider", ResourceID: rejection.ProviderID,
					Title: "Provider data policy rejected requests: " + rejection.ProviderID,
					Detail: fmt.Sprintf("%d request(s)%s were rejected by the provider data/training policy in five minutes; latest request %s. Ferry does not change provider privacy settings.",
						rejection.Count, modelDetail, rejection.LatestRequestID),
					Severity: "critical",
				}, true, 0)
			}
		}
	}
	outcomes, _ := s.deps.Ledger.OutcomeSummary(now.Add(-5*time.Minute), now)
	total := outcomes.DirectSuccess + outcomes.RecoveredSuccess + outcomes.Failed
	failureRate := total > 0 && outcomes.Failed >= 5 && float64(outcomes.Failed)/float64(total) >= .05
	observe(controlstore.ManagementAlertSpec{Key: "client-failure-rate", Type: "client_failure_rate", ResourceKind: "runtime", ResourceID: "local",
		Title: "Client failure rate is elevated", Detail: fmt.Sprintf("%d failures across %d requests in five minutes", outcomes.Failed, total), Severity: "critical"}, failureRate, 0)
	recoveryRate := total > 0 && float64(outcomes.RecoveredSuccess)/float64(total) >= .20
	observe(controlstore.ManagementAlertSpec{Key: "recovery-rate", Type: "recovery_rate", ResourceKind: "runtime", ResourceID: "local",
		Title: "Recovery traffic is elevated", Detail: fmt.Sprintf("%d of %d requests required recovery", outcomes.RecoveredSuccess, total)}, recoveryRate, 0)
	current, _ := s.deps.Ledger.ByConsumer(now.Add(-5*time.Minute), now)
	previous, _ := s.deps.Ledger.ByConsumer(now.Add(-10*time.Minute), now.Add(-5*time.Minute))
	for key, usage := range current {
		prior := int64(0)
		if previous[key] != nil {
			prior = previous[key].Requests
		}
		spike := usage.Requests >= 100 && usage.Requests >= 3*max(prior, 1)
		observe(controlstore.ManagementAlertSpec{Key: "api-key-spike:" + key, Type: "api_key_spike", ResourceKind: "api_key", ResourceID: key,
			Title: "API key request volume spiked: " + key, Detail: "Fixed rule: at least 100 requests and 3× the preceding five-minute window."}, spike, 0)
	}
	if keys, err := s.deps.Control.ManagementAlertSignalKeys("eval:"); err == nil {
		for _, key := range keys {
			if !seen[key] {
				s.observeAlert(controlstore.ManagementAlertSpec{Key: key, Type: "recovered", Title: "Recovered"}, false, 0, now)
			}
		}
	}
	_ = s.deps.Control.CleanupManagementAlerts(now.Add(-30 * 24 * time.Hour))
}

func (s *Server) StartAlertMonitor(ctx context.Context) {
	s.evaluateAlerts(time.Now())
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case now := <-ticker.C:
				s.evaluateAlerts(now)
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (s *Server) handleV2Alerts(w http.ResponseWriter, _ *http.Request) {
	s.evaluateAlerts(time.Now())
	alerts, err := s.deps.Control.ManagementAlerts(200)
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	unread := 0
	for _, alert := range alerts {
		if alert.Unread {
			unread++
		}
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{"alerts": alerts, "unread": unread})
}

func (s *Server) handleV2AlertAcknowledge(w http.ResponseWriter, r *http.Request) {
	if err := s.deps.Control.AcknowledgeManagementAlert(r.PathValue("id"), time.Now()); err != nil {
		writeMgmtErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{"acknowledged": r.PathValue("id")})
}

func (s *Server) emitFailureAlert(kind, resourceKind, resourceID, title, detail string) {
	s.observeAlert(controlstore.ManagementAlertSpec{Key: "event:" + kind + ":" + resourceID + ":" + fmt.Sprint(time.Now().Unix()/60),
		Type: kind, ResourceKind: resourceKind, ResourceID: resourceID, Title: title, Detail: detail, Severity: "critical"}, true, 0, time.Now())
}

func connectionTestAlertType(failure *domain.Failure) (string, string) {
	if failure == nil {
		return "connection_test_failed", "Connection test failed"
	}
	switch failure.Type {
	case domain.FailAuth:
		return "connection_authentication_failed", "Connection authentication failed"
	case domain.FailRateLimit, domain.FailQuotaExhausted:
		return "connection_capacity_limited", "Connection capacity is limited"
	case domain.FailConnect, domain.FailTTFTTimeout, domain.FailStreamBroken:
		return "connection_unreachable", "Connection is temporarily unreachable"
	default:
		return "connection_test_failed", "Connection test failed"
	}
}

func (s *Server) observeConnectionTestAlert(connectionID string, failure *domain.Failure, now time.Time) {
	kind, title := connectionTestAlertType(failure)
	detail := ""
	if failure != nil {
		detail = failure.Message
	}
	s.recoverConnectionTestAlertsExcept(connectionID, kind, now)
	s.observeAlert(controlstore.ManagementAlertSpec{
		Key: "event:connection-test:" + connectionID + ":" + kind, Type: kind,
		ResourceKind: "connection", ResourceID: connectionID, Title: title + ": " + connectionID,
		Detail: detail, Severity: "critical",
	}, true, 0, now)
}

func (s *Server) recoverConnectionTestAlerts(connectionID string, now time.Time) {
	s.recoverConnectionTestAlertsExcept(connectionID, "", now)
}

func (s *Server) recoverConnectionTestAlertsExcept(connectionID, keep string, now time.Time) {
	if s.deps.Control == nil {
		return
	}
	for _, prefix := range []string{"event:connection-test:" + connectionID + ":", "event:credential_validation_failed:" + connectionID + ":"} {
		keys, err := s.deps.Control.ManagementAlertSignalKeys(prefix)
		if err != nil {
			continue
		}
		for _, key := range keys {
			if keep != "" && key == "event:connection-test:"+connectionID+":"+keep {
				continue
			}
			s.observeAlert(controlstore.ManagementAlertSpec{Key: key, Type: "recovered", Title: "Recovered"}, false, 0, now)
		}
	}
}
