package management

import (
	"testing"
	"time"

	"ferry/internal/domain"
)

func TestConfiguredProviderCapabilityDivergencesUsesSiblingTextDeclarations(t *testing.T) {
	file := nativeV2Config()
	file.Providers[0].Connections = append(file.Providers[0].Connections,
		domain.ConnectionRef{ID: "connection-b"}, domain.ConnectionRef{ID: "images"})
	file.Connections = append(file.Connections,
		domain.Connection{
			ID: "connection-b", ProviderID: "provider-a", Enabled: true,
			Models: []domain.ConnectionModel{{
				UpstreamModel: "upstream-model", Protocols: []domain.Protocol{domain.ProtocolAnthropic},
				Capabilities: domain.Capabilities{Text: true},
			}},
		},
		domain.Connection{
			ID: "images", ProviderID: "provider-a", Enabled: true,
			Models: []domain.ConnectionModel{{
				UpstreamModel: "upstream-model", Protocols: []domain.Protocol{domain.ProtocolOpenAIImages},
				Capabilities: domain.Capabilities{Images: true},
			}},
		},
	)
	got := configuredProviderCapabilityDivergences(file)
	if len(got) != 1 || got[0].ProviderID != "provider-a" || got[0].ModelID != "upstream-model" ||
		len(got[0].ConnectionIDs) != 2 || got[0].ConnectionIDs[0] != "connection-a" || got[0].ConnectionIDs[1] != "connection-b" {
		t.Fatalf("divergences=%+v", got)
	}
	file.Connections[1].Models[0].Capabilities = file.Connections[0].Models[0].Capabilities
	if got := configuredProviderCapabilityDivergences(file); len(got) != 0 {
		t.Fatalf("equal sibling declarations diverged: %+v", got)
	}
}

func TestAllIneligibleRequestAnomaliesRequireZeroAttemptExclusions(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	records := []domain.RequestRecord{
		{ID: "req-new", Ts: now, LogicalModelID: "smart", Status: domain.ReqFailed, Outcome: domain.OutcomeFailed,
			Exclusions: []domain.ExclusionRecord{{Target: "a", Reason: "missing capabilities"}}},
		{ID: "req-old", Ts: now.Add(-time.Minute), Alias: "smart", Status: domain.ReqFailed,
			Exclusions: []domain.ExclusionRecord{{Target: "b", Reason: "protocol compatibility"}}},
		{ID: "attempted", Ts: now, LogicalModelID: "smart", Status: domain.ReqFailed, Attempts: 1,
			Exclusions: []domain.ExclusionRecord{{Target: "c", Reason: "missing capabilities"}}},
		{ID: "no-exclusions", Ts: now, LogicalModelID: "smart", Status: domain.ReqFailed},
		{ID: "success", Ts: now, LogicalModelID: "other", Status: domain.ReqSuccess,
			Exclusions: []domain.ExclusionRecord{{Target: "d", Reason: "missing capabilities"}}},
	}
	got := allIneligibleRequestAnomalies(records)
	if len(got) != 1 || got[0].ModelID != "smart" || got[0].Count != 2 || got[0].LatestRequestID != "req-new" {
		t.Fatalf("anomalies=%+v", got)
	}
}

func TestProviderDataPolicyRejectionsAreExplicitAndProviderScoped(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	got := providerDataPolicyRejections([]domain.RequestRecord{
		{ID: "latest", Ts: now, ProviderID: "openrouter", UpstreamModel: "meta/contributor",
			FailureMsg: "No endpoints found matching your data policy. Enable prompt training in Privacy settings."},
		{ID: "older", Ts: now.Add(-time.Minute), ProviderID: "openrouter", ModelSelected: "other",
			FailureMsg: "Provider DATA POLICY disallows TRAINING."},
		{ID: "unrelated", Ts: now, ProviderID: "openrouter", FailureMsg: "rate limited"},
	})
	if len(got) != 1 || got[0].ProviderID != "openrouter" || got[0].ModelID != "meta/contributor" ||
		got[0].Count != 2 || got[0].LatestRequestID != "latest" {
		t.Fatalf("rejections=%+v", got)
	}
}

func TestEvaluateAlertsReportsAndRecoversCapabilityRoutingAnomalies(t *testing.T) {
	f := newV2APIFixture(t)
	f.server.configMu.Lock()
	next, err := cloneConfig(f.server.deps.Config)
	if err == nil {
		next.Providers[0].Connections = append(next.Providers[0].Connections, domain.ConnectionRef{ID: "connection-b"})
		next.Connections = append(next.Connections, domain.Connection{
			ID: "connection-b", ProviderID: "provider-a", Enabled: true,
			Credential: next.Connections[0].Credential,
			Models: []domain.ConnectionModel{{
				UpstreamModel: "upstream-model", Protocols: []domain.Protocol{domain.ProtocolAnthropic},
				Capabilities: domain.Capabilities{Text: true},
			}},
		})
		err = f.server.publishConfig(next)
	}
	f.server.configMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	f.ledger.BeginRequest(domain.RequestRecord{
		ID: "req-all-ineligible", Ts: now.Add(-time.Minute), Alias: "smart", LogicalModelID: "smart",
		ModelRequested: "smart", Protocol: domain.ProtocolAnthropic,
	})
	f.ledger.FinishRequest("req-all-ineligible", func(record *domain.RequestRecord) {
		record.Status, record.Outcome = domain.ReqFailed, domain.OutcomeFailed
		record.FailureType = domain.FailUnsupportedParam
		record.Exclusions = []domain.ExclusionRecord{
			{Target: "provider-a/connection-a/upstream-model", Reason: "protocol compatibility requires native Anthropic"},
			{Target: "provider-a/connection-b/upstream-model", Reason: "missing capabilities: stream,tools,reasoning"},
		}
	})
	f.ledger.BeginRequest(domain.RequestRecord{
		ID: "req-data-policy", Ts: now.Add(-30 * time.Second), Alias: "smart", ProviderID: "provider-a",
		UpstreamModel: "upstream-model", ModelRequested: "smart",
	})
	f.ledger.FinishRequest("req-data-policy", func(record *domain.RequestRecord) {
		record.Status, record.Outcome = domain.ReqFailed, domain.OutcomeFailed
		record.FailureType, record.Attempts = domain.FailUnsupportedParam, 1
		record.FailureMsg = "No endpoints found matching your data policy; enable prompt training in privacy settings."
	})
	deadline := time.Now().Add(2 * time.Second)
	for {
		stored, queryErr := f.ledger.QueryRequest("req-data-policy")
		if queryErr != nil {
			t.Fatal(queryErr)
		}
		if stored != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("request ledger did not flush")
		}
		time.Sleep(10 * time.Millisecond)
	}

	f.server.evaluateAlerts(now)
	alerts, err := f.control.ManagementAlerts(20)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, alert := range alerts {
		states[alert.Type] = alert.State
	}
	if states["capability_divergence"] != "ACTIVE" || states["all_connections_ineligible"] != "ACTIVE" ||
		states["provider_data_policy_rejected"] != "ACTIVE" {
		t.Fatalf("alert states=%v alerts=%+v", states, alerts)
	}

	f.server.configMu.Lock()
	next, err = cloneConfig(f.server.deps.Config)
	if err == nil {
		next.Connections[1].Models[0].Capabilities = next.Connections[0].Models[0].Capabilities
		err = f.server.publishConfig(next)
	}
	f.server.configMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	f.server.evaluateAlerts(now.Add(6 * time.Minute))
	alerts, err = f.control.ManagementAlerts(20)
	if err != nil {
		t.Fatal(err)
	}
	states = map[string]string{}
	for _, alert := range alerts {
		states[alert.Type] = alert.State
	}
	if states["capability_divergence"] != "RECOVERED" || states["all_connections_ineligible"] != "RECOVERED" ||
		states["provider_data_policy_rejected"] != "RECOVERED" {
		t.Fatalf("recovered alert states=%v alerts=%+v", states, alerts)
	}
}
